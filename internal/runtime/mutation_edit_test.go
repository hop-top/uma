package runtime_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// updated is one message.updated event an operation triggered: its
// payload, its raw keys and its envelope.
type updated struct {
	model.MessageUpdatedPayload
	keys  map[string]json.RawMessage
	event model.Event
}

// updates returns the message.updated events naming op.
func updates(t *testing.T, r *rig, op model.OperationRef) []updated {
	t.Helper()
	var out []updated
	for _, ev := range r.Events() {
		if ev.Kind != model.EventMessageUpdated {
			continue
		}
		u := updated{event: ev}
		if err := json.Unmarshal(ev.Payload.Data, &u.MessageUpdatedPayload); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(ev.Payload.Data, &u.keys); err != nil {
			t.Fatal(err)
		}
		if u.Operation != nil && *u.Operation == op {
			out = append(out, u)
		}
	}
	return out
}

// requireNoContent fails when a message.updated payload carries content.
func requireNoContent(t *testing.T, u updated) {
	t.Helper()
	for _, k := range []string{"content", "parts", "text"} {
		if _, ok := u.keys[k]; ok {
			t.Fatalf("message.updated payload carries %q: %s", k, u.event.Payload.Data)
		}
	}
}

func text(m model.Message) string {
	if len(m.Content.Parts) == 0 {
		return ""
	}
	return m.Content.Parts[0].Text
}

// rejectedAnswer is a final provider refusal of the account's change.
func rejectedAnswer(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
	return adapter.Outcome{State: model.Rejected, Failure: &model.Error{Code: model.Unauthorized, SafeMessage: "not the author", Retry: model.NoRetry}}, nil, false
}

// during runs fn while the provider holds the call, then answers with
// answer.
func during(fn func(), answer enginetest.SendFunc) enginetest.SendFunc {
	return func(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error, bool) {
		fn()
		return answer(ctx, req)
	}
}

// An edit changes the message only once the provider applied it: a
// rejected edit leaves content and revision, an accepted one replaces the
// content, records the caller as the editor and emits message.updated
// edited naming the operation, without content.
func TestEditIsAppliedOnlyWhenProviderApplied(t *testing.T) {
	r := newRig(t, enginetest.WithMutations)
	m := r.SeedMessage("original")
	r.F.Respond(rejectedAnswer)
	refused := r.Edit("op-refused", m, "refused")
	r.Step("w")
	op := r.Op(refused.Operation.Ref)
	requireState(t, op, model.Rejected, model.Unauthorized)
	if op.Pending != nil {
		t.Fatalf("settled edit still pending: %+v", op.Pending)
	}
	if got := r.Message(m.Ref); got.Revision != m.Revision || text(got) != "original" || got.LastEdit != nil {
		t.Fatalf("message after a rejected edit = %+v, want it untouched", got)
	}
	if u := updates(t, r, refused.Operation.Ref); len(u) != 0 {
		t.Fatalf("rejected edit emitted %d message.updated events", len(u))
	}

	a := r.Edit("op-edit", m, "corrected")
	r.Step("w")
	op = r.Op(a.Operation.Ref)
	requireState(t, op, model.ProviderAccepted, "")
	got := r.Message(m.Ref)
	if text(got) != "corrected" || got.Revision == m.Revision {
		t.Fatalf("message after an accepted edit = %+v", got)
	}
	if le := got.LastEdit; le == nil || le.Actor != enginetest.Actor || le.By != nil || le.Operation == nil ||
		*le.Operation != a.Operation.Ref || !le.At.Equal(r.Clock.Now().UTC()) {
		t.Fatalf("last edit = %+v, want the caller under the operation", got.LastEdit)
	}
	if !slices.Equal(got.External, m.External) {
		t.Fatalf("external = %v, want the target's references only", got.External)
	}
	u := updates(t, r, a.Operation.Ref)
	if len(u) != 1 || u[0].Change != model.MessageChangeEdited || u[0].Trigger != model.TriggerCommand ||
		u[0].PreviousRevision != m.Revision || u[0].event.Revision != got.Revision || u[0].event.Actor != enginetest.Actor ||
		u[0].event.Audience != m.Audience {
		t.Fatalf("message.updated = %+v, want one edited event", u)
	}
	requireNoContent(t, u[0])
}

// The provider answers an edit after a newer inbound edit was applied:
// the older completion never clobbers it. The operation settles with the
// provider's answer, and one edit_superseded event names it; a refetch is
// queued when the adapter declares refetch, under the mutation's caller.
// A rejected answer in the same race emits no edit_superseded.
func TestOlderEditCompletionDoesNotClobberNewerInboundEdit(t *testing.T) {
	inbound := func(r *rig, m model.Message) func() {
		return func() {
			r.Mutate(m.Ref, func(x *model.Message) {
				x.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "X2"}}}
				x.LastEdit = &model.Edit{Actor: "sink", By: &model.IdentityRef{Inbox: r.Conv.Ref.Inbox, ID: "00000006-0000-7000-8000-000000000001"}, At: r.Clock.Now().UTC()}
			})
		}
	}
	for _, refetch := range []bool{false, true} {
		t.Run(map[bool]string{false: "no refetch", true: "refetch"}[refetch], func(t *testing.T) {
			opts := []enginetest.RigOption{enginetest.WithMutations}
			if refetch {
				opts = append(opts, enginetest.WithRefetch)
			}
			r := newRig(t, opts...)
			m := r.SeedMessage("original")
			a := r.Edit("op-edit", m, "X1")
			r.F.Respond(during(inbound(r, m), enginetest.Accepted))
			r.Step("w")
			op := r.Op(a.Operation.Ref)
			requireState(t, op, model.ProviderAccepted, "")
			if op.Pending != nil {
				t.Fatalf("pending = %+v", op.Pending)
			}
			got := r.Message(m.Ref)
			if text(got) != "X2" || got.LastEdit == nil || got.LastEdit.Operation != nil {
				t.Fatalf("message = %+v, want the newer inbound edit kept", got)
			}
			u := updates(t, r, a.Operation.Ref)
			if len(u) != 1 || u[0].Change != model.MessageChangeEditSuperseded || u[0].Trigger != model.TriggerCommand ||
				u[0].PreviousRevision != got.Revision || u[0].event.Revision != got.Revision {
				t.Fatalf("message.updated = %+v, want one edit_superseded at the current revision", u)
			}
			requireNoContent(t, u[0])
			if ev := r.Evidence(a.Operation.Ref); len(ev) != 2 || ev[1].State != model.ProviderAccepted {
				t.Fatalf("evidence = %+v, want the provider's answer kept", ev)
			}
			refetches := refetchRecords(t, r, a.Operation.Ref, m)
			if !refetch {
				if len(refetches) != 0 {
					t.Fatalf("refetch recorded without the refetch capability: %+v", refetches)
				}
				return
			}
			if len(refetches) != 1 || refetches[0].Key.Caller != enginetest.Actor || refetches[0].Key.Kind != operations.KindRefetch ||
				refetches[0].Key.Target != m.Ref.Ref() || refetches[0].Operation.State != model.Queued {
				t.Fatalf("refetches = %+v, want one queued under the edit's caller", refetches)
			}
			in, err := operations.DecodeRefetchIntent(refetches[0].Command)
			if err != nil || in.Target != model.ProviderMessageRef(r.Ch.Ref, enginetest.Target) || in.Channel != r.Ch.Ref {
				t.Fatalf("refetch intent = %+v, %v", in, err)
			}
			if works := r.Claim("w"); len(works) != 1 || works[0].Resource != refetches[0].Operation.Ref.Ref() {
				t.Fatalf("work = %+v, want the refetch queued", works)
			}
		})
	}
	t.Run("rejected", func(t *testing.T) {
		r := newRig(t, enginetest.WithMutations, enginetest.WithRefetch)
		m := r.SeedMessage("original")
		a := r.Edit("op-edit", m, "X1")
		r.F.Respond(during(inbound(r, m), rejectedAnswer))
		r.Step("w")
		requireState(t, r.Op(a.Operation.Ref), model.Rejected, model.Unauthorized)
		if u := updates(t, r, a.Operation.Ref); len(u) != 0 {
			t.Fatalf("rejected edit emitted %+v", u)
		}
		if n := len(refetchRecords(t, r, a.Operation.Ref, m)); n != 0 {
			t.Fatalf("rejected edit recorded %d refetches", n)
		}
	})
}

// A denied refetch fails closed: queued under the edit's caller, it needs
// channel.receive, and settles failed without one.
func TestSupersededRefetchFailsClosedWithoutReceive(t *testing.T) {
	r := newRig(t, enginetest.WithMutations, enginetest.WithRefetch)
	m := r.SeedMessage("original")
	a := r.Edit("op-edit", m, "X1")
	r.F.Respond(during(func() {
		r.Mutate(m.Ref, func(x *model.Message) {
			x.Content.Parts[0].Text = "X2"
			x.LastEdit = &model.Edit{Actor: "sink", At: r.Clock.Now().UTC()}
		})
	}, enginetest.Accepted))
	r.Step("w")
	r.Policy.Set(true)
	r.Step("w")
	refetches := refetchRecords(t, r, a.Operation.Ref, m)
	if len(refetches) != 1 || refetches[0].Operation.State != model.Failed || refetches[0].Operation.Failure == nil ||
		refetches[0].Operation.Failure.Code != model.Unauthorized {
		t.Fatalf("refetches = %+v, want one failed unauthorized", refetches)
	}
	if got := r.Policy.Actions(); got[len(got)-1] != model.ActionChannelReceive {
		t.Fatalf("policy checks = %v, want the refetch denied channel.receive", got)
	}
	if slices.Contains(r.F.Calls(), "refetch") {
		t.Fatalf("calls = %v, want no refetch reaching the provider", r.F.Calls())
	}
}

// A refetch is queued on the descriptor, which declares refetch, and gated
// on the channel's availability when it runs: refetch disabled for the
// channel settles it failed without reaching the provider, and a
// temporary outage leaves it queued for its next claim.
func TestQueuedRefetchFollowsChannelAvailability(t *testing.T) {
	for _, tc := range []struct {
		reason model.ErrorCode
		want   model.DeliveryState
	}{
		{model.Disabled, model.Failed},
		{model.Unavailable, model.Queued},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			r := newRig(t, enginetest.WithMutations, enginetest.WithRefetch)
			r.F.Withdraw(model.CapabilityRefetch, tc.reason)
			m := r.SeedMessage("original")
			a := r.Edit("op-edit", m, "X1")
			r.F.Respond(during(func() {
				r.Mutate(m.Ref, func(x *model.Message) {
					x.Content.Parts[0].Text = "X2"
					x.LastEdit = &model.Edit{Actor: "sink", At: r.Clock.Now().UTC()}
				})
			}, enginetest.Accepted))
			r.Step("w")
			refetches := refetchRecords(t, r, a.Operation.Ref, m)
			if len(refetches) != 1 {
				t.Fatalf("refetches = %+v, want one queued on the descriptor", refetches)
			}
			r.Step("w")
			refetches = refetchRecords(t, r, a.Operation.Ref, m)
			if got := refetches[0].Operation.State; got != tc.want {
				t.Fatalf("refetch state = %q (failure %v), want %q", got, refetches[0].Operation.Failure, tc.want)
			}
			if slices.Contains(r.F.Calls(), "refetch") {
				t.Fatalf("calls = %v, want no refetch reaching the provider", r.F.Calls())
			}
		})
	}
}

// refetchRecords returns the refetch operation the settle of edit op on
// m recorded, keyed by op under the edit's caller, if any.
func refetchRecords(t *testing.T, r *rig, op model.OperationRef, m model.Message) []store.OperationRecord {
	t.Helper()
	var out []store.OperationRecord
	err := r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) error {
		rec, err := rd.OperationByKey(r.Ctx, store.OperationKey{
			Tenant: tenantA, Caller: enginetest.Actor, Kind: operations.KindRefetch, Target: m.Ref.Ref(),
			ID: model.OperationID(operations.RefetchKeyPrefix + string(op.ID)),
		})
		if model.IsCode(err, model.NotFound) {
			return nil
		}
		out = append(out, rec)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A reaction recorded while the edit is in flight bumps the revision but
// not the edit state: the edit still applies.
func TestReactionDuringFlightDoesNotTripGuard(t *testing.T) {
	r := newRig(t, enginetest.WithMutations)
	m := r.SeedMessage("original")
	a := r.Edit("op-edit", m, "corrected")
	r.F.Respond(during(func() {
		r.Mutate(m.Ref, func(x *model.Message) {
			x.AddReaction(model.MessageReaction{Emoji: "👍", By: &model.IdentityRef{Inbox: r.Conv.Ref.Inbox, ID: "00000006-0000-7000-8000-000000000001"}, Count: 1})
		})
	}, enginetest.Accepted))
	r.Step("w")
	got := r.Message(m.Ref)
	if text(got) != "corrected" || len(got.Reactions) != 1 {
		t.Fatalf("message = %+v, want the edit applied and the reaction kept", got)
	}
	if u := updates(t, r, a.Operation.Ref); len(u) != 1 || u[0].Change != model.MessageChangeEdited {
		t.Fatalf("message.updated = %+v, want edited", u)
	}
}

// An unmarked echo of the edit beat its answer: the content already
// equals the edit's, so settling writes nothing and emits nothing.
func TestEditEchoedBeforeAnswerWritesNothing(t *testing.T) {
	r := newRig(t, enginetest.WithMutations)
	m := r.SeedMessage("original")
	a := r.Edit("op-edit", m, "corrected")
	var echoed model.Message
	r.F.Respond(during(func() {
		echoed = r.Mutate(m.Ref, func(x *model.Message) {
			x.Content.Parts[0].Text = "corrected"
			x.LastEdit = &model.Edit{Actor: "sink", At: r.Clock.Now().UTC()}
		})
	}, enginetest.Accepted))
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if got := r.Message(m.Ref); got.Revision != echoed.Revision || got.LastEdit.Operation != nil {
		t.Fatalf("message = %+v, want the echoed copy untouched", got)
	}
	if u := updates(t, r, a.Operation.Ref); len(u) != 0 {
		t.Fatalf("message.updated = %+v, want none", u)
	}
}
