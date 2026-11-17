package slack

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// The provider calls the adapter's request URL as Slack does: a signed
// event_callback for each event, in the documented shapes
// (https://docs.slack.dev/reference/events/), and a signed form for an
// interaction. What Slack cannot say is not expressible: a message only
// some see, a reaction summary, a reaction removed with no name, an echo of
// uma's own edit (the adapter reads none of Slack's message_metadata_* events), and an
// interaction whose acknowledgement deadline has passed (the deadline is
// three seconds from the request, always).

func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if (ev.Visibility != "" && ev.Visibility != model.Public) || ev.Echo != nil || ev.Kind == conformance.InboundReactionSummary {
		return conformance.ErrNotExpressible
	}
	if ev.Kind == conformance.InboundReactionRemoved && ev.Emoji == "" {
		return conformance.ErrNotExpressible
	}
	if ev.Kind == conformance.InboundReactionAdded || ev.Kind == conformance.InboundReactionRemoved {
		if _, ok := reactionName(ev.Emoji); !ok {
			return conformance.ErrNotExpressible
		}
	}
	if ev.Interaction != nil && (ev.Interaction.Deadline < 0 || ev.Interaction.Kind == model.InteractionOptions) {
		return conformance.ErrNotExpressible
	}
	if ev.Interaction != nil && ev.Interaction.Kind == model.InteractionSubmission {
		return conformance.ErrNotExpressible
	}
	// A thread is named by the message a control sits on; without one an
	// interaction has no thread to be scoped by.
	if ev.Interaction != nil && p.mode == model.ModePerThread && ev.MessageID == "" {
		return conformance.ErrNotExpressible
	}
	pre := p.prefix(ch)
	conv, ts, _ := strings.Cut(ev.MessageID, ":")
	switch {
	case ev.Interaction != nil:
	case ev.Kind == conformance.InboundMessage:
		if p.mode == model.ModePerContact {
			p.f.holdDM(pre, ev.Sender, conv)
		}
		if p.f.find(pre, conv, ts) == nil {
			thread := ""
			if p.mode == model.ModePerThread {
				thread = ev.Room
			}
			p.f.put(pre, conv, ev.Sender, ev.Text, ts, thread)
		}
	case ev.Kind == conformance.InboundEdited:
		p.f.edit(pre, conv, ts, ev.Text, editStamp(ev))
	case ev.Kind == conformance.InboundDeleted:
		p.f.remove(pre, conv, ts)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hook == nil {
		p.hook = map[model.ChannelRef][]conformance.InboundEvent{}
	}
	p.hook[ch] = append(p.hook[ch], ev)
	return nil
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
	for _, ev := range evs {
		var req *http.Request
		var body []byte
		if ev.Interaction != nil {
			body = p.interactionForm(ev)
			req = httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			body = p.eventBody(ev)
			req = httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		ts := strconv.FormatInt(p.now().Unix(), 10)
		req.Header.Set(HeaderTimestamp, ts)
		req.Header.Set(HeaderSignature, sign(conformance.CredentialSecret, ts, body))
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

// editStamp is when an edit happened: after the message and after every
// earlier edit of the suite.
func editStamp(ev conformance.InboundEvent) string {
	return formatTS(testNow.Add(ev.OccurredAfter))
}

func (p *provider) channelType() string {
	if p.mode == model.ModePerContact {
		return "im"
	}
	return "channel"
}

// eventBody renders ev as the event_callback Slack sends for it.
func (p *provider) eventBody(ev conformance.InboundEvent) []byte {
	conv, ts, _ := strings.Cut(ev.MessageID, ":")
	if ev.Kind != conformance.InboundMessage && ev.Kind != conformance.InboundEdited && ev.Kind != conformance.InboundDeleted {
		// A reaction: the message it is on names the conversation.
		conv, ts, _ = strings.Cut(ev.MessageID, ":")
	}
	thread := ""
	if p.mode == model.ModePerThread {
		thread = threadOf(ev.Sender)
	}
	message := func(text string) map[string]any {
		m := map[string]any{"type": "message", "user": ev.Sender, "text": text, "ts": ts}
		if thread != "" {
			m["thread_ts"] = thread
		}
		return m
	}
	var inner map[string]any
	switch ev.Kind {
	case conformance.InboundMessage:
		inner = message(ev.Text)
		inner["channel"], inner["channel_type"], inner["event_ts"] = conv, p.channelType(), ts
		if len(ev.Claims) > 0 {
			// Unauthenticated: a name in the payload, never the author.
			inner["username"] = ev.Claims[0]
		}
	case conformance.InboundEdited:
		at := editStamp(ev)
		m := message(ev.Text)
		m["edited"] = map[string]any{"user": ev.Sender, "ts": at}
		inner = map[string]any{"type": "message", "subtype": "message_changed", "hidden": true, "channel": conv, "channel_type": p.channelType(), "ts": at, "event_ts": at, "message": m, "previous_message": message("earlier")}
	case conformance.InboundDeleted:
		at := formatTS(testNow.Add(ev.OccurredAfter + time.Second))
		inner = map[string]any{"type": "message", "subtype": "message_deleted", "hidden": true, "channel": conv, "channel_type": p.channelType(), "ts": at, "deleted_ts": ts, "event_ts": at, "previous_message": message("gone")}
	default:
		name, _ := reactionName(ev.Emoji)
		typ := "reaction_added"
		if ev.Kind == conformance.InboundReactionRemoved {
			typ = "reaction_removed"
		}
		inner = map[string]any{
			"type": typ, "user": ev.Sender, "reaction": name, "item_user": testBotUser, "event_ts": formatTS(testNow.Add(ev.OccurredAfter)),
			"item": map[string]any{"type": "message", "channel": conv, "ts": ts},
		}
	}
	b, _ := json.Marshal(callback(ev.EventID, inner))
	return b
}

// interactionForm renders an interaction as the form Slack posts: a
// block_actions payload for an action, a slash command for a command.
func (p *provider) interactionForm(ev conformance.InboundEvent) []byte {
	payload, command := p.interactionParts(ev)
	if command != nil {
		return []byte(command.Encode())
	}
	b, _ := json.Marshal(payload)
	return []byte(url.Values{"payload": {string(b)}}.Encode())
}

// interactionParts renders an interaction as what Slack sends for it: a
// block_actions payload for an action, or the fields of a slash command.
func (p *provider) interactionParts(ev conformance.InboundEvent) (map[string]any, url.Values) {
	in := ev.Interaction
	conv, ts, _ := strings.Cut(ev.MessageID, ":")
	if conv == "" {
		conv = p.roomOf(ev.Sender)
	}
	hook := ""
	if in.Token != "" {
		hook = p.f.responseBase() + "/" + in.Token
	}
	trigger := ev.EventID
	if in.Kind == model.InteractionCommand {
		f := url.Values{
			"team_id": {testTeam}, "api_app_id": {testApp}, "channel_id": {conv}, "user_id": {ev.Sender}, "user_name": {firstClaim(ev)},
			"command": {"/" + in.Command}, "text": {in.Arguments}, "response_url": {hook}, "trigger_id": {trigger},
		}
		return nil, f
	}
	payload := map[string]any{
		"type": "block_actions", "api_app_id": testApp, "trigger_id": trigger,
		"team": map[string]any{"id": testTeam}, "user": map[string]any{"id": ev.Sender, "username": firstClaim(ev), "team_id": testTeam},
		"channel": map[string]any{"id": conv}, "container": map[string]any{"type": "message", "channel_id": conv, "is_ephemeral": false},
		"actions": []any{map[string]any{"action_id": in.Action, "block_id": "b1", "type": "button", "value": in.Arguments, "action_ts": formatTS(testNow)}},
	}
	if ts != "" {
		msg := map[string]any{"type": "message", "ts": ts}
		if p.mode == model.ModePerThread {
			msg["thread_ts"] = threadOf(ev.Sender)
		}
		payload["message"] = msg
		payload["container"].(map[string]any)["message_ts"] = ts
	}
	if hook != "" {
		payload["response_url"] = hook
	}
	return payload, nil
}

func firstClaim(ev conformance.InboundEvent) string {
	if len(ev.Claims) > 0 {
		return ev.Claims[0]
	}
	return ""
}
