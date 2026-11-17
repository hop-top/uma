// Package events is the Events service: subscriptions over the committed
// event log, polled and acknowledged by the client that opened them.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Events delivers committed domain events to consumers at least once.
//
// Open creates or reopens the principal's subscription under a consumer
// name at an explicit start: the retained beginning, the current tail, or a
// supplied checkpoint. Poll returns one ordered batch of committed events
// the principal may read, bound to a lease on the subscription and an ACK
// token; it never advances the checkpoint. Ack spends the token: it moves
// the checkpoint past the batch's scan range in the same unit as its
// operation claim and releases the lease. A re-Poll before the lease ends
// returns the same outstanding batch; after it ends the batch is redelivered
// under a new lease and the old token is refused. Consumers deduplicate by
// Event.ID.
//
// The lease belongs to the subscription, and a subscription to one actor:
// pollers that share an actor (several processes of one bot) share one
// lease. A second Poll while a batch is outstanding gets that same batch and
// token, never a different range; the first Ack wins and every later Ack of
// the token is model.Conflict, so the loser knows another poller handled
// the batch. Consumers that must process independently use distinct
// consumer names, each its own subscription.

// Poll limits: a zero Poll.Limit asks for defaultPollLimit events; larger
// requests are capped at maxPollLimit.
const (
	defaultPollLimit = 100
	maxPollLimit     = 1000
)

// ackLease is how long a batch stays acknowledgeable after Poll returns it.
const ackLease = time.Minute

// pollStep bounds one wait of a long poll between scans. A commit of this
// engine that appends events ends the wait early; the step bounds how late
// events committed by other processes are seen.
const pollStep = time.Second

// maxUnitAttempts bounds how often Poll reruns a batch issue that lost a
// race with a concurrent unit on the same subscription.
const maxUnitAttempts = 3

// Stored intents of subscription commands.
const (
	intentSubscriptionOpen  = "uma.intent.subscription.open"
	intentSubscriptionAck   = "uma.intent.subscription.ack"
	intentSubscriptionClose = "uma.intent.subscription.close"
)

// Service is the Events service of one client.
type Service struct{ c *engine.Client }

// New returns the Events service bound to c.
func New(c *engine.Client) Service { return Service{c} }

// Open opens the principal's subscription named req.Consumer, or returns it
// when it exists with the same filter. Start and After apply when the
// subscription is created or reopened after Close; an open subscription
// keeps its position. The same consumer name with a different filter is
// model.Conflict: a changed filter is a new subscription. A payload major
// other than model.PayloadVersion1 is model.SchemaMismatch. A checkpoint
// behind retention is model.CursorExpired.
func (s Service) Open(ctx context.Context, req model.OpenSubscription) (model.SubscriptionResult, error) {
	if s.c.Engine().IsClosed() {
		return model.SubscriptionResult{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.SubscriptionResult{}, err
	}
	if err := s.checkFilter(req.Filter); err != nil {
		return model.SubscriptionResult{}, err
	}
	if req.PayloadMajor != model.PayloadVersion1 {
		return model.SubscriptionResult{}, &model.Error{Code: model.SchemaMismatch,
			SafeMessage: "payload major not emitted by this version; declare payload_major 1", Retry: model.NoRetry}
	}
	sub := model.SubscriptionRef{Tenant: s.c.Principal().Tenant, ID: subscriptionID(s.c.Principal(), req.Consumer)}
	err := s.c.Authorize(ctx, policy.Authorization{Action: model.ActionSubscriptionOpen, Resource: sub.Ref()}, sub.Ref())
	if err != nil {
		return model.SubscriptionResult{}, err
	}
	intent, err := json.Marshal(req)
	if err != nil {
		return model.SubscriptionResult{}, model.NewError(model.Validation, "subscription request cannot be stored")
	}
	cmd := subscriptionCommand{
		c: s.c, kind: model.ActionSubscriptionOpen, sub: sub, command: req.Command, fingerprint: openFingerprint(req),
		intent: model.Extension{Name: intentSubscriptionOpen, Version: 1, Data: intent},
	}
	return cmd.run(ctx, func(ctx context.Context, tx store.Tx) (model.Subscription, error) {
		rec, err := tx.Subscription(ctx, sub)
		switch {
		case model.IsCode(err, model.NotFound):
			return s.create(ctx, tx, sub, req)
		case err != nil:
			return model.Subscription{}, err
		case rec.Principal != s.c.Principal():
			return model.Subscription{}, engine.ErrNotFound()
		case !sameFilter(rec.Filter, req.Filter):
			return model.Subscription{}, &model.Error{Code: model.Conflict, Retry: model.NoRetry,
				SafeMessage: "filter differs from the existing subscription; a changed filter needs a new consumer name"}
		case rec.ClosedAt == nil:
			return rec.Subscription, nil
		}
		return s.reopen(ctx, tx, rec, req)
	})
}

// checkFilter rejects filter fields subscriptions do not support and event
// kinds that are neither builtin nor namespaced, and hides inboxes outside
// the principal's tenant.
func (s Service) checkFilter(f model.Filter) error {
	switch {
	case f.Conversation != nil:
		return model.NewError(model.Validation, "filter.conversation: not supported by subscriptions")
	case f.Contact != nil:
		return model.NewError(model.Validation, "filter.contact: not supported by subscriptions")
	case len(f.Statuses) > 0:
		return model.NewError(model.Validation, "filter.statuses: not supported by subscriptions")
	case f.Identity != nil:
		return model.NewError(model.Validation, "filter.identity: not supported by subscriptions")
	case f.Labels != nil:
		return model.NewError(model.Validation, "filter.labels: not supported by subscriptions")
	case f.Task != nil:
		return model.NewError(model.Validation, "filter.task: not supported by subscriptions")
	case f.Text != "":
		return model.NewError(model.Validation, "filter.text: not supported by subscriptions")
	case f.Since != nil:
		return model.NewError(model.Validation, "filter.since: not supported by subscriptions")
	case f.Before != nil:
		return model.NewError(model.Validation, "filter.before: not supported by subscriptions")
	case f.Attachments != nil:
		return model.NewError(model.Validation, "filter.attachments: not supported by subscriptions")
	case f.Assignee != nil:
		return model.NewError(model.Validation, "filter.assignee: not supported by subscriptions")
	case f.Snoozed != nil:
		return model.NewError(model.Validation, "filter.snoozed: not supported by subscriptions")
	case f.SnoozeEnded != nil:
		return model.NewError(model.Validation, "filter.snooze_ended: not supported by subscriptions")
	case f.Linked != nil:
		return model.NewError(model.Validation, "filter.linked: not supported by subscriptions")
	case f.Unread != nil:
		return model.NewError(model.Validation, "filter.unread: not supported by subscriptions")
	case f.Actor != nil:
		return model.NewError(model.Validation, "filter.actor: not supported by subscriptions")
	}
	return s.checkFilterRefs(f)
}

// checkFilterRefs hides inboxes and a conversation outside the principal's
// tenant and rejects event kinds that are neither builtin nor namespaced.
func (s Service) checkFilterRefs(f model.Filter) error {
	tenant := s.c.Principal().Tenant
	for _, in := range f.Inboxes {
		if in.Tenant != tenant {
			return engine.ErrNotFound()
		}
	}
	if f.Conversation != nil && f.Conversation.Inbox.Tenant != tenant {
		return engine.ErrNotFound()
	}
	for i, k := range f.Kinds {
		if !s.c.Engine().EventKind(k) {
			return model.NewError(model.Validation, "filter.kinds["+strconv.Itoa(i)+"]: unknown event kind")
		}
	}
	return nil
}

// startPosition resolves the subscription's first checkpoint for req.Start
// within the unit. The beginning is the retained one, an explicit position,
// so a later purge past it expires the checkpoint rather than skipping
// history silently. A supplied checkpoint is read once so a malformed or
// expired cursor fails here, not at the first Poll.
func (s Service) startPosition(ctx context.Context, tx store.Tx, f model.Filter, req model.OpenSubscription) (model.Cursor, error) {
	switch req.Start {
	case model.StartBeginning:
		return tx.EventBase(ctx)
	case model.StartTail:
		return tx.EventHead(ctx)
	}
	_, err := tx.Events(ctx, f, req.After, 1)
	if model.IsCode(err, model.CursorExpired) {
		return "", errCursorExpired()
	}
	if err != nil {
		return "", err
	}
	return req.After, nil
}

// create stores a new subscription. A concurrent creation of the same
// subscription is transient: the rerun returns the record it finds.
func (s Service) create(ctx context.Context, tx store.Tx, sub model.SubscriptionRef, req model.OpenSubscription) (model.Subscription, error) {
	at, err := s.startPosition(ctx, tx, req.Filter, req)
	if err != nil {
		return model.Subscription{}, err
	}
	rev, err := operations.NewRevision(ctx, s.c.Engine())
	if err != nil {
		return model.Subscription{}, err
	}
	rec := store.SubscriptionRecord{
		Subscription: model.Subscription{Ref: sub, Checkpoint: at, Revision: rev},
		Principal:    s.c.Principal(), Consumer: req.Consumer, Filter: req.Filter.Clone(), PayloadMajor: req.PayloadMajor,
	}
	if err := tx.PutSubscription(ctx, rec, ""); err != nil {
		if model.IsCode(err, model.Conflict) {
			return model.Subscription{}, model.NewError(model.Transient, "subscription created concurrently")
		}
		return model.Subscription{}, err
	}
	return rec.Subscription, nil
}

// reopen revives a closed subscription at the requested start.
func (s Service) reopen(ctx context.Context, tx store.Tx, rec store.SubscriptionRecord, req model.OpenSubscription) (model.Subscription, error) {
	at, err := s.startPosition(ctx, tx, rec.Filter, req)
	if err != nil {
		return model.Subscription{}, err
	}
	rev, err := operations.NewRevision(ctx, s.c.Engine())
	if err != nil {
		return model.Subscription{}, err
	}
	expected := rec.Subscription.Revision
	rec = rec.Clone()
	rec.Subscription.Checkpoint, rec.Subscription.Revision = at, rev
	rec.PendingBatch, rec.ClosedAt = nil, nil
	if err := tx.PutSubscription(ctx, rec, expected); err != nil {
		return model.Subscription{}, err
	}
	return rec.Subscription, nil
}

// Poll returns the next batch of committed events after the subscription's
// checkpoint that the principal may read, up to req.Limit, bound to a lease
// of ackLease and an ACK token. An outstanding unexpired batch is returned
// again unchanged; an expired one is redelivered under a new lease. Events
// the principal may not read are skipped without exposure but stay within
// the scanned range, so an empty batch may still carry a token. With no
// events to scan, Poll waits up to req.Wait for new ones; at the deadline
// it returns an empty batch with no token. The wait rescans when a unit of
// this engine that appended events commits, and at least once per pollStep
// for events other processes commit. A checkpoint behind retention is
// model.CursorExpired: reopen the subscription from beginning or tail.
func (s Service) Poll(ctx context.Context, req model.Poll) (model.EventBatch, error) {
	if s.c.Engine().IsClosed() {
		return model.EventBatch{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.EventBatch{}, err
	}
	rec, err := s.load(ctx, req.Subscription, model.ActionSubscriptionPoll)
	if err != nil {
		return model.EventBatch{}, err
	}
	limit := req.Limit
	if limit == 0 {
		limit = defaultPollLimit
	}
	limit = min(limit, maxPollLimit)
	deadline := s.c.Engine().Clock().Now().Add(req.Wait)
	waiter := s.c.Engine().Waiter()
	if waiter == nil {
		waiter = timerWaiter{}
	}
	gate := newEventGate(s.c)
	for {
		// Taken before the scan, so a commit after it ends the wait.
		commit := s.c.Engine().EventCommits(rec.Subscription.Ref.Tenant)
		batch, found, err := s.attempt(ctx, gate, rec.Subscription.Ref, limit)
		if err != nil || found {
			return batch, err
		}
		remaining := deadline.Sub(s.c.Engine().Clock().Now())
		if remaining <= 0 {
			return batch, nil
		}
		if err := waitOrCommit(ctx, waiter, min(remaining, pollStep), commit); err != nil {
			return model.EventBatch{}, err
		}
	}
}

// waitOrCommit waits d through w or until commit closes, whichever ends
// first. The waiter sees a context cancelled by the commit; only a
// cancellation of ctx itself is an error.
func waitOrCommit(ctx context.Context, w ports.Waiter, d time.Duration, commit <-chan struct{}) error {
	select {
	case <-commit:
		return nil
	default:
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	woken := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-commit:
			close(woken)
			cancel()
		case <-stop:
		}
	}()
	err := w.Wait(wctx, d)
	if err == nil || ctx.Err() != nil {
		return err
	}
	select {
	case <-woken:
		return nil
	default:
		return err
	}
}

var errTransientUnit = model.NewError(model.Transient, "unit retried")

// timerWaiter is the default Waiter: a stdlib timer that honours
// cancellation.
type timerWaiter struct{}

func (timerWaiter) Wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// attempt runs issue, rerunning it when a concurrent unit changed the
// subscription meanwhile.
func (s Service) attempt(ctx context.Context, gate *eventGate, sub model.SubscriptionRef, limit int) (model.EventBatch, bool, error) {
	var batch model.EventBatch
	var found bool
	var err error
	for range maxUnitAttempts {
		batch, found, err = s.issue(ctx, gate, sub, limit)
		if !model.IsCode(err, model.Transient) || ctx.Err() != nil {
			break
		}
	}
	return batch, found, err
}

// issue makes one attempt at a batch: it returns the outstanding batch when
// one is live, otherwise scans past the checkpoint and, when anything was
// scanned, binds the result to a new lease. found is false when nothing was
// scanned; the batch then carries the checkpoint and no token. Store units
// only read and write; authorization runs between them.
func (s Service) issue(ctx context.Context, gate *eventGate, sub model.SubscriptionRef, limit int) (model.EventBatch, bool, error) {
	var rec store.SubscriptionRecord
	var page model.Page[model.Event]
	var read []model.Event
	live := false
	err := s.c.Engine().Store().View(ctx, sub.Tenant, func(r store.Reader) error {
		var err error
		if rec, err = s.current(ctx, r, sub); err != nil {
			return err
		}
		if live = batchLive(rec, s.c.Engine().Clock().Now()); live {
			read, err = readBatch(ctx, r, rec)
			return err
		}
		page, err = readPage(ctx, r, rec, rec.Subscription.Checkpoint, limit)
		read = page.Items
		return err
	})
	if err != nil {
		return model.EventBatch{}, false, err
	}
	if !live && len(page.Items) == 0 && page.Next == rec.Subscription.Checkpoint {
		return model.EventBatch{ScannedThrough: rec.Subscription.Checkpoint}, false, nil
	}
	kept, err := gate.keep(ctx, read)
	if err != nil {
		return model.EventBatch{}, false, err
	}
	if live {
		return batchFor(rec, kept), true, nil
	}
	rev, err := operations.NewRevision(ctx, s.c.Engine())
	if err != nil {
		return model.EventBatch{}, false, err
	}
	done, err := s.c.Engine().Begin()
	if err != nil {
		return model.EventBatch{}, false, err
	}
	defer done()
	err = s.c.Engine().Store().Transact(ctx, sub.Tenant, func(tx store.Tx) error {
		cur, err := s.current(ctx, tx, sub)
		if err != nil {
			return err
		}
		now := s.c.Engine().Clock().Now()
		if cur.Subscription.Revision != rec.Subscription.Revision || batchLive(cur, now) {
			return errTransientUnit
		}
		lease, err := tx.AcquireLease(ctx, store.LeaseRequest{
			Resource: sub.Ref(), Owner: string(s.c.Principal().Actor), Until: now.Add(ackLease),
		})
		if model.IsCode(err, model.Conflict) {
			return errTransientUnit
		}
		if err != nil {
			return err
		}
		rec = cur.Clone()
		rec.Subscription.Revision = rev
		rec.PendingBatch = &store.BatchRecord{
			From: cur.Subscription.Checkpoint, Through: page.Next, EventIDs: eventIDsOf(kept),
			ExpiresAt: lease.Until, Fence: lease.Fence,
		}
		rec.PendingBatch.TokenDigest = tokenDigest(batchToken(rec))
		return tx.PutSubscription(ctx, rec, cur.Subscription.Revision)
	})
	if err != nil {
		return model.EventBatch{}, false, err
	}
	return batchFor(rec, kept), true, nil
}

func eventIDsOf(evs []model.Event) []model.ID {
	ids := make([]model.ID, len(evs))
	for i, ev := range evs {
		ids[i] = ev.ID
	}
	return ids
}

// Ack spends the batch token: the checkpoint moves to the batch's
// scanned-through position in the same unit as the operation claim, and the
// lease is released. A token that is not the outstanding batch's, or whose
// lease has expired, is model.Conflict; the events are redelivered by the
// next Poll. Replaying the same operation ID returns the stored result.
func (s Service) Ack(ctx context.Context, req model.Acknowledge) (model.SubscriptionResult, error) {
	if s.c.Engine().IsClosed() {
		return model.SubscriptionResult{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.SubscriptionResult{}, err
	}
	if _, err := s.load(ctx, req.Subscription, model.ActionSubscriptionAck); err != nil {
		return model.SubscriptionResult{}, err
	}
	sub := req.Subscription
	digest := tokenDigest(req.Token)
	intent, err := json.Marshal(struct {
		Subscription model.SubscriptionRef `json:"subscription"`
		TokenDigest  []byte                `json:"token_digest"`
	}{sub, digest})
	if err != nil {
		return model.SubscriptionResult{}, model.NewError(model.Validation, "acknowledgement cannot be stored")
	}
	cmd := subscriptionCommand{
		c: s.c, kind: model.ActionSubscriptionAck, sub: sub, command: req.Command, fingerprint: ackFingerprint(req),
		intent: model.Extension{Name: intentSubscriptionAck, Version: 1, Data: intent},
	}
	return cmd.run(ctx, func(ctx context.Context, tx store.Tx) (model.Subscription, error) {
		cur, err := s.current(ctx, tx, sub)
		if err != nil {
			return model.Subscription{}, err
		}
		tok, ok := decodeAckToken(req.Token)
		b := cur.PendingBatch
		if !ok || b == nil || tok.Subscription != sub.ID || tok.Actor != s.c.Principal().Actor ||
			!bytes.Equal(b.TokenDigest, digest) {
			return model.Subscription{}, errStaleToken()
		}
		if !s.c.Engine().Clock().Now().Before(b.ExpiresAt) {
			return model.Subscription{}, model.NewError(model.Conflict, "batch lease expired; poll again for redelivery")
		}
		rev, err := operations.NewRevision(ctx, s.c.Engine())
		if err != nil {
			return model.Subscription{}, err
		}
		err = tx.AdvanceCheckpoint(ctx, store.CheckpointAdvance{
			Subscription: sub, Prior: cur.Subscription.Checkpoint, Next: b.Through, Fence: b.Fence, Revision: rev,
		})
		if err != nil {
			return model.Subscription{}, err
		}
		if err := tx.ReleaseLease(ctx, store.Lease{Resource: sub.Ref(), Fence: b.Fence, Until: b.ExpiresAt}); err != nil {
			return model.Subscription{}, err
		}
		return model.Subscription{Ref: sub, Checkpoint: b.Through, Revision: rev}, nil
	})
}

// Close stops delivery on the subscription and releases its lease. History
// and the subscription's position are kept; Open with the same consumer
// name reopens it at the requested start. Closing a closed subscription is
// model.Conflict.
func (s Service) Close(ctx context.Context, req model.Lookup[model.SubscriptionRef], cmd model.Command) (model.Acceptance, error) {
	if s.c.Engine().IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if err := cmd.Validate(model.RevisionAbsent); err != nil {
		return model.Acceptance{}, err
	}
	if _, err := s.load(ctx, req.Ref, model.ActionSubscriptionClose); err != nil {
		return model.Acceptance{}, err
	}
	sub := req.Ref
	intent, err := json.Marshal(struct {
		Subscription model.SubscriptionRef `json:"subscription"`
	}{sub})
	if err != nil {
		return model.Acceptance{}, model.NewError(model.Validation, "close cannot be stored")
	}
	c := subscriptionCommand{
		c: s.c, kind: model.ActionSubscriptionClose, sub: sub, command: cmd, fingerprint: closeFingerprint(sub),
		intent: model.Extension{Name: intentSubscriptionClose, Version: 1, Data: intent},
	}
	r, err := c.run(ctx, func(ctx context.Context, tx store.Tx) (model.Subscription, error) {
		cur, err := s.current(ctx, tx, sub)
		if err != nil {
			return model.Subscription{}, err
		}
		if b := cur.PendingBatch; b != nil {
			err := tx.ReleaseLease(ctx, store.Lease{Resource: sub.Ref(), Fence: b.Fence, Until: b.ExpiresAt})
			if err != nil && !model.IsCode(err, model.Conflict) {
				return model.Subscription{}, err
			}
		}
		rev, err := operations.NewRevision(ctx, s.c.Engine())
		if err != nil {
			return model.Subscription{}, err
		}
		now := s.c.Engine().Clock().Now()
		expected := cur.Subscription.Revision
		cur = cur.Clone()
		cur.Subscription.Revision, cur.PendingBatch, cur.ClosedAt = rev, nil, &now
		if err := tx.PutSubscription(ctx, cur, expected); err != nil {
			return model.Subscription{}, err
		}
		return cur.Subscription, nil
	})
	return r.Acceptance, err
}

// load reads the subscription for a request on it and authorizes action.
// A missing subscription, or one of another principal, is not found.
func (s Service) load(ctx context.Context, sub model.SubscriptionRef, action string) (store.SubscriptionRecord, error) {
	if sub.Tenant != s.c.Principal().Tenant {
		return store.SubscriptionRecord{}, engine.ErrNotFound()
	}
	var rec store.SubscriptionRecord
	err := s.c.Engine().Store().View(ctx, sub.Tenant, func(r store.Reader) error {
		var err error
		rec, err = s.current(ctx, r, sub)
		return err
	})
	if err != nil {
		return store.SubscriptionRecord{}, err
	}
	err = s.c.Authorize(ctx, policy.Authorization{Action: action, Resource: sub.Ref(), Revision: rec.Subscription.Revision}, sub.Ref())
	if err != nil {
		return store.SubscriptionRecord{}, err
	}
	if rec.ClosedAt != nil {
		return store.SubscriptionRecord{}, errSubscriptionClosed()
	}
	return rec, nil
}

// current reads the subscription within a unit, hiding foreign ones.
func (s Service) current(ctx context.Context, r store.Reader, sub model.SubscriptionRef) (store.SubscriptionRecord, error) {
	rec, err := r.Subscription(ctx, sub)
	if model.IsCode(err, model.NotFound) || (err == nil && rec.Principal != s.c.Principal()) {
		return store.SubscriptionRecord{}, engine.ErrNotFound()
	}
	if err != nil {
		return store.SubscriptionRecord{}, err
	}
	if rec.ClosedAt != nil {
		return store.SubscriptionRecord{}, errSubscriptionClosed()
	}
	return rec, nil
}

// subscriptionCommand is a local command on a subscription: it is claimed
// in the operation ledger and applied in the same unit, with no provider
// work, so it commits directly. The same operation ID with the same payload
// replays the stored result; a different payload is model.Conflict.
type subscriptionCommand struct {
	c           *engine.Client
	kind        string
	sub         model.SubscriptionRef
	command     model.Command
	fingerprint []byte
	intent      model.Extension
}

// run claims the operation and applies fn once; a replay returns the
// subscription's current state with the stored operation.
func (cmd subscriptionCommand) run(ctx context.Context, fn func(context.Context, store.Tx) (model.Subscription, error)) (model.SubscriptionResult, error) {
	if err := operations.RefuseHold(cmd.command); err != nil {
		return model.SubscriptionResult{}, err
	}
	done, err := cmd.c.Engine().Begin()
	if err != nil {
		return model.SubscriptionResult{}, err
	}
	defer done()
	opID, err := operations.NewID(ctx, cmd.c.Engine())
	if err != nil {
		return model.SubscriptionResult{}, err
	}
	rev, err := operations.NewRevision(ctx, cmd.c.Engine())
	if err != nil {
		return model.SubscriptionResult{}, err
	}
	tenant := cmd.c.Principal().Tenant
	rec := store.OperationRecord{
		Operation: model.Operation{Ref: model.OperationRef{Tenant: tenant, ID: opID}, State: model.Committed, Revision: rev},
		Key: store.OperationKey{
			Tenant: tenant, Caller: cmd.c.Principal().Actor, Kind: cmd.kind, Target: cmd.sub.Ref(), ID: cmd.command.Operation,
		},
		FingerprintVersion: operations.FingerprintVersion1,
		Fingerprint:        cmd.fingerprint,
		Command:            cmd.intent,
	}
	var got store.OperationRecord
	var created bool
	var subscription model.Subscription
	err = operations.Transact(ctx, cmd.c.Engine(), tenant, func(tx store.Tx) error {
		var err error
		got, created, err = tx.ClaimOperation(ctx, rec)
		if err != nil {
			return err
		}
		if !created {
			stored, err := tx.Subscription(ctx, cmd.sub)
			if err != nil {
				return err
			}
			subscription = stored.Subscription
			return nil
		}
		subscription, err = fn(ctx, tx)
		return err
	})
	if err != nil {
		if model.IsCode(err, model.Conflict) && !created {
			return model.SubscriptionResult{}, model.NewError(model.Conflict, "operation ID already used with a different payload")
		}
		return model.SubscriptionResult{}, err
	}
	return model.SubscriptionResult{
		Subscription: subscription,
		Acceptance: model.Acceptance{
			Operation: operations.Shown(got), Durable: cmd.c.Engine().Store().Properties().Durable, Replayed: !created,
		},
	}, nil
}
