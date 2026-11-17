package host_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"hop.top/uma"
	"hop.top/uma/companions/bot/host"
	"hop.top/uma/model"
)

var (
	principal = model.Principal{Tenant: "acme", Actor: "helper"}
	refA      = model.SubscriptionRef{Tenant: "acme", ID: "00000000-0000-7000-8000-0000000000a1"}
	filter    = model.Filter{Kinds: []string{model.EventMessageReceived}}
	opA       = model.OperationRef{Tenant: "acme", ID: "00000000-0000-7000-8000-0000000000b1"}
	opB       = model.OperationRef{Tenant: "acme", ID: "00000000-0000-7000-8000-0000000000b2"}
)

// fakeEvents answers Open; any Poll, Ack or Close hits the nil embedded
// interface and panics, which fails a test that consumed too early.
type fakeEvents struct {
	uma.Events
	replayed bool
	op       model.OperationRef // Acceptance.Operation.Ref; opA when zero
	opens    []model.OpenSubscription
}

func (f *fakeEvents) Open(_ context.Context, r model.OpenSubscription) (model.SubscriptionResult, error) {
	f.opens = append(f.opens, r)
	op := f.op
	if op == (model.OperationRef{}) {
		op = opA
	}
	return model.SubscriptionResult{
		Subscription: model.Subscription{Ref: refA, Checkpoint: "cp-1"},
		Acceptance:   model.Acceptance{Operation: model.Operation{Ref: op}, Replayed: f.replayed},
	}, nil
}

type fakeClient struct {
	uma.Client
	ev *fakeEvents
}

func (c fakeClient) Events() uma.Events { return c.ev }

func cfg(ev *fakeEvents, in host.Intent) host.PrepareConfig {
	return host.PrepareConfig{Client: fakeClient{ev: ev}, Principal: principal, Consumer: "support-helper", Filter: filter, Intent: in}
}

func marker() host.Marker {
	return host.Marker{
		Tenant: "acme", Actor: "helper", Consumer: "support-helper", Ref: refA, Operation: opA,
		Open: host.OpenRequest{Start: model.StartTail, Filter: filter, PayloadMajor: model.PayloadVersion1},
	}
}

func TestZeroIntentFails(t *testing.T) {
	ev := &fakeEvents{}
	if _, err := host.Prepare(t.Context(), cfg(ev, host.Intent{})); model.CodeOf(err) != model.Validation {
		t.Fatalf("err = %v, want validation", err)
	}
	if len(ev.opens) != 0 {
		t.Fatal("Open called for a zero intent")
	}
}

func TestBootstrapOpensOnceAndRecordsTheRequest(t *testing.T) {
	ev := &fakeEvents{}
	p, err := host.Prepare(t.Context(), cfg(ev, host.Bootstrap(model.StartTail, "")))
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.opens) != 1 || ev.opens[0].Start != model.StartTail || ev.opens[0].Consumer != "support-helper" {
		t.Fatalf("opens = %+v", ev.opens)
	}
	if !reflect.DeepEqual(p.Marker, marker()) || p.Subscription.Ref != refA || p.Subscription.Checkpoint != "cp-1" || p.Replayed {
		t.Fatalf("prepared = %+v", p)
	}
}

func TestBootstrapNeedsExplicitStart(t *testing.T) {
	ev := &fakeEvents{}
	if _, err := host.Prepare(t.Context(), cfg(ev, host.Bootstrap("", ""))); model.CodeOf(err) != model.Validation {
		t.Fatalf("err = %v, want validation", err)
	}
	if len(ev.opens) != 0 {
		t.Fatal("Open called without a start")
	}
}

// Bootstrap and Resume send the same command: same ID, same payload, which
// is what makes uma replay it.
func TestResumeResendsTheBootstrapRequest(t *testing.T) {
	ev := &fakeEvents{replayed: true}
	if _, err := host.Prepare(t.Context(), cfg(ev, host.Bootstrap(model.StartTail, ""))); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Prepare(t.Context(), cfg(ev, host.Resume(marker()))); err != nil {
		t.Fatal(err)
	}
	a, b := ev.opens[0], ev.opens[1]
	if a.Operation == "" || a.Operation != b.Operation || a.Start != b.Start || a.After != b.After || a.PayloadMajor != b.PayloadMajor {
		t.Fatalf("bootstrap = %+v, resume = %+v, want the identical command", a, b)
	}
}

func TestResumeRejectsUnboundMarkers(t *testing.T) {
	for name, mut := range map[string]func(*host.Marker){
		"zero ref":       func(m *host.Marker) { m.Ref = model.SubscriptionRef{} },
		"other tenant":   func(m *host.Marker) { m.Tenant = "other" },
		"other actor":    func(m *host.Marker) { m.Actor = "someone" },
		"other consumer": func(m *host.Marker) { m.Consumer = "elsewhere" },
		"no start":       func(m *host.Marker) { m.Open.Start = "" },
		"no operation":   func(m *host.Marker) { m.Operation = model.OperationRef{} },
	} {
		m := marker()
		mut(&m)
		ev := &fakeEvents{replayed: true}
		if _, err := host.Prepare(t.Context(), cfg(ev, host.Resume(m))); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
		if len(ev.opens) != 0 {
			t.Errorf("%s: Open called", name)
		}
	}
}

// A different filter would be refused by uma's own Conflict; refuse it
// before sending, naming the cause.
func TestResumeRejectsChangedFilter(t *testing.T) {
	ev := &fakeEvents{replayed: true}
	c := cfg(ev, host.Resume(marker()))
	c.Filter = model.Filter{Kinds: []string{model.EventInteractionReceived}}
	if _, err := host.Prepare(t.Context(), c); model.CodeOf(err) != model.Validation {
		t.Fatalf("err = %v, want validation", err)
	}
	if len(ev.opens) != 0 {
		t.Fatal("Open called with a changed filter")
	}
}

func TestResumeRejectsChangedPayloadMajor(t *testing.T) {
	ev := &fakeEvents{replayed: true}
	c := cfg(ev, host.Resume(marker()))
	c.PayloadMajor = model.PayloadVersion1 + 1
	if _, err := host.Prepare(t.Context(), c); model.CodeOf(err) != model.Validation {
		t.Fatalf("err = %v, want validation", err)
	}
	if len(ev.opens) != 0 {
		t.Fatal("Open called with another payload major")
	}
}

func TestResumeReplayedContinues(t *testing.T) {
	ev := &fakeEvents{replayed: true}
	p, err := host.Prepare(t.Context(), cfg(ev, host.Resume(marker())))
	if err != nil || !p.Replayed || p.Subscription.Ref != refA || p.Marker.Operation != opA {
		t.Fatalf("prepared = %+v, err = %v", p, err)
	}
}

// Bootstrap records the operation uma accepted, also when Bootstrap itself
// is a replay.
func TestBootstrapRecordsTheOperation(t *testing.T) {
	for _, replayed := range []bool{false, true} {
		ev := &fakeEvents{replayed: replayed, op: opB}
		p, err := host.Prepare(t.Context(), cfg(ev, host.Bootstrap(model.StartTail, "")))
		if err != nil || p.Marker.Operation != opB || p.Replayed != replayed {
			t.Fatalf("replayed=%v: prepared = %+v, err = %v", replayed, p, err)
		}
	}
}

// Replayed, but of another operation: the repeat after a first lost Resume,
// whose Open recorded the operation in the replaced store. Still lost.
func TestResumeReplayOfAnotherOperationIsContinuityLost(t *testing.T) {
	ev := &fakeEvents{replayed: true, op: opB}
	_, err := host.Prepare(t.Context(), cfg(ev, host.Resume(marker())))
	if !errors.Is(err, host.ErrSubscriptionContinuityLost) {
		t.Fatalf("err = %v, want continuity lost", err)
	}
	var lost *host.SubscriptionContinuityLost
	if !errors.As(err, &lost) || lost.Expected != opA || lost.Observed != opB || !lost.Replayed {
		t.Fatalf("detail = %+v, want expected %v, observed %v, replayed", lost, opA, opB)
	}
	if msg := err.Error(); !strings.Contains(msg, string(opA.ID)) || !strings.Contains(msg, string(opB.ID)) || !strings.Contains(msg, "replayed true") {
		t.Fatalf("message %q does not name both operations and the replay", msg)
	}
}

// Not a replay: uma has no record of the Open the marker names. Reported
// every time, and nothing was adopted.
func TestResumeWithoutReplayIsContinuityLost(t *testing.T) {
	ev := &fakeEvents{replayed: false}
	for range 2 {
		_, err := host.Prepare(t.Context(), cfg(ev, host.Resume(marker())))
		if !errors.Is(err, host.ErrSubscriptionContinuityLost) {
			t.Fatalf("err = %v, want continuity lost", err)
		}
		var lost *host.SubscriptionContinuityLost
		if !errors.As(err, &lost) || lost.Expected != opA || lost.Replayed || lost.Consumer != "support-helper" {
			t.Fatalf("detail = %+v", lost)
		}
	}
}
