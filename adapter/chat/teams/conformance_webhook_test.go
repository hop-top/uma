package teams

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// The provider calls the adapter's messaging endpoint as the Bot Connector
// does: one activity for each event, in the documented shapes
// (https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/conversations/subscribe-to-conversation-events),
// with a signed Bot Connector token. What Teams cannot say is not
// expressible: a message only some see, a reaction summary, a removal
// naming no reaction, an echo of uma's own mutation (no marker rides on a
// message), an interaction other than an Action.Execute on a card's
// message, and one whose acknowledgement deadline has passed (it is the
// adapter's own, from the moment it reads the invoke).

func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if (ev.Visibility != "" && ev.Visibility != model.Public) || ev.Echo != nil || ev.Kind == conformance.InboundReactionSummary {
		return conformance.ErrNotExpressible
	}
	if (ev.Kind == conformance.InboundReactionAdded || ev.Kind == conformance.InboundReactionRemoved) && reactionKindFor(ev.Emoji) == "" {
		return conformance.ErrNotExpressible
	}
	if in := ev.Interaction; in != nil {
		if in.Deadline < 0 || (in.Kind != "" && in.Kind != model.InteractionAction) || ev.MessageID == "" {
			return conformance.ErrNotExpressible
		}
	}
	pre := p.prefix(ch)
	conv, id, _ := parseMessageID(ev.MessageID)
	if ev.Interaction == nil && ev.Kind == conformance.InboundMessage && p.f.find(pre, conv, id) == nil {
		full, _ := p.conversationOf(ev.Sender)
		p.f.put(pre, full, ev.Sender, ev.Text, id)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hook == nil {
		p.hook = map[model.ChannelRef][]conformance.InboundEvent{}
	}
	p.hook[ch] = append(p.hook[ch], ev)
	return nil
}

// reactionKindFor is the reaction type Teams reports for an emoji: one of
// the six named types, or its code points in hexadecimal and a name.
func reactionKindFor(emoji string) string {
	if k, ok := reactionType(emoji); ok {
		return k
	}
	var kind string
	for _, r := range emoji {
		kind += fmt.Sprintf("%04x_", r)
	}
	if kind == "" {
		return ""
	}
	kind += "emoji"
	if e, ok := reactionEmoji(kind); !ok || e != emoji {
		return ""
	}
	return kind
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]conformance.InboundResult(nil), p.results[ch]...)
}

func (p *provider) Push(ch model.ChannelRef, h http.Handler) error {
	p.mu.Lock()
	evs := p.hook[ch]
	p.hook[ch] = nil
	p.mu.Unlock()
	pre := p.prefix(ch)
	for _, ev := range evs {
		body, err := json.Marshal(p.activity(pre, ev))
		if err != nil {
			return err
		}
		req := httptest.NewRequest(http.MethodPost, "/api/messages", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.f.token(testAppID, p.f.serviceURL(pre)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		p.mu.Lock()
		if p.results == nil {
			p.results = map[model.ChannelRef][]conformance.InboundResult{}
		}
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: rec.Code == http.StatusOK})
		p.mu.Unlock()
	}
	return nil
}

type obj = map[string]any

// activity renders ev as the activity the Bot Connector posts for it.
func (p *provider) activity(pre string, ev conformance.InboundEvent) obj {
	conv, kind := p.conversationOf(ev.Sender)
	_, id, _ := parseMessageID(ev.MessageID)
	at := p.now()
	from := obj{"id": ev.Sender, "name": "peer"}
	a := obj{
		"id": id, "timestamp": at.Format(time.RFC3339Nano), "serviceUrl": p.f.serviceURL(pre), "channelId": TeamsChannelID,
		"from": from, "recipient": obj{"id": testBot, "name": "uma"},
		"conversation": obj{"id": conv, "conversationType": kind, "tenantId": testTenantID},
		"channelData":  obj{"tenant": obj{"id": testTenantID}},
	}
	if len(ev.Claims) > 0 {
		// Unauthenticated: a name, an Entra object ID and a mention in the
		// payload, never the author.
		from["name"], from["aadObjectId"] = "claim "+ev.Claims[0], ev.Claims[0]
		a["entities"] = []obj{{"type": "mention", "mentioned": obj{"id": ev.Claims[0], "name": "claim"}, "text": "<at>claim</at>"}}
	}
	switch {
	case ev.Interaction != nil:
		in := ev.Interaction
		data := any(nil)
		if in.Arguments != "" {
			data = in.Arguments
		}
		a["type"], a["name"], a["id"], a["replyToId"] = "invoke", ActionExecuteInvoke, ev.EventID, id
		a["value"] = obj{"action": obj{"type": "Action.Execute", "verb": in.Action, "data": data}, "trigger": "manual"}
	case ev.Kind == conformance.InboundMessage:
		a["type"], a["text"] = "message", ev.Text
	case ev.Kind == conformance.InboundEdited:
		a["type"], a["text"] = "messageUpdate", ev.Text
		a["timestamp"] = at.Add(ev.OccurredAfter).Format(time.RFC3339Nano)
		a["channelData"] = obj{"eventType": "editMessage", "tenant": obj{"id": testTenantID}}
	case ev.Kind == conformance.InboundDeleted:
		a["type"] = "messageDelete"
		a["timestamp"] = at.Add(ev.OccurredAfter).Format(time.RFC3339Nano)
		a["channelData"] = obj{"eventType": "softDeleteMessage", "tenant": obj{"id": testTenantID}}
	default:
		r := []obj{{"type": reactionKindFor(ev.Emoji)}}
		a["type"], a["id"], a["replyToId"] = "messageReaction", ev.EventID, id
		a["timestamp"] = at.Add(ev.OccurredAfter).Format(time.RFC3339Nano)
		if ev.Kind == conformance.InboundReactionRemoved {
			a["reactionsRemoved"] = r
		} else {
			a["reactionsAdded"] = r
		}
	}
	return a
}
