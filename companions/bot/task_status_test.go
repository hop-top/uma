package bot_test

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"testing"

	"hop.top/uma/companions/bot"
	"hop.top/uma/model"
)

// taskStatusEvent is the message.created event of a task's status message:
// uma-authored, origin task_status, committed by the host's Tasks.Act.
func taskStatusEvent(t *testing.T, n int) model.Event {
	t.Helper()
	ref := model.MessageRef{Conversation: conv(n), ID: model.ID("00000000-0000-7000-8000-0000000004" + pad(n))}
	data, err := json.Marshal(model.MessageCreatedPayload{
		Message: ref, Trigger: model.TriggerCommand, Origin: model.MessageOriginTaskStatus,
		Operation: model.OperationRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000005" + model.ID(pad(n))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return model.Event{
		Version: model.EventEnvelopeVersion, ID: model.ID("00000000-0000-7000-8000-0000000006" + pad(n)),
		Kind: model.EventMessageCreated, Tenant: tenant, Inbox: inbox.ID, Resource: ref.Ref(), Actor: "host",
		Payload: model.Extension{Name: model.SchemaMessageCreated, Version: model.PayloadVersion1, Data: data},
	}
}

// A task's status message never reaches the message routes: bots route
// message.received only. A message.created handler in Config.Kinds is the
// opt-in, and it sees the task_status origin in the payload.
func TestBotIgnoresTaskStatusUnlessOptedIn(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Patterns = []bot.Pattern{{Name: "any", Match: regexp.MustCompile(""), Handle: replying("should not answer")}}
	b := mustNew(t, cfg)
	c.ev.push(taskStatusEvent(t, 1))
	r := step(t, b)
	if o := outcomes(r); !slices.Equal(o, []bot.Outcome{bot.Ignored}) {
		t.Fatalf("outcomes = %v", o)
	}
	if sent := c.msgs.sent(); len(sent) != 0 {
		t.Fatalf("replies = %v", sent)
	}

	c = newFake()
	cfg = config(c)
	var origins []string
	cfg.Kinds = map[string]bot.Handler{model.EventMessageCreated: func(_ context.Context, t *bot.Turn) error {
		var p model.MessageCreatedPayload
		if err := json.Unmarshal(t.Event.Payload.Data, &p); err != nil {
			return err
		}
		origins = append(origins, p.Origin)
		return nil
	}}
	b = mustNew(t, cfg)
	c.ev.push(taskStatusEvent(t, 2))
	r = step(t, b)
	if o := outcomes(r); !slices.Equal(o, []bot.Outcome{bot.Handled}) || r.Results[0].Route != bot.RouteKind {
		t.Fatalf("results = %+v", r.Results)
	}
	if !slices.Equal(origins, []string{model.MessageOriginTaskStatus}) {
		t.Fatalf("origins = %v", origins)
	}
}
