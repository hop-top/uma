package ingress_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// newMutationDesk is a desk whose adapter edits, deletes, reacts and
// refetches.
func newMutationDesk(t *testing.T) *desk {
	t.Helper()
	d := newDeskWith(t, &enginetest.OutFactory{Mutations: true, Refetches: true})
	d.MustAccept(d.comment("e-1", "c-1", "customer", "7"))
	return d
}

func (d *desk) target() model.Message {
	d.T.Helper()
	return d.MessageByProviderID(d.inbox.Ref, "c-1")
}

func (d *desk) edit(opID model.OperationID, m model.Message, body string) model.Acceptance {
	d.T.Helper()
	a, err := d.Client.Messages().Edit(d.Ctx, model.EditMessage{
		Command: model.Command{Call: model.Call{Correlation: "corr"}, Operation: opID, ExpectedRevision: m.Revision},
		Message: m.Ref, Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: body}}},
	})
	if err != nil {
		d.T.Fatalf("Edit: %v", err)
	}
	return a
}

// echoOf is an inbound update of kind marked as the echo of the latest
// provider call of operation op.
func (d *desk) echoOf(op model.OperationRef, env model.Envelope) model.Envelope {
	d.T.Helper()
	calls := d.f.MutationCalls()
	if len(calls) == 0 {
		d.T.Fatal("no mutation reached the provider")
	}
	env.References = append(env.References, model.EchoRef(d.Channel.Ref, op, calls[len(calls)-1].Effect.Attempt))
	return env
}

func (d *desk) edited(event string, at time.Time, body string) model.Envelope {
	env := editedBy(d.harness, event, "c-1", "customer", at, body)
	env.References = []model.ExternalRef{d.issue("7")}
	return env
}

// The provider's marked echo of uma's own edit is evidence, not a second
// change: the lost answer is settled from it by the edit's worker, which
// applies the content once under the caller; ingress itself writes
// nothing and emits nothing.
func TestEchoOfOutboundEditSettlesWithoutSecondUpdate(t *testing.T) {
	d := newMutationDesk(t)
	m := d.target()
	d.f.Respond(enginetest.LostAnswer)
	a := d.edit("op-edit", m, "corrected")
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.Unknown {
		t.Fatalf("edit = %s, want outcome_unknown", op.State)
	}

	d.MustAccept(d.echoOf(a.Operation.Ref, d.edited("e-echo", t0.Add(time.Minute), "corrected")))
	if got := d.target(); got.Revision != m.Revision || text(got) != text(m) || got.LastEdit != nil {
		t.Fatalf("ingress applied the echo: %+v", got)
	}
	if n := len(updatesOf(t, d.harness, m)); n != 0 {
		t.Fatalf("ingress emitted %d message.updated for the echo", n)
	}
	if n := len(identitiesOf(d.harness, d.inbox)); n != 1 {
		t.Fatalf("identities = %d: the echo claimed a sender", n)
	}

	d.deliver()
	op := d.op(a.Operation.Ref)
	if op.State != model.ProviderAccepted || op.Failure != nil {
		t.Fatalf("edit = %s (%v), want provider_accepted from the echo", op.State, op.Failure)
	}
	got := d.target()
	if text(got) != "corrected" || got.LastEdit == nil || got.LastEdit.Operation == nil || *got.LastEdit.Operation != a.Operation.Ref {
		t.Fatalf("message = %+v, want the edit applied under its operation", got)
	}
	us := updatesOf(t, d.harness, got)
	if len(us) != 1 || us[0].Change != model.MessageChangeEdited || us[0].Trigger != model.TriggerCommand {
		t.Fatalf("message.updated = %v, want one command edit", changesOf(us))
	}
	if n := len(d.f.MutationCalls()); n != 1 {
		t.Fatalf("provider edits = %d, want 1", n)
	}
	// Redelivered under another event ID: still evidence, still nothing.
	d.MustAccept(d.echoOf(a.Operation.Ref, d.edited("e-echo2", t0.Add(2*time.Minute), "corrected")))
	if n := len(updatesOf(t, d.harness, got)); n != 1 {
		t.Fatalf("message.updated = %d after a repeated echo", n)
	}
}

// Marked echoes of a delete and a reaction settle their operations the
// same way; a reaction echo never joins the target's provider ID to the
// reaction.
func TestEchoOfOutboundDeleteAndReactionSettle(t *testing.T) {
	d := newMutationDesk(t)
	m := d.target()
	d.f.Respond(enginetest.LostAnswer, enginetest.LostAnswer)
	react, err := d.Client.Messages().React(d.Ctx, model.Reaction{
		Command: model.Command{Call: model.Call{Correlation: "corr"}, Operation: "op-react"}, Message: m.Ref, Emoji: "👍",
	})
	if err != nil {
		t.Fatal(err)
	}
	d.deliver()
	env := reacted(d.harness, "e-r", model.EnvelopeReactionAdded, "c-1", "uma-bot", "👍")
	d.MustAccept(d.echoOf(react.Operation.Ref, env))
	d.deliver()
	got := d.target()
	if op := d.op(react.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("react = %s, want provider_accepted", op.State)
	}
	if len(got.Reactions) != 1 || got.Reactions[0].By != nil || got.Reactions[0].Actor == "" ||
		slices.Contains(got.Reactions[0].External, model.ProviderMessageRef(d.Channel.Ref, "c-1")) ||
		!slices.Equal(got.External, m.External) {
		t.Fatalf("message = %+v, want the caller's reaction without the target's references", got)
	}
	if n := len(updatesOf(t, d.harness, got)); n != 1 {
		t.Fatalf("message.updated = %d, want the settle's alone", n)
	}

	del, err := d.Client.Messages().Delete(d.Ctx, model.DeleteMessage{
		Command: model.Command{Call: model.Call{Correlation: "corr"}, Operation: "op-del", ExpectedRevision: got.Revision}, Message: m.Ref,
	})
	if err != nil {
		t.Fatal(err)
	}
	d.deliver()
	gone := d.Inbound("e-d", "c-1", "uma-bot", dest)
	gone.Kind, gone.Content = model.EnvelopeMessageDeleted, model.Content{}
	d.MustAccept(d.echoOf(del.Operation.Ref, gone))
	if g := d.target(); g.Deleted {
		t.Fatalf("ingress applied the delete echo")
	}
	d.deliver()
	if g := d.target(); !g.Deleted || g.Redacted || len(g.Content.Parts) != 0 {
		t.Fatalf("message = %+v, want the settle's tombstone", g)
	}
}

// An echo naming a send, not a mutation, of this inbox is no echo of an
// update: the inbound edit applies as any other.
func TestEchoRefOfSendDoesNotShadowAnEdit(t *testing.T) {
	d := newMutationDesk(t)
	d.sentAs("c-2")
	a := d.reply(d.onlyConversation().Ref, "op-1", "on it")
	d.deliver()
	sends := d.f.ProviderSends()
	env := d.edited("e-x", t0.Add(time.Minute), "changed")
	env.References = append(env.References, model.EchoRef(d.Channel.Ref, a.Operation.Ref, sends[0].Attempt))
	d.MustAccept(env)
	if got := d.target(); text(got) != "changed" {
		t.Fatalf("content %q, want the edit applied", text(got))
	}
}

// A newer inbound edit that lands while uma's edit is in flight wins:
// the edit's later completion records the provider's answer and leaves
// the inbound content, with edit_superseded naming the operation.
func TestInboundEditDuringOutboundEditWins(t *testing.T) {
	d := newMutationDesk(t)
	m := d.target()
	d.f.Respond(func(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error, bool) {
		d.MustAccept(d.edited("e-in", t0.Add(time.Minute), "from the provider side"))
		return enginetest.Accepted(ctx, r)
	})
	a := d.edit("op-edit", m, "from uma")
	d.deliver()
	op := d.op(a.Operation.Ref)
	got := d.target()
	if op.State != model.ProviderAccepted || text(got) != "from the provider side" || got.LastEdit == nil || got.LastEdit.Operation != nil {
		t.Fatalf("edit = %s, message = %+v, want the inbound edit kept", op.State, got)
	}
	us := updatesOf(t, d.harness, got)
	if want := []string{model.MessageChangeEdited, model.MessageChangeEditSuperseded}; !slices.Equal(changesOf(us), want) {
		t.Fatalf("message.updated = %v, want %v", changesOf(us), want)
	}
}

// An edit that ties with the applied version over other content cannot
// be ordered: ingress keeps what it holds and refetches.
func TestTiedEditStillRefetches(t *testing.T) {
	d := newMutationDesk(t)
	first := d.edited("e-2", t0.Add(time.Minute), "first")
	d.MustAccept(first)
	held := d.target()
	tie := d.edited("e-3", t0.Add(time.Minute), "second")
	tie.SourceRevision = first.SourceRevision
	d.MustAccept(tie)
	if got := d.target(); got.Revision != held.Revision || text(got) != "first" {
		t.Fatalf("a tied edit changed the message: %+v", got)
	}
	d.deliver()
	if !slices.Contains(d.f.Calls(), "refetch") {
		t.Fatalf("calls = %v, want a refetch", d.f.Calls())
	}
}

// The provider copy a refetch reads applies as authoritative even when
// it is older than what uma holds, and records its editor: the provider
// showing the older edit after a superseded settle wins reconciliation.
func TestRefetchedCopyReconcilesSupersededEdit(t *testing.T) {
	d := newMutationDesk(t)
	d.MustAccept(d.edited("e-2", t0.Add(2*time.Minute), "newer local"))
	held := d.target()
	older := d.edited("e-3", t0.Add(time.Minute), "provider copy")
	older.Sender = d.SenderRef("editor")
	in := model.Ingress{Call: model.Call{Correlation: "refetch"}, Envelope: older}
	if _, err := d.Sink.AcceptRefetched(d.Ctx, model.OperationRef{Tenant: tenantA, ID: d.ID()}, in); err != nil {
		t.Fatal(err)
	}
	got := d.target()
	editor, _ := identityOf(d.harness, d.inbox.Ref, "editor")
	if text(got) != "provider copy" || got.Revision == held.Revision || got.LastEdit == nil || got.LastEdit.By == nil ||
		*got.LastEdit.By != editor.Ref || !got.LastEdit.At.Equal(t0.Add(time.Minute)) {
		t.Fatalf("message = %+v, want the provider copy with its editor", got)
	}
}

func identitiesOf(h *harness, in model.Inbox) []model.Identity { return h.Identities(in.Ref) }
