package runtime_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

// carrierJob is a testJob that names the interaction its effect answers.
type carrierJob struct {
	testJob
	ic *adapter.InteractionContext
}

func (j carrierJob) Interaction() *adapter.InteractionContext { return j.ic }

type carrierEffector struct{ job carrierJob }

func (carrierEffector) Intent() string                                { return testIntent }
func (f carrierEffector) Decode(model.Extension) (runtime.Job, error) { return f.job, nil }

// A job naming an interaction hands it to the adapter in the attempt's
// effect, token included, as a copy; a job naming none leaves it nil. The
// stored attempt evidence and the operation never hold the token.
func TestEffectCarriesInteraction(t *testing.T) {
	r := newRig(t)
	var effects []adapter.Effect
	ref := model.InteractionRef{Conversation: r.Conv.Ref, ID: "00000003-0000-7000-8000-0000000000aa"}
	ic := &adapter.InteractionContext{
		Ref: ref, ProviderID: "pi-1", Kind: model.InteractionAction, Token: "tok-secret",
		ExpiresAt: r.Clock.Now().Add(time.Hour),
	}
	job := carrierJob{testJob{testEffector{ch: r.Ch, route: r.Conv.ReplyRoute, effects: &effects}}, ic}
	rt := runtime.New(r.E, carrierEffector{job})
	op := queueIntent(t, r, model.Extension{Name: testIntent, Version: 1})
	step(t, r, rt)
	requireState(t, r.Op(op), model.ProviderAccepted, "")
	if len(effects) != 1 || effects[0].Interaction == nil {
		t.Fatalf("effects = %+v, want one carrying the interaction", effects)
	}
	got := effects[0].Interaction
	if got.Ref != ref || got.Token != "tok-secret" || got.ProviderID != "pi-1" {
		t.Fatalf("effect interaction = %v, want the job's context with its token", got)
	}
	if got == ic {
		t.Fatal("effect shares the job's context; want a copy")
	}
	for _, ev := range r.Evidence(op) {
		if s := fmt.Sprintf("%+v", ev); strings.Contains(s, "tok-secret") {
			t.Fatalf("attempt evidence holds the token: %s", s)
		}
	}
	if s := fmt.Sprintf("%+v", r.Op(op)); strings.Contains(s, "tok-secret") {
		t.Fatalf("operation holds the token: %s", s)
	}
	for _, ev := range r.Events() {
		if s := fmt.Sprintf("%+v", ev); strings.Contains(s, "tok-secret") {
			t.Fatalf("event holds the token: %s", s)
		}
	}
}

// A job that is no carrier gets an effect without an interaction.
func TestEffectWithoutCarrierHasNoInteraction(t *testing.T) {
	r := newRig(t)
	var effects []adapter.Effect
	rt := runtime.New(r.E, testEffector{ch: r.Ch, route: r.Conv.ReplyRoute, effects: &effects})
	queueIntent(t, r, model.Extension{Name: testIntent, Version: 1})
	step(t, r, rt)
	if len(effects) != 1 || effects[0].Interaction != nil {
		t.Fatalf("effects = %+v, want one without an interaction", effects)
	}
}
