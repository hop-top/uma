package discord

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// Discord sends a gateway dispatch for each event Deliver queued, on the
// account's live connection, in order; an event counts as acknowledged
// when the receiver handled its dispatch without a refusal (the gateway
// has no acknowledgement of its own). Over the Interactions Endpoint URL
// only interactions are sent, each a signed request, acknowledged by a 200.
// What Discord cannot say is not expressible: a message only some see, a
// reaction summary, a removal naming no emoji, an echo of uma's own edit
// (Discord carries nothing of an attempt on an edit), a modal submission
// in the suite's fields, an options request, and an interaction whose
// deadline has passed (it is always three seconds after it arrived).

func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	switch {
	case (ev.Visibility != "" && ev.Visibility != model.Public) || ev.Echo != nil || ev.Kind == conformance.InboundReactionSummary:
		return conformance.ErrNotExpressible
	case ev.Kind == conformance.InboundReactionRemoved && ev.Emoji == "":
		return conformance.ErrNotExpressible
	case ev.Interaction != nil && (ev.Interaction.Deadline < 0 || ev.Interaction.Kind == model.InteractionOptions || ev.Interaction.Kind == model.InteractionSubmission):
		return conformance.ErrNotExpressible
	case ev.Interaction == nil && p.transport == ReceiveInteractions:
		return conformance.ErrNotExpressible
	}
	pre := p.prefix(ch)
	p.ensure(pre, ev.Sender)
	conv, id, _ := strings.Cut(ev.MessageID, ":")
	switch {
	case ev.Interaction != nil:
	case ev.Kind == conformance.InboundMessage:
		if p.f.find(pre, conv, id) == nil {
			p.f.put(pre, conv, ev.Sender, ev.Text, id)
		}
	case ev.Kind == conformance.InboundEdited:
		if m := p.f.find(pre, conv, id); m != nil {
			p.f.mu.Lock()
			m.content, m.edited = ev.Text, editStamp(ev)
			p.f.mu.Unlock()
		}
	case ev.Kind == conformance.InboundDeleted:
		p.f.mu.Lock()
		if c := p.f.accounts[pre].channels[conv]; c != nil {
			c.msgs = deleteMsg(c.msgs, id)
		}
		p.f.mu.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.queued == nil {
		p.queued = map[model.ChannelRef][]conformance.InboundEvent{}
	}
	p.queued[ch] = append(p.queued[ch], ev)
	return nil
}

// ItemState reports whether the thread the item's message is in is
// archived: the thread is the conversation, and archived for everyone.
func (p *provider) ItemState(ch model.ChannelRef, ref model.ExternalRef) (conformance.ItemState, bool) {
	conv, _, ok := parseMessageID(ref.ID)
	if !ok {
		return conformance.ItemState{}, false
	}
	pre := p.prefix(ch)
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	c := p.f.accounts[pre].channels[conv]
	if c == nil {
		return conformance.ItemState{}, false
	}
	return conformance.ItemState{Archived: c.archived}, true
}

// DeliverState: a THREAD_UPDATE names the thread, so the adapter reports
// the conversation by its thread reference, which the engine resolves by
// the thread selection of the inbox. The suite's conversation is stored
// bare, outside any selection, so the report cannot find it: nothing the
// provider could send resolves it, and the report is not expressible. The
// reports are held by TestThreadUpdate* and TestGatewayDispatchesThreadUpdate.
func (p *provider) DeliverState(model.ChannelRef, conformance.StateEvent) error {
	return conformance.ErrNotExpressible
}

func deleteMsg(msgs []*fakeMsg, id string) []*fakeMsg {
	out := msgs[:0]
	for _, m := range msgs {
		if m.id != id {
			out = append(out, m)
		}
	}
	return out
}

// editStamp is when an edit happened: after the message and every earlier
// edit of the suite.
func editStamp(ev conformance.InboundEvent) string {
	return suiteNow.Add(ev.OccurredAfter).Format("2006-01-02T15:04:05.000000+00:00")
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]conformance.InboundResult(nil), p.results[ch]...)
}

// handled records the end of a dispatch the receiver handled.
func (p *provider) handled(ch model.ChannelRef, seq int64, refusal error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.seqs[ch][seq]
	if !ok {
		return
	}
	if p.results == nil {
		p.results = map[model.ChannelRef][]conformance.InboundResult{}
	}
	p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: id, Acked: refusal == nil})
}

// pushGateway sends ch's queued events over c until it closes.
func (p *provider) pushGateway(ch model.ChannelRef, c *fakeGWConn) {
	for {
		select {
		case <-c.done:
			return
		default:
		}
		p.mu.Lock()
		evs := p.queued[ch]
		var ev conformance.InboundEvent
		if len(evs) > 0 {
			ev, p.queued[ch] = evs[0], evs[1:]
		}
		p.mu.Unlock()
		if len(evs) == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		typ, data := p.dispatchOf(ev)
		p.mu.Lock()
		if p.seqs == nil {
			p.seqs = map[model.ChannelRef]map[int64]string{}
		}
		if p.seqs[ch] == nil {
			p.seqs[ch] = map[int64]string{}
		}
		p.mu.Unlock()
		// The sequence number is the session's next; it is mapped before
		// the dispatch is sent, so the receiver never handles one unmapped.
		c.mu.Lock()
		next := c.sess.seq + 1
		c.mu.Unlock()
		p.mu.Lock()
		p.seqs[ch][next] = ev.EventID
		p.mu.Unlock()
		c.dispatch(typ, data)
	}
}

// dispatchOf renders ev as the gateway dispatch Discord sends for it.
func (p *provider) dispatchOf(ev conformance.InboundEvent) (string, any) {
	conv, id, _ := strings.Cut(ev.MessageID, ":")
	if ev.Interaction != nil {
		return "INTERACTION_CREATE", p.interactionOf(ev)
	}
	guild := testGuild
	if p.mode == model.ModePerContact {
		guild = ""
	}
	message := func(text string) map[string]any {
		name := "someone"
		if len(ev.Claims) > 0 {
			// Unauthenticated: a name in the payload, never the author.
			name = ev.Claims[0]
		}
		m := msgEvent(id, conv, guild, ev.Sender, text, map[string]any{"author": map[string]any{"id": ev.Sender, "username": name, "global_name": name}})
		m["timestamp"] = snowflakeTime(id).Format("2006-01-02T15:04:05.000000+00:00")
		return m
	}
	switch ev.Kind {
	case conformance.InboundMessage:
		return "MESSAGE_CREATE", message(ev.Text)
	case conformance.InboundEdited:
		m := message(ev.Text)
		m["edited_timestamp"] = editStamp(ev)
		return "MESSAGE_UPDATE", m
	case conformance.InboundDeleted:
		d := map[string]any{"id": id, "channel_id": conv}
		if guild != "" {
			d["guild_id"] = guild
		}
		return "MESSAGE_DELETE", d
	}
	typ := "MESSAGE_REACTION_ADD"
	if ev.Kind == conformance.InboundReactionRemoved {
		typ = "MESSAGE_REACTION_REMOVE"
	}
	d := map[string]any{"user_id": ev.Sender, "channel_id": conv, "message_id": id, "emoji": map[string]any{"id": nil, "name": ev.Emoji}}
	if guild != "" {
		d["guild_id"] = guild
	}
	return typ, d
}

// interactionOf renders an interaction: a component on the message it
// names, or a slash command.
func (p *provider) interactionOf(ev conformance.InboundEvent) map[string]any {
	in := ev.Interaction
	conv, id, _ := strings.Cut(ev.MessageID, ":")
	if conv == "" {
		conv = p.roomOf(ev.Sender)
	}
	chType, parent := 0, ""
	switch p.mode {
	case model.ModePerThread:
		chType, parent = 11, threadRoom
	case model.ModePerContact:
		chType = 1
	}
	channel := map[string]any{"id": conv, "type": chType}
	if parent != "" {
		channel["parent_id"] = parent
	}
	name := "someone"
	if len(ev.Claims) > 0 {
		name = ev.Claims[0]
	}
	who := map[string]any{"id": ev.Sender, "username": name}
	payload := map[string]any{
		"id": ev.EventID, "application_id": testApp, "token": in.Token, "version": 1, "channel_id": conv, "channel": channel,
	}
	if p.mode == model.ModePerContact {
		payload["user"] = who
	} else {
		payload["guild_id"], payload["member"] = testGuild, map[string]any{"user": who}
	}
	if in.Kind == model.InteractionCommand {
		payload["type"] = interactionCommand
		data := map[string]any{"name": in.Command, "type": 1}
		if in.Arguments != "" {
			data["options"] = []any{map[string]any{"name": "text", "type": 3, "value": in.Arguments}}
		}
		payload["data"] = data
		return payload
	}
	payload["type"] = interactionComponent
	payload["data"] = map[string]any{"custom_id": in.Action, "component_type": 2}
	if id != "" {
		payload["message"] = map[string]any{"id": id, "channel_id": conv, "flags": 0}
	}
	return payload
}

// Push sends the queued interactions to the Interactions Endpoint URL as
// Discord does: signed, and acknowledged by a 200 carrying the callback.
func (p *provider) Push(ch model.ChannelRef, h http.Handler) error {
	p.mu.Lock()
	evs := p.queued[ch]
	p.queued[ch] = nil
	p.mu.Unlock()
	pre := p.prefix(ch)
	for _, ev := range evs {
		body, _ := json.Marshal(p.interactionOf(ev))
		req := httptest.NewRequest(http.MethodPost, "/discord", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ts := strconv.FormatInt(p.now().Unix(), 10)
		req.Header.Set(HeaderTimestamp, ts)
		req.Header.Set(HeaderSignature, signBody(suiteKey, ts, body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var cb struct {
			Type int `json:"type"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &cb)
		if ev.Interaction != nil && rec.Code == http.StatusOK {
			p.f.acknowledge(pre, ev.Interaction.Token, cb.Type)
		}
		p.mu.Lock()
		if p.results == nil {
			p.results = map[model.ChannelRef][]conformance.InboundResult{}
		}
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: rec.Code == http.StatusOK})
		p.mu.Unlock()
	}
	return nil
}
