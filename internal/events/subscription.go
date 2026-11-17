package events

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Subscription identity, ACK tokens and per-event authorization.
//
// A subscription is identified by its principal and consumer name: the
// subscription ID is derived from (tenant, actor, consumer), so one engine
// and one store converge on one record without a lookup index, and the
// filter is pinned at creation. A changed filter needs a new consumer name.
//
// An ACK token is an opaque encoding of the stored batch binding:
// subscription, actor, lease generation, scan range and expiry. Only its
// digest is stored. Acknowledging compares the digest and the lease, so a
// token is spent by one ACK and invalidated by lease expiry or redelivery.
// A token is reconstructible from the stored record, which is how a re-Poll
// before expiry returns the identical outstanding batch.

// subscriptionDomain separates subscription IDs from any other SHA-256 use
// of the same bytes.
const subscriptionDomain = "uma.subscription.id"

// subscriptionID derives the stable subscription ID of (tenant, actor,
// consumer) as an RFC 9562 version 8 UUID.
func subscriptionID(p model.Principal, consumer string) model.ID {
	w := operations.Field().Str(subscriptionDomain).Str(string(p.Tenant)).Str(string(p.Actor)).Str(consumer)
	sum := sha256.Sum256(w.Bytes())
	b := sum[:16]
	b[6] = 0x80 | (b[6] & 0x0f) // version 8
	b[8] = 0x80 | (b[8] & 0x3f) // variant 10xx
	h := hex.EncodeToString(b)
	return model.ID(h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32])
}

// ackToken is the decoded form of an ACK token.
type ackToken struct {
	Subscription model.ID      `json:"s"`
	Actor        model.ActorID `json:"a"`
	Generation   uint64        `json:"g"`
	From         model.Cursor  `json:"f"`
	Through      model.Cursor  `json:"t"`
	ExpiresAt    int64         `json:"e"` // Unix milliseconds
}

func (t ackToken) encode() string {
	b, err := json.Marshal(t)
	if err != nil {
		panic("uma: ack token encoding: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeAckToken(s string) (ackToken, bool) {
	var t ackToken
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return t, false
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return t, false
	}
	return t, true
}

// tokenDigest is the stored form of a token.
func tokenDigest(token string) []byte {
	d := sha256.Sum256([]byte(token))
	return d[:]
}

// batchToken returns the token of the batch bound to rec.
func batchToken(rec store.SubscriptionRecord) string {
	b := rec.PendingBatch
	return ackToken{
		Subscription: rec.Subscription.Ref.ID, Actor: rec.Principal.Actor, Generation: b.Fence.Generation,
		From: b.From, Through: b.Through, ExpiresAt: b.ExpiresAt.UnixMilli(),
	}.encode()
}

// errStaleToken reports a token that does not bind the current outstanding
// batch: spent, expired, replaced by a redelivery, or never issued.
func errStaleToken() error {
	return model.NewError(model.Conflict, "ack token is not the outstanding batch; poll again")
}

// errCursorExpired reports a subscription position behind retention with
// the recovery the caller must take.
func errCursorExpired() error {
	return model.NewError(model.CursorExpired,
		"subscription position is behind event retention; reopen the subscription from "+
			"beginning or tail and reconcile from current resource state")
}

// errSubscriptionClosed reports a closed subscription.
func errSubscriptionClosed() error {
	return &model.Error{Code: model.Conflict, SafeMessage: "subscription closed; reopen it to resume", Retry: model.NoRetry}
}

// readAction returns the policy action that grants reading the resource an
// event is about. ok is false for kinds no read action covers; such events
// are never delivered.
func readAction(k model.ResourceKind) (string, bool) { return model.ReadAction(k) }

// eventGate authorizes events for one principal within one Poll. Decisions
// are memoized per (action, resource, audience) for the call only; nothing
// is cached across calls.
type eventGate struct {
	c    *engine.Client
	memo map[policy.Authorization]bool
}

func newEventGate(c *engine.Client) *eventGate {
	return &eventGate{c: c, memo: map[policy.Authorization]bool{}}
}

// allows reports whether the principal may receive ev: it must read the
// event's inbox, when the event has one, and the resource the event is
// about, at the event's audience. An event of a registered extension kind
// also needs the kind's policy action (Engine.EventAction) on the same
// resource, audience and revision. A denial skips the event; a policy
// failure fails the poll.
func (g *eventGate) allows(ctx context.Context, ev model.Event) (bool, error) {
	if ev.Inbox != "" {
		in := model.InboxRef{Tenant: ev.Tenant, ID: ev.Inbox}.Ref()
		ok, err := g.check(ctx, policy.Authorization{Action: model.ActionInboxRead, Resource: in})
		if err != nil || !ok {
			return false, err
		}
	}
	action, ok := readAction(ev.Resource.Kind)
	if !ok {
		return false, nil
	}
	read := policy.Authorization{Action: action, Resource: ev.Resource, Audience: ev.Audience, Revision: ev.Revision}
	if ok, err := g.check(ctx, read); err != nil || !ok {
		return false, err
	}
	if model.IsHeldEvent(ev.Kind) {
		// A held operation is the draft content of its target: its events
		// reach only a principal that may read the target as well.
		return g.targetReadable(ctx, ev)
	}
	if model.IsWorkflowEvent(ev.Kind) {
		// Workflow events carry workflow state: they reach only a
		// principal that may read it.
		workflow := read
		workflow.Action = model.ActionConversationReadWorkflow
		if ok, err := g.check(ctx, workflow); err != nil || !ok {
			return false, err
		}
		if ev.Kind == model.EventConversationLinked || ev.Kind == model.EventConversationUnlinked {
			return g.farEndReadable(ctx, ev)
		}
	}
	extension, ok := g.c.Engine().EventAction(ev.Kind)
	if !ok {
		return true, nil
	}
	read.Action = extension
	return g.check(ctx, read)
}

// targetReadable reports whether the principal may read the target of the
// held operation an event names. A payload that does not decode, or names a
// target no read action covers, is withheld.
func (g *eventGate) targetReadable(ctx context.Context, ev model.Event) (bool, error) {
	var p model.HeldOperationPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		return false, nil
	}
	action, ok := readAction(p.Key.Target.Kind)
	if !ok {
		return false, nil
	}
	return g.check(ctx, policy.Authorization{Action: action, Resource: p.Key.Target})
}

// farEndReadable reports whether the principal may read the far end a
// link event names: a link event never confirms an end its subscriber
// could not read. A payload that does not decode is withheld.
func (g *eventGate) farEndReadable(ctx context.Context, ev model.Event) (bool, error) {
	var p model.ConversationLinkPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil || p.Link.Validate() != nil {
		return false, nil
	}
	action := model.ActionConversationRead
	if p.Link.WorkItem != nil {
		action = model.ActionWorkItemRead
	}
	return g.check(ctx, policy.Authorization{Action: action, Resource: p.Link.Ref()})
}

func (g *eventGate) check(ctx context.Context, a policy.Authorization) (bool, error) {
	if ok, seen := g.memo[a]; seen {
		return ok, nil
	}
	err := g.c.Authorize(ctx, a, a.Resource)
	switch {
	case err == nil:
		g.memo[a] = true
		return true, nil
	case model.IsCode(err, model.Unauthorized), model.IsCode(err, model.NotFound):
		g.memo[a] = false
		return false, nil
	}
	return false, err
}

// readPage reads the next page of events after the checkpoint through the
// subscription's filter. page.Next is the scanned-through position whether
// or not the page holds anything.
func readPage(ctx context.Context, r store.Reader, rec store.SubscriptionRecord, after model.Cursor, limit int) (model.Page[model.Event], error) {
	page, err := r.Events(ctx, rec.Filter, after, limit)
	if model.IsCode(err, model.CursorExpired) {
		return page, errCursorExpired()
	}
	return page, err
}

// readBatch re-reads the events of the outstanding batch in order. Events
// are immutable and never leave the log except by retention, so the IDs
// bound to the batch identify them.
func readBatch(ctx context.Context, r store.Reader, rec store.SubscriptionRecord) ([]model.Event, error) {
	b := rec.PendingBatch
	want := make(map[model.ID]bool, len(b.EventIDs))
	for _, id := range b.EventIDs {
		want[id] = true
	}
	out := make([]model.Event, 0, len(b.EventIDs))
	after := b.From
	for len(want) > 0 {
		page, err := r.Events(ctx, rec.Filter, after, len(want))
		if model.IsCode(err, model.CursorExpired) {
			return nil, errCursorExpired()
		}
		if err != nil {
			return nil, err
		}
		for _, ev := range page.Items {
			if want[ev.ID] {
				delete(want, ev.ID)
				out = append(out, ev)
			}
		}
		if page.Complete || page.Next == after {
			break
		}
		after = page.Next
	}
	return out, nil
}

// keep returns the events of evs the principal may receive, in order. It
// runs outside any store unit. On redelivery, events the principal may no
// longer read are dropped without exposure; the batch range is unchanged.
func (g *eventGate) keep(ctx context.Context, evs []model.Event) ([]model.Event, error) {
	kept := make([]model.Event, 0, len(evs))
	for _, ev := range evs {
		ok, err := g.allows(ctx, ev)
		if err != nil {
			return nil, err
		}
		if ok {
			kept = append(kept, ev)
		}
	}
	return kept, nil
}

// batchFor returns the public batch of rec's outstanding batch and the
// events delivered for it.
func batchFor(rec store.SubscriptionRecord, events []model.Event) model.EventBatch {
	b := rec.PendingBatch
	out := model.EventBatch{Events: make([]model.Event, 0, len(events)), AckToken: batchToken(rec),
		ExpiresAt: b.ExpiresAt, ScannedThrough: b.Through}
	for _, ev := range events {
		out.Events = append(out.Events, ev.Clone())
	}
	return out
}

// batchLive reports whether rec holds a batch that has not expired at now.
func batchLive(rec store.SubscriptionRecord, now time.Time) bool {
	return rec.PendingBatch != nil && now.Before(rec.PendingBatch.ExpiresAt)
}

// openFingerprint returns the version 1 fingerprint of an Open: consumer,
// filter, start, after and payload major.
func openFingerprint(req model.OpenSubscription) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionSubscriptionOpen))
	f.Set("consumer", operations.Field().Str(req.Consumer))
	f.Set("filter", filterField(req.Filter))
	f.Set("start", operations.Field().Str(req.Start))
	f.Set("after", operations.Field().Str(string(req.After)))
	f.Set("payload_major", operations.Field().U32(req.PayloadMajor))
	return f.Sum()
}

// ackFingerprint returns the version 1 fingerprint of an Ack: target and
// token.
func ackFingerprint(req model.Acknowledge) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionSubscriptionAck))
	f.Set("target", operations.Field().Ref(req.Subscription.Ref()))
	f.Set("token", operations.Field().Str(req.Token))
	return f.Sum()
}

// closeFingerprint returns the version 1 fingerprint of a Close: target.
func closeFingerprint(sub model.SubscriptionRef) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionSubscriptionClose))
	f.Set("target", operations.Field().Ref(sub.Ref()))
	return f.Sum()
}

// filterField encodes a filter's fields in a fixed order; list order is
// meaningful.
func filterField(f model.Filter) *operations.Writer {
	w := operations.Field()
	w.U32(uint32(len(f.Inboxes))) // #nosec G115 -- a filter lists a handful of inboxes
	for _, in := range f.Inboxes {
		w.Ref(in.Ref())
	}
	w.Present(f.Conversation != nil)
	if f.Conversation != nil {
		w.Ref(f.Conversation.Ref())
	}
	w.Present(f.Contact != nil)
	if f.Contact != nil {
		w.Ref(f.Contact.Ref())
	}
	w.U32(uint32(len(f.Kinds))) // #nosec G115 -- a filter lists a handful of kinds
	for _, k := range f.Kinds {
		w.Str(k)
	}
	w.U32(uint32(len(f.Statuses))) // #nosec G115 -- a filter lists a handful of statuses
	for _, st := range f.Statuses {
		w.Str(st)
	}
	return w
}

// sameFilter reports whether two filters select the same events.
func sameFilter(a, b model.Filter) bool {
	return slices.Equal(a.Inboxes, b.Inboxes) && slices.Equal(a.Kinds, b.Kinds) &&
		slices.Equal(a.Statuses, b.Statuses) && ptrEqual(a.Conversation, b.Conversation) &&
		ptrEqual(a.Contact, b.Contact)
}

func ptrEqual[T comparable](a, b *T) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return *a == *b
}
