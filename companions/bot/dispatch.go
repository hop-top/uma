package bot

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"hop.top/uma/model"
)

// item is one event of a batch with its position and prior state.
type item struct {
	at      int
	event   model.Event
	conv    model.ConversationRef
	settled *Result
	attempt int
}

// dispatch runs the events of one batch and returns their results in batch
// order. Events of one conversation form a lane run in order by one
// goroutine; at most Concurrency lanes run at once. Every goroutine has
// returned when dispatch returns.
func (b *Bot) dispatch(ctx context.Context, events []model.Event) []Result {
	results := make([]Result, len(events))
	var lanes [][]item
	index := map[string]int{}
	for i, ev := range events {
		it := item{at: i, event: ev, conv: conversationOf(ev), attempt: b.attempts[ev.ID] + 1}
		if r, ok := b.settled[ev.ID]; ok {
			it.settled = &r
		}
		key := "event/" + string(ev.ID)
		if it.conv != (model.ConversationRef{}) {
			key = "conversation/" + string(it.conv.Inbox.Tenant) + "/" + string(it.conv.Inbox.ID) + "/" + string(it.conv.ID)
		}
		n, ok := index[key]
		if !ok {
			n = len(lanes)
			index[key] = n
			lanes = append(lanes, nil)
		}
		lanes[n] = append(lanes[n], it)
	}
	slots := make(chan struct{}, b.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, lane := range lanes {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			for _, it := range lane {
				results[it.at] = b.unrun(it, ctx.Err())
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			b.runLane(ctx, lane, results)
		}()
	}
	wg.Wait()
	return results
}

// runLane runs one conversation's events in order. After a retryable
// failure the rest of the lane waits for the redelivery.
func (b *Bot) runLane(ctx context.Context, lane []item, results []Result) {
	var blocked error
	for _, it := range lane {
		switch {
		case blocked != nil:
			results[it.at] = b.unrun(it, blocked)
		case it.settled != nil:
			results[it.at] = *it.settled
		default:
			r := b.handle(ctx, it)
			if r.Outcome == Retried {
				blocked = r.Err
			}
			results[it.at] = r
		}
	}
}

// unrun is the result of an event that did not run this time.
func (b *Bot) unrun(it item, cause error) Result {
	if it.settled != nil {
		return *it.settled
	}
	return Result{Event: it.event, Outcome: Retried, Attempt: it.attempt - 1, Err: cause}
}

// handle routes and runs one event.
func (b *Bot) handle(ctx context.Context, it item) Result {
	ev := it.event
	r := Result{Event: ev, Attempt: it.attempt}
	if ev.Actor == b.cfg.Self {
		r.Outcome = Ignored
		return r
	}
	t := &Turn{Event: ev, Conversation: it.conv, bot: b}
	var h Handler
	var err error
	switch ev.Kind {
	case model.EventMessageReceived:
		h, err = b.routeMessage(ctx, t)
	case model.EventInteractionReceived:
		h, err = b.routeInteraction(ctx, t)
	default:
		if h = b.kinds[ev.Kind]; h != nil {
			t.Route, t.Handler = RouteKind, "kind/"+ev.Kind
		}
	}
	if err == nil && h != nil {
		err = h(ctx, t)
	}
	r.Route, r.Handler = t.Route, t.Handler
	switch {
	case err != nil:
		r.Outcome, r.Err = Classify(err), err
		if r.Outcome == Ignored {
			r.Err = nil
		}
		if r.Outcome == Retried && r.Attempt >= b.cfg.MaxAttempts {
			r.Outcome = Dropped
		}
	case h == nil:
		r.Outcome = Ignored
	default:
		r.Outcome = Handled
	}
	return r
}

// routeMessage reads the message of a message.received event and selects
// its route, filling t. It returns a nil handler when no route claims the
// message or the message echoes the bot's own operation.
func (b *Bot) routeMessage(ctx context.Context, t *Turn) (Handler, error) {
	if b.cfg.Direct == nil && b.cfg.Watched == nil && b.cfg.Mention == nil && len(b.patterns) == 0 && b.cfg.Fallback == nil {
		return nil, nil
	}
	ev := t.Event
	if ev.Payload.Name != model.SchemaMessageReceived || ev.Payload.Version != model.PayloadVersion1 {
		return nil, &model.Error{Code: model.SchemaMismatch, SafeMessage: "bot: unsupported message event payload", Retry: model.NoRetry}
	}
	var p model.MessageReceivedPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil || p.Message.Validate() != nil {
		return nil, &model.Error{Code: model.SchemaMismatch, SafeMessage: "bot: malformed message event payload", Retry: model.NoRetry}
	}
	msg, err := b.cfg.Client.Messages().Get(ctx, model.Lookup[model.MessageRef]{Call: t.call(), Ref: p.Message})
	if err != nil {
		return nil, err
	}
	if msg.Origin != nil && msg.Origin.Actor == b.cfg.Self {
		return nil, nil
	}
	t.Message, t.Conversation = &msg, msg.Ref.Conversation
	if b.cfg.Direct != nil {
		conv, err := b.cfg.Client.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: t.call(), Ref: t.Conversation})
		if err != nil {
			return nil, err
		}
		if b.cfg.IsDirect(conv) {
			t.Route, t.Handler = RouteDirect, string(RouteDirect)
			return b.cfg.Direct, nil
		}
	}
	if b.cfg.Watched != nil {
		ok, err := b.cfg.Watches.Has(ctx, Watch{Bot: b.cfg.Self, Conversation: t.Conversation})
		if err != nil {
			return nil, err
		}
		if ok {
			t.Route, t.Handler = RouteWatched, string(RouteWatched)
			return b.cfg.Watched, nil
		}
	}
	if b.cfg.Mention != nil && b.cfg.IsMention(msg) {
		t.Route, t.Handler = RouteMention, string(RouteMention)
		return b.cfg.Mention, nil
	}
	if len(b.patterns) > 0 {
		body := Text(msg.Content)
		for _, p := range b.patterns {
			if p.Match.MatchString(body) {
				t.Route, t.Handler = RoutePattern, string(RoutePattern)+"/"+p.Name
				return p.Handle, nil
			}
		}
	}
	if b.cfg.Fallback != nil {
		t.Route, t.Handler = RouteFallback, string(RouteFallback)
		return b.cfg.Fallback, nil
	}
	return nil, nil
}

// Text returns the text parts of c joined by newlines.
func Text(c model.Content) string {
	var parts []string
	for _, p := range c.Parts {
		if p.Kind == model.PartText {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// TextContent returns content holding one text part. It is the writing
// counterpart of Text; the result is ordinary structured content.
func TextContent(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

// refs holds the payload references that name a conversation.
type refs struct {
	Message      *model.MessageRef      `json:"message"`
	Interaction  *model.InteractionRef  `json:"interaction"`
	Conversation *model.ConversationRef `json:"conversation"`
	WorkItem     *model.WorkItemRef     `json:"work_item"`
}

// conversationOf returns the conversation a builtin event payload names,
// or the zero ref. It reads the payload only.
func conversationOf(ev model.Event) model.ConversationRef {
	if !strings.HasPrefix(ev.Payload.Name, model.CoreNamespace+".") {
		return model.ConversationRef{}
	}
	var r refs
	if json.Unmarshal(ev.Payload.Data, &r) != nil {
		return model.ConversationRef{}
	}
	switch {
	case r.Message != nil:
		return r.Message.Conversation
	case r.Interaction != nil:
		return r.Interaction.Conversation
	case r.Conversation != nil:
		return *r.Conversation
	case r.WorkItem != nil:
		return r.WorkItem.Conversation
	}
	return model.ConversationRef{}
}
