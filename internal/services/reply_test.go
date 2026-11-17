package services_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// memberPolicy allows everything except reading non-public messages: a
// conversation member who is not staff.
type memberPolicy struct {
	mu    sync.Mutex
	staff bool
	// hidden is a resource the member may not read whatever its audience.
	hidden model.Ref
	checks []policy.Authorization
}

func (p *memberPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks = append(p.checks, a)
	if a.Action == model.ActionMessageRead && a.Audience.Visibility != model.Public && !p.staff {
		return policy.Decision{}, nil
	}
	if (a.Action == model.ActionMessageRead || a.Action == model.ActionInteractionRead) && a.Resource == p.hidden {
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true}, nil
}

func (p *memberPolicy) reads() []policy.Authorization {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []policy.Authorization
	for _, a := range p.checks {
		if a.Action == model.ActionMessageRead {
			out = append(out, a)
		}
	}
	return out
}

// replyWorld seeds, beside the rig conversation, a public message and an
// internal note in it, a message in a sibling conversation of the same
// inbox, one in another inbox, and one in another tenant.
type replyWorld struct {
	*rig
	policy                           *memberPolicy
	public, note                     model.Message
	sibling, otherInbox, otherTenant model.Message
}

func newReplyWorld(t *testing.T) *replyWorld {
	p := &memberPolicy{}
	r := newRig(t, func(c *engine.Config, _ *outFactory) { c.Authorizer = p })
	w := &replyWorld{rig: r, policy: p}
	msg := func(conv model.ConversationRef, id model.ID, vis model.Visibility) model.Message {
		return model.Message{
			Ref: model.MessageRef{Conversation: conv, ID: id}, Audience: model.Audience{Visibility: vis},
			Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "secret " + string(id)}}},
			RecordedAt: r.Clock.Now(), Revision: "m1",
		}
	}
	id := func(n int) model.ID { return model.ID(fmt.Sprintf("00000003-0000-7000-8000-%012x", n)) }
	siblingConv := model.Conversation{Ref: model.ConversationRef{Inbox: r.Conv.Ref.Inbox, ID: id(1)}, Mode: model.ModePerRoom, Status: model.ConversationOpen, Revision: "cv"}
	inbox2 := model.Inbox{Ref: model.InboxRef{Tenant: tenantA, ID: inboxTwo}, Channel: r.Ch.Ref, Name: "two", Revision: "in"}
	conv2 := model.Conversation{Ref: model.ConversationRef{Inbox: inbox2.Ref, ID: id(2)}, Mode: model.ModePerRoom, Status: model.ConversationOpen, Revision: "cv"}
	w.public = msg(r.Conv.Ref, id(3), model.Public)
	w.note = msg(r.Conv.Ref, id(4), model.Internal)
	w.sibling = msg(siblingConv.Ref, id(5), model.Public)
	w.otherInbox = msg(conv2.Ref, id(6), model.Public)
	if err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error {
		return engine.FirstErr(
			tx.PutInbox(r.Ctx, inbox2, ""),
			tx.PutConversation(r.Ctx, siblingConv, ""),
			tx.PutConversation(r.Ctx, conv2, ""),
			tx.PutMessage(r.Ctx, w.public, ""),
			tx.PutMessage(r.Ctx, w.note, ""),
			tx.PutMessage(r.Ctx, w.sibling, ""),
			tx.PutMessage(r.Ctx, w.otherInbox, ""),
		)
	}); err != nil {
		t.Fatal(err)
	}
	chB := model.Channel{Ref: model.ChannelRef{Tenant: tenantB, ID: chanOne}, Type: outKey, Revision: "c"}
	inB := model.Inbox{Ref: model.InboxRef{Tenant: tenantB, ID: inboxOne}, Channel: chB.Ref, Revision: "i"}
	convB := model.Conversation{Ref: model.ConversationRef{Inbox: inB.Ref, ID: convOne}, Mode: model.ModePerRoom, Status: model.ConversationOpen, Revision: "cv"}
	w.otherTenant = msg(convB.Ref, id(7), model.Public)
	if err := r.E.Store().Transact(r.Ctx, tenantB, func(tx store.Tx) error {
		return engine.FirstErr(tx.PutChannel(r.Ctx, chB, ""), tx.PutInbox(r.Ctx, inB, ""),
			tx.PutConversation(r.Ctx, convB, ""), tx.PutMessage(r.Ctx, w.otherTenant, ""))
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *replyWorld) replyTo(op model.OperationID, parent model.MessageRef) error {
	req := w.ReplyReq(op, "answer")
	req.InReplyTo = &parent
	_, err := w.C.Messages().Reply(w.Ctx, req)
	return err
}

// requireNoReplyQueued fails when any reply operation was queued.
func (w *replyWorld) requireNoReplyQueued(t *testing.T) {
	t.Helper()
	if n := len(w.Claim("w")); n != 0 {
		t.Fatalf("rejected replies queued %d work items", n)
	}
}

// A reply's parent is read through the reply's own conversation: a message
// ID that lives in another conversation, inbox or tenant is not found under
// it, exactly like a missing one, and nothing is queued.
func TestReplyParentOutsideConversationIsNotFound(t *testing.T) {
	w := newReplyWorld(t)
	missing := model.ID("00000003-0000-7000-8000-0000000000ff")
	cases := map[string]model.ID{
		"missing":               missing,
		"sibling conversation":  w.sibling.Ref.ID,
		"another inbox":         w.otherInbox.Ref.ID,
		"another tenant":        w.otherTenant.Ref.ID,
		"internal note, unread": w.note.Ref.ID,
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			err := w.replyTo(model.OperationID("op-"+name[:4]), model.MessageRef{Conversation: w.Conv.Ref, ID: id})
			requireCode(t, err, model.NotFound, "resource not found")
		})
	}
	w.requireNoReplyQueued(t)
}

// A parent reference naming another conversation is rejected from the
// request alone, before any store read or policy call: it reveals nothing
// about stored messages.
func TestReplyParentNamingAnotherConversationIsInvalid(t *testing.T) {
	w := newReplyWorld(t)
	for name, ref := range map[string]model.MessageRef{
		"sibling conversation": w.sibling.Ref,
		"another inbox":        w.otherInbox.Ref,
		"another tenant":       w.otherTenant.Ref,
	} {
		t.Run(name, func(t *testing.T) {
			requireCode(t, w.replyTo("op-x", ref), model.Validation, "in_reply_to")
		})
	}
	if n := len(w.policy.reads()); n != 0 {
		t.Fatalf("policy consulted %d times for request-shape rejections", n)
	}
	w.requireNoReplyQueued(t)
}

// The principal's read of the parent is authorized before its audience is
// examined: a member cannot tell an internal note from a missing message;
// staff learn only that a public reply cannot answer it.
func TestReplyParentReadIsAuthorizedFirst(t *testing.T) {
	w := newReplyWorld(t)
	requireCode(t, w.replyTo("op-member", w.note.Ref), model.NotFound, "resource not found")
	reads := w.policy.reads()
	if len(reads) != 1 || reads[0].Resource != w.note.Ref.Ref() || reads[0].Audience.Visibility != model.Internal ||
		reads[0].Principal != principalA() {
		t.Fatalf("parent read authorizations %+v", reads)
	}
	w.policy.staff = true
	requireCode(t, w.replyTo("op-staff", w.note.Ref), model.Validation, "non-public")
	w.requireNoReplyQueued(t)
	if err := w.replyTo("op-public", w.public.Ref); err != nil {
		t.Fatalf("reply to public parent: %v", err)
	}
	if n := len(w.Claim("w")); n != 1 {
		t.Fatalf("queued %d replies, want 1", n)
	}
}

// idOnlyStore models a backend that resolves messages by ID alone,
// ignoring the conversation and inbox of the reference.
type idOnlyStore struct {
	store.Store
	byID map[model.ID]model.Message
}

func (s idOnlyStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	return s.Store.View(ctx, t, func(r store.Reader) error { return fn(idOnlyReader{r, s.byID}) })
}

type idOnlyReader struct {
	store.Reader
	byID map[model.ID]model.Message
}

func (r idOnlyReader) GetMessage(ctx context.Context, ref model.MessageRef) (model.Message, error) {
	if m, ok := r.byID[ref.ID]; ok {
		return m.Clone(), nil
	}
	return r.Reader.GetMessage(ctx, ref)
}

// The engine itself checks that a loaded parent lies in the reply's
// conversation: a store answering by ID alone still cannot attach a reply
// to another thread or reveal that message.
func TestReplyParentScopeHoldsOverIDOnlyStore(t *testing.T) {
	w := newReplyWorld(t)
	w.Store.Set(idOnlyStore{Store: w.Store.Inner(), byID: map[model.ID]model.Message{
		w.sibling.Ref.ID: w.sibling, w.otherInbox.Ref.ID: w.otherInbox,
	}})
	for _, id := range []model.ID{w.sibling.Ref.ID, w.otherInbox.Ref.ID} {
		err := w.replyTo(model.OperationID("op-"+string(id[len(id)-1:])), model.MessageRef{Conversation: w.Conv.Ref, ID: id})
		requireCode(t, err, model.NotFound, "resource not found")
	}
	if n := len(w.policy.reads()); n != 0 {
		t.Fatalf("policy consulted for out-of-scope parents: %d", n)
	}
	w.requireNoReplyQueued(t)
}

// storedParent returns the parent a queued reply's stored intent names.
func (w *replyWorld) storedParent(t *testing.T, op model.OperationRef) *model.MessageRef {
	t.Helper()
	var rec store.OperationRecord
	if err := w.E.Store().View(w.Ctx, tenantA, func(r store.Reader) (err error) {
		rec, err = r.Operation(w.Ctx, op)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	in, err := operations.DecodeReplyIntent(rec.Command)
	if err != nil {
		t.Fatal(err)
	}
	return in.InReplyTo
}

// A reply without InReplyTo answers the conversation's latest inbound
// message the caller may read: never a uma-authored message, a message the
// caller cannot read, or a non-public one a public reply cannot answer. An
// explicit InReplyTo always wins.
func TestReplyParentDefaultsToLatestReadableInbound(t *testing.T) {
	w := newReplyWorld(t)
	id := func(n int) model.ID { return model.ID(fmt.Sprintf("00000003-0000-7000-8000-%012x", n)) }
	put := func(m model.Message) {
		t.Helper()
		if err := w.E.Store().Transact(w.Ctx, tenantA, func(tx store.Tx) error { return tx.PutMessage(w.Ctx, m, "") }); err != nil {
			t.Fatal(err)
		}
	}
	msg := func(n int, vis model.Visibility, origin *model.Origin) model.Message {
		return model.Message{
			Ref: model.MessageRef{Conversation: w.Conv.Ref, ID: id(n)}, Audience: model.Audience{Visibility: vis},
			Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "m"}}},
			RecordedAt: w.Clock.Now(), Revision: "m1", Origin: origin,
		}
	}
	latest := msg(10, model.Public, nil)
	put(latest)
	put(msg(11, model.Public, &model.Origin{Actor: "agent-1"}))
	put(msg(12, model.Internal, nil))

	a, err := w.C.Messages().Reply(w.Ctx, w.ReplyReq("op-default", "answer"))
	if err != nil {
		t.Fatal(err)
	}
	if got := w.storedParent(t, a.Operation.Ref); got == nil || *got != latest.Ref {
		t.Fatalf("default parent = %v, want the latest public inbound %v", got, latest.Ref)
	}

	if err := w.replyTo("op-explicit", w.public.Ref); err != nil {
		t.Fatal(err)
	}
	ops := w.Claim("w")
	if len(ops) != 2 {
		t.Fatalf("queued %d replies, want 2", len(ops))
	}
	for _, wk := range ops {
		ref, _ := wk.Resource.AsOperation()
		if ref == a.Operation.Ref {
			continue
		}
		if got := w.storedParent(t, ref); got == nil || *got != w.public.Ref {
			t.Fatalf("explicit parent = %v, want %v", got, w.public.Ref)
		}
	}
}

// A message the caller may not read is never chosen as the default parent:
// the reply falls back to an older one it may read, or to none.
func TestReplyParentDefaultSkipsUnreadable(t *testing.T) {
	w := newReplyWorld(t)
	hidden := model.Message{
		Ref: model.MessageRef{Conversation: w.Conv.Ref, ID: "00000003-0000-7000-8000-0000000000aa"}, Audience: model.Audience{Visibility: model.Public, PolicyRef: "vip"},
		RecordedAt: w.Clock.Now(), Revision: "m1",
	}
	if err := w.E.Store().Transact(w.Ctx, tenantA, func(tx store.Tx) error { return tx.PutMessage(w.Ctx, hidden, "") }); err != nil {
		t.Fatal(err)
	}
	w.policy.hidden = hidden.Ref.Ref()
	a, err := w.C.Messages().Reply(w.Ctx, w.ReplyReq("op-default", "answer"))
	if err != nil {
		t.Fatal(err)
	}
	if got := w.storedParent(t, a.Operation.Ref); got == nil || *got != w.public.Ref {
		t.Fatalf("default parent = %v, want the readable %v", got, w.public.Ref)
	}
}

// seedInteraction stores an action interaction of the rig conversation
// under audience, with a response token, and returns it.
func (w *replyWorld) seedInteraction(t *testing.T, id model.ID, audience model.Audience) model.Interaction {
	t.Helper()
	actor := model.Identity{
		Ref:      model.IdentityRef{Inbox: w.Conv.Ref.Inbox, ID: "00000003-0000-7000-8000-0000000000e1"},
		SourceID: "user-1", Namespace: routeNSpace, Kind: model.ContactKindPerson,
	}
	rec := enginetest.InteractionSeed{
		Conv: w.Conv.Ref, ID: id, Actor: actor, Channel: w.Ch, Audience: audience,
		Now: w.Clock.Now(), RespondUntil: w.Clock.Now().Add(time.Hour),
	}.Record()
	if err := w.E.Store().Transact(w.Ctx, tenantA, func(tx store.Tx) error { return tx.PutInteraction(w.Ctx, rec) }); err != nil {
		t.Fatal(err)
	}
	return rec.Interaction
}

func (w *replyWorld) interaction(t *testing.T, ref model.InteractionRef) model.Interaction {
	t.Helper()
	var in model.Interaction
	if err := w.E.Store().View(w.Ctx, tenantA, func(r store.Reader) (err error) {
		in, err = r.Interaction(w.Ctx, ref)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return in
}

// A reply naming an interaction only correlates: the message records the
// interaction and message.created carries it, but the reply never claims
// the initial response, counts a follow-up, bumps the interaction or reads
// its token, whether queued or delivered.
func TestReplyInResponseToIsCorrelationOnly(t *testing.T) {
	w := newReplyWorld(t)
	spy := &enginetest.InteractionSpy{Store: w.Store.Inner()}
	w.Store.Set(spy)
	in := w.seedInteraction(t, "00000003-0000-7000-8000-0000000000aa", model.Audience{Visibility: model.Public})
	req := w.ReplyReq("op-irt", "answer")
	req.InResponseTo = &in.Ref
	a, err := w.C.Messages().Reply(w.Ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	w.Step("w")
	enginetest.RequireState(t, w.Op(a.Operation.Ref), model.ProviderAccepted, "")

	msg := w.Message(*a.Operation.ResultMessage)
	if msg.InResponseTo == nil || *msg.InResponseTo != in.Ref {
		t.Fatalf("message InResponseTo = %v, want %v", msg.InResponseTo, in.Ref)
	}
	var created *model.MessageCreatedPayload
	for _, ev := range w.Events() {
		if ev.Kind != model.EventMessageCreated {
			continue
		}
		created = new(model.MessageCreatedPayload)
		if err := json.Unmarshal(ev.Payload.Data, created); err != nil {
			t.Fatal(err)
		}
	}
	if created == nil || created.InResponseTo == nil || *created.InResponseTo != in.Ref || created.Origin != model.MessageOriginReply {
		t.Fatalf("message.created payload = %+v, want origin reply correlated with %v", created, in.Ref)
	}
	got := w.interaction(t, in.Ref)
	if got.Response.Initial != nil || got.Response.FollowUps != 0 || got.Revision != in.Revision {
		t.Fatalf("interaction after reply = %+v, want untouched", got.Response)
	}
	if spy.Claims() != 0 || spy.TokenReads() != 0 {
		t.Fatalf("reply made %d response claims and %d token reads, want none", spy.Claims(), spy.TokenReads())
	}
}

// The interaction a reply names must exist in the conversation and be
// readable by the caller: a missing one, one in another conversation and
// one the caller may not read are the same not found, and nothing is
// queued.
func TestReplyInResponseToMustBeReadable(t *testing.T) {
	w := newReplyWorld(t)
	hidden := w.seedInteraction(t, "00000003-0000-7000-8000-0000000000ab", model.Audience{Visibility: model.Internal, PolicyRef: "vip"})
	w.policy.hidden = hidden.Ref.Ref()
	reply := func(op model.OperationID, ref model.InteractionRef) error {
		req := w.ReplyReq(op, "answer")
		req.InResponseTo = &ref
		_, err := w.C.Messages().Reply(w.Ctx, req)
		return err
	}
	requireCode(t, reply("op-missing", model.InteractionRef{Conversation: w.Conv.Ref, ID: "00000003-0000-7000-8000-0000000000ff"}), model.NotFound, "resource not found")
	requireCode(t, reply("op-hidden", hidden.Ref), model.NotFound, "resource not found")
	other := model.ConversationRef{Inbox: w.Conv.Ref.Inbox, ID: "00000003-0000-7000-8000-0000000000fe"}
	requireCode(t, reply("op-other", model.InteractionRef{Conversation: other, ID: hidden.Ref.ID}), model.Validation, "in_response_to")
	w.requireNoReplyQueued(t)
}

// Retrying a correlated reply with the same operation ID replays it; the
// same ID naming another interaction is a payload conflict.
func TestReplyInResponseToIsPartOfTheFingerprint(t *testing.T) {
	w := newReplyWorld(t)
	one := w.seedInteraction(t, "00000003-0000-7000-8000-0000000000ac", model.Audience{Visibility: model.Public})
	two := w.seedInteraction(t, "00000003-0000-7000-8000-0000000000ad", model.Audience{Visibility: model.Public})
	reply := func(ref *model.InteractionRef) (model.Acceptance, error) {
		req := w.ReplyReq("op-fp", "answer")
		req.InResponseTo = ref
		return w.C.Messages().Reply(w.Ctx, req)
	}
	first, err := reply(&one.Ref)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reply(&one.Ref)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("retry = %+v, %v; want a replay of %v", again, err, first.Operation.Ref)
	}
	_, err = reply(&two.Ref)
	requireCode(t, err, model.Conflict, "different payload")
	_, err = reply(nil)
	requireCode(t, err, model.Conflict, "different payload")
}
