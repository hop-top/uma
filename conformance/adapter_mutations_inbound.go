package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
)

// Mutation cases that need the provider to speak: reaction summaries, the
// provider's echo of a mutation, edits of private messages and the scope
// of mutation targets.

// reactionTotals maps each emoji of a message's count-only entries to its
// count, failing on an attributed entry or a repeated emoji.
func reactionTotals(t *testing.T, m model.Message) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, r := range m.Reactions {
		if r.Attributed() {
			t.Fatalf("reaction %+v names a reactor, but a summary names none", r)
		}
		if _, dup := out[r.Emoji]; dup {
			t.Fatalf("reactions %+v hold %s twice", m.Reactions, r.Emoji)
		}
		out[r.Emoji] = r.Count
	}
	return out
}

var mutationInboundCases = []adapterCase{
	{name: "ReactionSummaryKeepsCountsOnly", needs: []model.Capability{model.CapabilityReact}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		g := e.inboundTarget(t, "")
		identities := len(e.identities(t, adapterInboxA))
		first := e.update(g, InboundReactionSummary, 1)
		first.Counts, first.Complete = []model.ReactionCount{{Emoji: reactEmoji, Count: 2}, {Emoji: "🎉", Count: 1}}, false
		wantAcked(t, e.receiveAll(t, adapterChanA, first))
		got := e.storedMessage(t, g.msg.Ref)
		if totals := reactionTotals(t, got); totals[reactEmoji] != 2 || totals["🎉"] != 1 || len(totals) != 2 {
			t.Fatalf("reactions = %+v, want the provider's counts", got.Reactions)
		}
		if got.ReactionsComplete {
			t.Fatal("reactions marked complete by a snapshot the provider did not vouch for")
		}

		// A later snapshot replaces every count: an emoji it lacks is gone.
		second := e.update(g, InboundReactionSummary, 2)
		second.Counts = []model.ReactionCount{{Emoji: reactEmoji, Count: 3}}
		wantAcked(t, e.receiveAll(t, adapterChanA, second))
		got = e.storedMessage(t, g.msg.Ref)
		if totals := reactionTotals(t, got); totals[reactEmoji] != 3 || len(totals) != 1 {
			t.Fatalf("reactions = %+v, want only %s counted 3", got.Reactions, reactEmoji)
		}
		if n := len(e.identities(t, adapterInboxA)); n != identities {
			t.Fatalf("identities = %d after summaries, want %d: no reactor is named", n, identities)
		}
	}},
	{name: "EditKeepsAudience", needs: []model.Capability{model.CapabilityEdit}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		g := e.inboundTarget(t, model.Internal)
		if g.msg.Audience.Visibility != model.Internal {
			t.Fatalf("private provider message stored with audience %+v", g.msg.Audience)
		}
		ref := e.mustMutate(t, kindEdit, g.msg, "op-edit")
		e.later(t, 0)
		wantActApplied(t, e.op(t, ref))
		if got := e.storedMessage(t, g.msg.Ref); got.Audience.Visibility != model.Internal {
			t.Fatalf("message audience %+v after an edit, want internal", got.Audience)
		}
		calls := e.calls(adapterChanA, model.CapabilityEdit)
		if len(calls) != 1 || calls[0].Audience.Visibility != model.Internal {
			t.Fatalf("provider edits = %+v, want one carrying the internal audience", calls)
		}
		changes := e.changes(t, ref)
		if len(changes) == 0 {
			t.Fatal("no message.updated event for the edit")
		}
		for _, c := range changes {
			if c.event.Audience.Visibility != model.Internal {
				t.Fatalf("message.updated event audience %+v, want internal only", c.event.Audience)
			}
		}
		e.stop(t)
	}},
	{name: "MutationEchoSettlesUnknown", needs: []model.Capability{model.CapabilityEdit}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		g := e.inboundTarget(t, "")
		e.respond(t, adapterChanA, Response{Outcome: OutcomeAnswerLost})
		ref := e.mustMutate(t, kindEdit, g.msg, "op-echo")
		e.later(t, 0)
		wantOp(t, e.op(t, ref), model.Unknown, model.OutcomeUnknown)
		calls := e.calls(adapterChanA, model.CapabilityEdit)
		if len(calls) != 1 {
			t.Fatalf("provider edits = %+v, want one", calls)
		}

		echo := e.update(g, InboundEdited, 1)
		echo.Text, echo.Echo = editText, &InboundEcho{Operation: ref, Attempt: calls[0].Attempt}
		// A provider that cannot echo an edit skips here, before the call is
		// judged for naming its attempt.
		before := len(e.provider.Inbound(adapterChanA))
		e.deliver(t, adapterChanA, echo)
		if calls[0].Attempt == "" {
			t.Fatalf("provider edit %+v does not name its attempt", calls[0])
		}
		wantAcked(t, e.pump(t, adapterChanA, before, echo))
		// The echo is evidence for the operation, not an edit of its own.
		e.wantUnchanged(t, g.msg)

		e.later(t, time.Second)
		wantActApplied(t, e.op(t, ref))
		got := e.storedMessage(t, g.msg.Ref)
		wantApplied(t, kindEdit, got, true)
		if le := got.LastEdit; le == nil || le.Operation == nil || *le.Operation != ref || le.Actor != "conformance-agent" {
			t.Fatalf("last edit = %+v, want the edit applied under operation %v", le, ref)
		}
		if n := len(e.calls(adapterChanA, model.CapabilityEdit)); n != 1 {
			t.Fatalf("provider edits = %d, want 1: the echo settles the edit without a resend", n)
		}
		e.wantChanges(t, ref, model.MessageChangeEdited)
		e.stop(t)
	}},
}

var mutationBypassCases = []adapterCase{
	{name: "MutationOnForeignMessageNotFound", anyOf: mutationCaps, run: func(t *testing.T, e *adapterEnv) {
		g := e.anyTarget(t)
		c := g.msg.Ref.Conversation
		foreign := []struct {
			why string
			ref model.MessageRef
		}{
			{"another tenant", model.MessageRef{Conversation: model.ConversationRef{Inbox: model.InboxRef{Tenant: "conformance-other", ID: c.Inbox.ID}, ID: c.ID}, ID: g.msg.Ref.ID}},
			{"another inbox", model.MessageRef{Conversation: model.ConversationRef{Inbox: adapterInboxB, ID: c.ID}, ID: g.msg.Ref.ID}},
			{"no such message", model.MessageRef{Conversation: c, ID: "00000ad0-0000-7000-8000-0000000000ff"}},
		}
		before := len(e.provider.Calls(adapterChanA))
		for _, k := range e.kinds(true) {
			for i, f := range foreign {
				m := g.msg
				m.Ref = f.ref
				id := model.OperationID("op-foreign-" + k.name + "-" + string(rune('a'+i)))
				if _, err := e.mutate(k, m, id); model.CodeOf(err) != model.NotFound {
					t.Errorf("%s of a message in %s = %v, want not_found", k.name, f.why, err)
				}
			}
		}
		e.later(t, time.Hour)
		if calls := e.provider.Calls(adapterChanA); len(calls) != before || len(e.provider.Calls(adapterChanB)) != 0 {
			t.Fatalf("provider calls after refused mutations = %+v, want none", calls[before:])
		}
		e.wantUnchanged(t, g.msg)
		e.stop(t)
	}},
}
