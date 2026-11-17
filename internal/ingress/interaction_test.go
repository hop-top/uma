package ingress_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// validator records what it was asked and answers from fn.
type validator struct {
	mu   sync.Mutex
	seen []model.Principal
	fn   func(context.Context, model.InteractionInput) (model.InteractionDecision, error)
}

func (v *validator) Evaluate(ctx context.Context, p model.Principal, in model.InteractionInput) (model.InteractionDecision, error) {
	v.mu.Lock()
	v.seen = append(v.seen, p)
	v.mu.Unlock()
	return v.fn(ctx, in)
}

func withValidator(v *validator) enginetest.HarnessOption {
	return func(c *engine.Config) { c.InteractionValidator = v }
}

func interaction(h *harness, deadline time.Duration) model.InteractionInput {
	return model.InteractionInput{
		ID: "i1", Kind: model.InteractionSubmission, Actor: h.SenderRef("u1"),
		Fields:        model.Extension{Name: "org.example.form", Version: 1},
		ResponseToken: "response-token", ResponseExpiresAt: t0.Add(time.Hour), Deadline: t0.Add(deadline),
	}
}

// Evaluation asks the validator as the sink's service principal, returns
// its field errors, and writes nothing.
func TestEvaluateInteractionIsReadOnly(t *testing.T) {
	v := &validator{fn: func(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
		return model.InteractionDecision{FieldErrors: map[string]string{"email": "invalid"}}, nil
	}}
	h := newHarness(t, chatType, withValidator(v))
	in := h.Configure(perContact(true), dest)
	before := len(h.Events())
	d, err := h.Sink.EvaluateInteraction(h.Ctx, interaction(h, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if d.FieldErrors["email"] != "invalid" {
		t.Fatalf("decision = %+v, want the validator's field errors", d)
	}
	if len(v.seen) != 1 || v.seen[0] != h.Service() {
		t.Fatalf("validator principals = %v, want the sink's service principal", v.seen)
	}
	if len(h.Events()) != before || len(h.Contacts()) != 0 || len(h.Conversations(in.Ref)) != 0 {
		t.Fatal("evaluation wrote domain state")
	}
}

// A missing validator denies; a malformed or foreign interaction is
// rejected before the validator sees it.
func TestEvaluateInteractionRejects(t *testing.T) {
	h := newHarness(t, chatType)
	_, err := h.Sink.EvaluateInteraction(h.Ctx, interaction(h, time.Second))
	requireCode(t, err, model.Unauthorized, "validator")

	v := &validator{fn: func(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
		return model.InteractionDecision{}, nil
	}}
	h = newHarness(t, chatType, withValidator(v))
	for name, m := range map[string]func(*model.InteractionInput){
		"no id":           func(i *model.InteractionInput) { i.ID = "" },
		"no kind":         func(i *model.InteractionInput) { i.Kind = "" },
		"no deadline":     func(i *model.InteractionInput) { i.Deadline = time.Time{} },
		"foreign actor":   func(i *model.InteractionInput) { i.Actor.Channel.ID = "00000009-0000-7000-8000-000000000009" },
		"unknown kind":    func(i *model.InteractionInput) { i.Kind = "click" },
		"no form":         func(i *model.InteractionInput) { i.Fields = model.Extension{} },
		"token no expiry": func(i *model.InteractionInput) { i.ResponseExpiresAt = time.Time{} },
		"foreign bot": func(i *model.InteractionInput) {
			bot := i.Actor
			bot.Channel.ID = "00000009-0000-7000-8000-000000000009"
			i.Bot = &bot
		},
		"foreign message": func(i *model.InteractionInput) {
			msg := i.Actor
			msg.Channel.ID = "00000009-0000-7000-8000-000000000009"
			i.Message = &msg
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := interaction(h, time.Second)
			m(&in)
			_, err := h.Sink.EvaluateInteraction(h.Ctx, in)
			requireCode(t, err, model.Validation, "")
		})
	}
	if len(v.seen) != 0 {
		t.Fatalf("validator asked %d times, want none", len(v.seen))
	}
}

// Evaluation is bounded by the interaction deadline.
func TestEvaluateInteractionDeadline(t *testing.T) {
	v := &validator{fn: func(ctx context.Context, _ model.InteractionInput) (model.InteractionDecision, error) {
		<-ctx.Done()
		return model.InteractionDecision{}, ctx.Err()
	}}
	h := newHarness(t, chatType, withValidator(v))
	_, err := h.Sink.EvaluateInteraction(h.Ctx, interaction(h, 0))
	requireCode(t, err, model.DeadlineExpired, "deadline")
	if len(v.seen) != 0 {
		t.Fatal("validator asked after the deadline")
	}
	_, err = h.Sink.EvaluateInteraction(h.Ctx, interaction(h, 20*time.Millisecond))
	requireCode(t, err, model.DeadlineExpired, "deadline")

	v.fn = func(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
		return model.InteractionDecision{}, errors.New("validator crashed: secret detail")
	}
	_, err = h.Sink.EvaluateInteraction(h.Ctx, interaction(h, time.Second))
	requireCode(t, err, model.Unavailable, "validator unavailable")
}
