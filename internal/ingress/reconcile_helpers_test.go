package ingress_test

import (
	"encoding/json"
	"testing"
	"time"

	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
)

// updated is a decoded message.updated event.
type updated struct {
	ingress.MessageUpdatedPayload
	keys  map[string]json.RawMessage
	event model.Event
}

// updatesOf returns the message.updated events of m, in order.
func updatesOf(t *testing.T, h *harness, m model.Message) []updated {
	t.Helper()
	var out []updated
	for _, ev := range h.Events() {
		if ev.Kind != model.EventMessageUpdated || ev.Resource != m.Ref.Ref() {
			continue
		}
		u := updated{event: ev}
		if err := json.Unmarshal(ev.Payload.Data, &u.MessageUpdatedPayload); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(ev.Payload.Data, &u.keys); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

func changesOf(us []updated) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.Change
	}
	return out
}

func text(m model.Message) string {
	if len(m.Content.Parts) == 0 {
		return ""
	}
	return m.Content.Parts[0].Text
}

// editedBy is an inbound edit of provider message msgID by from.
func editedBy(h *harness, event, msgID, from string, at time.Time, body string) model.Envelope {
	env := h.Inbound(event, msgID, from, dest)
	env.Kind, env.SourceRevision, env.OccurredAt = model.EnvelopeMessageEdited, event, at
	env.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: body}}}
	return env
}

// reacted is an inbound reaction of kind (added or removed) by from;
// emoji may be empty for a removal naming none.
func reacted(h *harness, event, kind, msgID, from, emoji string) model.Envelope {
	env := h.Inbound(event, msgID, from, dest)
	env.Kind, env.Content = kind, model.Content{}
	if emoji != "" {
		env.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: emoji}}}
	}
	return env
}

// summary is a count-only reaction snapshot from a system actor.
func summary(h *harness, event, msgID string, at time.Time, s model.ReactionSummary) model.Envelope {
	data, err := json.Marshal(s)
	if err != nil {
		h.T.Fatal(err)
	}
	return model.Envelope{
		EventID: event, MessageID: msgID, Destination: dest, Kind: model.EnvelopeReactionAdded, SystemActor: true,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{
			Name: model.SchemaReactionSummary, Version: 1, Data: data,
		}}}},
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: at, Assurance: "verified",
	}
}
