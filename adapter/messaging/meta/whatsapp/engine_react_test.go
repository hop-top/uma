package whatsapp_test

import (
	"fmt"
	"testing"

	"hop.top/uma/model"
)

func reactionMessage(id, from, target, emoji string) string {
	rx := fmt.Sprintf(`{"message_id":%q}`, target)
	if emoji != "" {
		rx = fmt.Sprintf(`{"message_id":%q,"emoji":%q}`, target, emoji)
	}
	return fmt.Sprintf(`"contacts":[{"profile":{"name":"Sheena Nelson"},"wa_id":%q}],"messages":[{"from":%q,"id":%q,"timestamp":"%d","type":"reaction","reaction":%s}]`,
		from, from, id, testNow.Unix(), rx)
}

func (r *engineRig) message(providerID string) model.Message {
	r.t.Helper()
	for _, m := range r.messages() {
		for _, x := range m.External {
			if x.Namespace == model.ProviderMessageNamespace && x.ID == providerID {
				return m
			}
		}
	}
	r.t.Fatalf("no message holds provider ID %s", providerID)
	return model.Message{}
}

// A removal that names no emoji, which is all WhatsApp reports, removes the
// sender's one reaction on the message.
func TestInboundReactionRemovalWithoutEmojiThroughEngine(t *testing.T) {
	r := newEngineRig(t)
	r.notify(textMessage("wamid.in-1", testUser, "hello", testNow.Unix()))
	emojis := func() []string {
		var out []string
		for _, x := range r.message("wamid.in-1").Reactions {
			out = append(out, x.Emoji)
		}
		return out
	}
	r.notify(reactionMessage("wamid.rx-1", testUser, "wamid.in-1", "👍"))
	if got := emojis(); len(got) != 1 || got[0] != "👍" {
		t.Fatalf("reactions %v, want 👍", got)
	}
	r.notify(reactionMessage("wamid.rx-2", testUser, "wamid.in-1", ""))
	if got := emojis(); len(got) != 0 {
		t.Fatalf("reactions %v after the removal, want none", got)
	}
	// Removing what is not there changes nothing.
	r.notify(reactionMessage("wamid.rx-3", testUser, "wamid.in-1", ""))
	if got := emojis(); len(got) != 0 {
		t.Fatalf("reactions %v, want none", got)
	}
	r.notify(reactionMessage("wamid.rx-4", testUser, "wamid.in-1", "❤️"))
	r.notify(reactionMessage("wamid.rx-5", testUser, "wamid.in-1", ""))
	if got := emojis(); len(got) != 0 {
		t.Fatalf("reactions %v after the second removal, want none", got)
	}
}

// A user holds one reaction per message: a second emoji replaces the
// first, so the removal that names no emoji is never ambiguous, and
// another user's reaction is left alone.
func TestInboundSecondReactionReplacesFirstThroughEngine(t *testing.T) {
	r := newEngineRig(t)
	r.notify(textMessage("wamid.in-1", testUser, "hello", testNow.Unix()))
	emojis := func() []string {
		var out []string
		for _, x := range r.message("wamid.in-1").Reactions {
			out = append(out, x.Emoji)
		}
		return out
	}
	r.notify(reactionMessage("wamid.rx-1", testUser, "wamid.in-1", "👍"))
	r.notify(reactionMessage("wamid.rx-2", testUser, "wamid.in-1", "❤️"))
	if got := emojis(); len(got) != 1 || got[0] != "❤️" {
		t.Fatalf("reactions %v, want ❤️ alone", got)
	}
	r.notify(reactionMessage("wamid.rx-3", testUser, "wamid.in-1", ""))
	if got := emojis(); len(got) != 0 {
		t.Fatalf("reactions %v after the removal, want none", got)
	}
}

// The business reacts to a message of the user and to one of its own, and
// removes the reaction: reaction messages go out, the stored reaction keeps
// the reaction message's ID, and the message keeps its references.
func TestReactThroughEngine(t *testing.T) {
	r := newEngineRig(t)
	r.notify(textMessage("wamid.in-1", testUser, "hello", testNow.Unix()))
	conv := r.conversation().Ref
	sent := r.reply(conv, "op-reply", textContent("Happy to help."))
	r.work()
	reply := r.op(sent)
	if reply.State != model.ProviderAccepted || reply.ResultMessage == nil {
		t.Fatalf("reply %+v", reply)
	}
	react := func(op model.OperationID, m model.Message, emoji string, remove bool) model.Operation {
		t.Helper()
		a, err := r.client.Messages().React(t.Context(), model.Reaction{
			Command: model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + op)}, Operation: op},
			Message: m.Ref, Emoji: emoji, Remove: remove,
		})
		if err != nil {
			t.Fatal(err)
		}
		r.work()
		return r.op(a.Operation.Ref)
	}

	inbound := r.message("wamid.in-1")
	if op := react("op-rx-1", inbound, "👍", false); op.State != model.ProviderAccepted {
		t.Fatalf("react on the user's message: %+v", op)
	}
	got := r.message("wamid.in-1")
	if len(got.Reactions) != 1 || got.Reactions[0].Emoji != "👍" || got.Reactions[0].Actor != "agent" || len(got.Reactions[0].External) != 1 {
		t.Fatalf("reactions %+v, want the agent's 👍 with the reaction message's ID", got.Reactions)
	}
	if len(got.External) != len(inbound.External) {
		t.Fatalf("message references %+v, want %+v: the reaction's reference stays on its entry", got.External, inbound.External)
	}
	reqs := r.f.recordedRequests()
	rx := reqs[len(reqs)-1].Body["reaction"].(map[string]any)
	if reqs[len(reqs)-1].Body["type"] != "reaction" || rx["message_id"] != "wamid.in-1" || rx["emoji"] != "👍" || reqs[len(reqs)-1].Body["to"] != testUser {
		t.Fatalf("request %v", reqs[len(reqs)-1].Body)
	}

	// One reaction per message: a second emoji replaces the first.
	if op := react("op-rx-1b", r.message("wamid.in-1"), "❤️", false); op.State != model.ProviderAccepted {
		t.Fatalf("replacing react: %+v", op)
	}
	if got := r.message("wamid.in-1").Reactions; len(got) != 1 || got[0].Emoji != "❤️" {
		t.Fatalf("reactions %+v, want ❤️ alone", got)
	}

	own := r.storedMessage(*reply.ResultMessage)
	if op := react("op-rx-2", own, "🎉", false); op.State != model.ProviderAccepted {
		t.Fatalf("react on the business's message: %+v", op)
	}

	if op := react("op-rx-3", r.message("wamid.in-1"), "👍", true); op.State != model.ProviderAccepted {
		t.Fatalf("removal: %+v", op)
	}
	reqs = r.f.recordedRequests()
	rx = reqs[len(reqs)-1].Body["reaction"].(map[string]any)
	if emoji, present := rx["emoji"]; !present || emoji != "" {
		t.Fatalf("removal request %v, want an empty emoji", reqs[len(reqs)-1].Body)
	}
	if got := r.message("wamid.in-1"); len(got.Reactions) != 0 {
		t.Fatalf("reactions %+v after the removal, want none", got.Reactions)
	}
}

func (r *engineRig) storedMessage(ref model.MessageRef) model.Message {
	r.t.Helper()
	m, err := r.client.Messages().Get(r.t.Context(), model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "get-msg"}, Ref: ref})
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}
