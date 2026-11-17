package bot_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// fakeInteractions serves stored interactions and models the engine's
// response rules the dispatcher relies on: the same operation replays, a
// second operation claiming the initial response is a conflict, and every
// accepted operation is one provider call.
type fakeInteractions struct {
	uma.Interactions
	mu       sync.Mutex
	byRef    map[model.InteractionRef]model.Interaction
	ops      map[model.OperationID]model.Respond
	initial  model.OperationID
	got      []model.Respond
	getErr   error
	respErr  func() error
	getCalls int
}

func (f *fakeInteractions) Get(_ context.Context, req model.Lookup[model.InteractionRef]) (model.Interaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return model.Interaction{}, f.getErr
	}
	in, ok := f.byRef[req.Ref]
	if !ok {
		return model.Interaction{}, model.NewError(model.NotFound, "interaction")
	}
	return in, nil
}

func (f *fakeInteractions) Respond(_ context.Context, req model.Respond) (model.Acceptance, error) {
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, req)
	if f.respErr != nil {
		if err := f.respErr(); err != nil {
			return model.Acceptance{}, err
		}
	}
	if _, ok := f.ops[req.Operation]; ok {
		return model.Acceptance{Replayed: true}, nil
	}
	if !req.FollowUp {
		if f.initial != "" {
			return model.Acceptance{}, model.NewError(model.Conflict, "interaction already has an initial response")
		}
		f.initial = req.Operation
	}
	f.ops[req.Operation] = req
	return model.Acceptance{}, nil
}

func (f *fakeInteractions) submissions() []model.Respond {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.Respond(nil), f.got...)
}

// calls returns the distinct accepted response operations: the provider
// calls the engine would make.
func (f *fakeInteractions) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ops)
}

var interactionAt = time.Unix(1_700_000_000, 0).UTC()

// interaction stores an interaction of the actor "u1" in conversation in and
// returns its interaction.received event. mod adjusts the record first.
func (c *fakeClient) interaction(t *testing.T, n int, in model.ConversationRef, mod ...func(*model.Interaction)) model.Event {
	t.Helper()
	ref := model.InteractionRef{Conversation: in, ID: model.ID("00000000-0000-7000-8000-0000000004" + pad(n))}
	rec := model.Interaction{
		Ref: ref, Kind: model.InteractionAction, Action: "approve",
		OccurredAt: interactionAt, RecordedAt: interactionAt, Deadline: interactionAt.Add(3 * time.Second),
		RespondUntil: interactionAt.Add(15 * time.Minute),
	}
	for _, m := range mod {
		m(&rec)
	}
	c.ints.mu.Lock()
	c.ints.byRef[ref] = rec
	c.ints.mu.Unlock()
	c.convs.mu.Lock()
	if _, ok := c.convs.byRef[in]; !ok {
		c.convs.byRef[in] = model.Conversation{Ref: in, Mode: model.ModePerRoom}
	}
	c.convs.mu.Unlock()
	data, err := json.Marshal(model.InteractionReceivedPayload{
		Interaction: ref, Trigger: model.TriggerProvider, Kind: rec.Kind, Action: rec.Action, Command: rec.Command,
	})
	if err != nil {
		t.Fatal(err)
	}
	return model.Event{
		Version: model.EventEnvelopeVersion, ID: model.ID("00000000-0000-7000-8000-0000000005" + pad(n)),
		Kind: model.EventInteractionReceived, Tenant: tenant, Inbox: inbox.ID,
		Resource: ref.Ref(), Actor: human, Correlation: model.CorrelationID("ix-" + pad(n)),
		Payload: model.Extension{Name: model.SchemaInteractionReceived, Version: model.PayloadVersion1, Data: data},
	}
}

func asCommand(name string) func(*model.Interaction) {
	return func(i *model.Interaction) { i.Kind, i.Action, i.Command = model.InteractionCommand, "", name }
}

func asSubmission(schema string) func(*model.Interaction) {
	return func(i *model.Interaction) {
		i.Kind, i.Action = model.InteractionSubmission, ""
		i.Fields = model.Extension{Name: schema, Version: 1}
	}
}
