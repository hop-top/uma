package conformance

import (
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/model"
)

// reaction is an inbound reaction of kind by the fault sender on provider
// message msg; emoji may be empty for a removal naming none.
func (f *faultEnv) reaction(event, msg, kind, emoji string) model.Ingress {
	in := f.message(event, "")
	in.Envelope.MessageID, in.Envelope.Kind, in.Envelope.Content = msg, kind, model.Content{}
	if emoji != "" {
		in.Envelope.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: emoji}}}
	}
	return in
}

// reactionChanges returns the reaction changes announced for m in order,
// each as "<change> <emoji>".
func (f *faultEnv) reactionChanges(t *testing.T, m model.Message) []string {
	t.Helper()
	var out []string
	for _, ev := range f.allEvents(t, model.Filter{}, 50) {
		if ev.Kind != model.EventMessageUpdated || ev.Resource != m.Ref.Ref() {
			continue
		}
		var pl model.MessageUpdatedPayload
		if err := json.Unmarshal(ev.Payload.Data, &pl); err != nil {
			t.Fatal(err)
		}
		if pl.Reaction != nil {
			out = append(out, pl.Change+" "+pl.Reaction.Emoji)
		}
	}
	return out
}

// The adapter declares single-reaction cardinality, as WhatsApp does. A
// sender's second emoji replaces the first, each a stored change with its
// own message.updated event; a removal naming no emoji then removes the
// one reaction rather than failing as ambiguous. Another sender's
// reaction is left alone, and the result survives a restart.
func inboundSingleReactionReplaces(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	f.p.singleReactions = true
	p0 := f.start(t)
	_, stop := p0.receive()
	push := func(in model.Ingress) {
		t.Helper()
		f.push(t, in)
		if r := f.accept(t); r.err != nil || !r.receipt.Committed || r.receipt.Duplicate {
			t.Fatalf("ingress of %s = %+v, %v; want it committed", in.Envelope.EventID, r.receipt, r.err)
		}
	}
	emojis := func() []string {
		var out []string
		for _, r := range f.messages(t)[0].Reactions {
			out = append(out, r.Emoji)
		}
		return out
	}
	push(f.message("evt-1", "m-1"))
	push(f.reaction("evt-2", "m-1", model.EnvelopeReactionAdded, "👍"))
	push(f.reaction("evt-3", "m-1", model.EnvelopeReactionAdded, "🎉"))
	if got := emojis(); !slices.Equal(got, []string{"🎉"}) {
		t.Fatalf("reactions = %v after a second emoji, want 🎉 alone", got)
	}
	stop()

	p := f.restart(t, cross)
	_, stop = p.receive()
	push(f.reaction("evt-4", "m-1", model.EnvelopeReactionRemoved, ""))
	if got := emojis(); len(got) != 0 {
		t.Fatalf("reactions = %v after a removal naming no emoji, want none", got)
	}
	stop()
	want := []string{"reaction_added 👍", "reaction_removed 👍", "reaction_added 🎉", "reaction_removed 🎉"}
	if got := f.reactionChanges(t, f.messages(t)[0]); !slices.Equal(got, want) {
		t.Fatalf("reaction events = %v, want %v", got, want)
	}
}
