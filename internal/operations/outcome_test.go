package operations_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

var at0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

const (
	attemptA model.ID = "00000000-0000-7000-8000-00000000000a"
	attemptB model.ID = "00000000-0000-7000-8000-00000000000b"
)

var final = &model.Error{Code: model.Permanent, SafeMessage: "gateway reported the delivery failed", Retry: model.NoRetry}

func ref(id string) model.ExternalRef {
	return model.ExternalRef{Channel: model.ChannelRef{Tenant: "t", ID: "c"}, Namespace: "ns", Kind: "message", ID: id}
}

// ev is an evidence entry of attempt a.
func ev(a model.ID, st model.DeliveryState, mut ...func(*store.AttemptEvidence)) store.AttemptEvidence {
	e := store.AttemptEvidence{Operation: model.OperationRef{Tenant: "t", ID: "op"}, Attempt: a, State: st, ObservedAt: at0}
	for _, m := range mut {
		m(&e)
	}
	return e
}

func withFailure(f *model.Error) func(*store.AttemptEvidence) {
	return func(e *store.AttemptEvidence) { e.Failure = f }
}

func withRead(m int) func(*store.AttemptEvidence) {
	return func(e *store.AttemptEvidence) { t := at0.Add(time.Duration(m) * time.Minute); e.ReadAt = &t }
}

func withRefs(ids ...string) func(*store.AttemptEvidence) {
	return func(e *store.AttemptEvidence) {
		for _, id := range ids {
			e.ProviderReferences = append(e.ProviderReferences, ref(id))
		}
	}
}

// The outcome of an operation is the strongest evidence it holds, whatever
// the order the evidence arrived in: provider_accepted < failed (final) <
// delivered < delivered and read.
func TestOutcomeIsTheStrongestEvidenceInAnyOrder(t *testing.T) {
	start := ev(attemptA, model.Unknown)
	acc := ev(attemptA, model.ProviderAccepted)
	queued := ev(attemptA, model.Queued)
	del := ev(attemptA, model.Delivered)
	rd := ev(attemptA, model.Delivered, withRead(7))
	fail := ev(attemptA, model.Failed, withFailure(final))
	rej := ev(attemptA, model.Rejected, withFailure(final))
	retried := ev(attemptA, model.Failed, withFailure(&model.Error{Code: model.Transient, SafeMessage: "x", Retry: model.RetrySameOperation}))
	for _, tt := range []struct {
		name string
		evs  []store.AttemptEvidence
		want model.DeliveryState
	}{
		{"nothing", nil, ""},
		{"only the start", []store.AttemptEvidence{start}, ""},
		{"accepted", []store.AttemptEvidence{acc}, model.ProviderAccepted},
		{"queued is acceptance", []store.AttemptEvidence{queued}, model.ProviderAccepted},
		{"delivered", []store.AttemptEvidence{del}, model.Delivered},
		{"read", []store.AttemptEvidence{rd}, model.Delivered},
		{"final failure", []store.AttemptEvidence{fail}, model.Failed},
		{"rejection", []store.AttemptEvidence{rej}, model.Rejected},
		{"a failure the runtime retried is no verdict", []store.AttemptEvidence{retried}, ""},
		{"a failure without a summary is no verdict", []store.AttemptEvidence{ev(attemptA, model.Failed)}, ""},
		{"accepted below failed", []store.AttemptEvidence{acc, fail}, model.Failed},
		{"failed above accepted", []store.AttemptEvidence{fail, acc}, model.Failed},
		{"failed below delivered", []store.AttemptEvidence{fail, del}, model.Delivered},
		{"delivered above failed", []store.AttemptEvidence{del, fail}, model.Delivered},
		{"accepted below delivered", []store.AttemptEvidence{del, acc}, model.Delivered},
		{"read above delivered", []store.AttemptEvidence{rd, del, acc, fail}, model.Delivered},
		{"retried failure below acceptance", []store.AttemptEvidence{retried, acc}, model.ProviderAccepted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The start of the attempt comes first; the reports after it
			// arrive in every order.
			for _, order := range permutations(tt.evs) {
				got := operations.ResolveOutcome(append([]store.AttemptEvidence{start}, order...)...)
				if got.State != tt.want {
					t.Fatalf("outcome %s of %s, want %q", got.State, describe(order), tt.want)
				}
			}
		})
	}
}

func describe(evs []store.AttemptEvidence) string {
	var s []string
	for _, e := range evs {
		s = append(s, string(e.State))
	}
	return fmt.Sprint(s)
}

func permutations[T any](in []T) [][]T {
	if len(in) <= 1 {
		return [][]T{slices.Clone(in)}
	}
	var out [][]T
	for i := range in {
		rest := slices.Concat(in[:i:i], in[i+1:])
		for _, p := range permutations(rest) {
			out = append(out, append([]T{in[i]}, p...))
		}
	}
	return out
}

// The first read recorded is the one kept, and read always belongs to a
// delivered outcome.
func TestOutcomeKeepsTheFirstRead(t *testing.T) {
	o := operations.ResolveOutcome(ev(attemptA, model.Unknown), ev(attemptA, model.Delivered, withRead(5)), ev(attemptA, model.Delivered, withRead(2)))
	if o.State != model.Delivered || o.ReadAt == nil || !o.ReadAt.Equal(at0.Add(5*time.Minute)) {
		t.Fatalf("outcome = %+v, want delivered, read at minute 5", o)
	}
	if o := operations.ResolveOutcome(ev(attemptA, model.Delivered)); o.ReadAt != nil {
		t.Fatalf("read time %v without a read report", o.ReadAt)
	}
}

// A failure counts for the attempt that reported it. It fails the operation
// only on the latest attempt and when no attempt was applied; delivery of
// any attempt delivers it.
func TestOutcomeAcrossAttempts(t *testing.T) {
	for _, tt := range []struct {
		name string
		evs  []store.AttemptEvidence
		want model.DeliveryState
	}{
		{
			"a later attempt that was accepted outlives an earlier failure",
			[]store.AttemptEvidence{ev(attemptA, model.Unknown), ev(attemptA, model.Failed, withFailure(final)), ev(attemptB, model.Unknown), ev(attemptB, model.ProviderAccepted)},
			model.ProviderAccepted,
		},
		{
			"an earlier failure after a later acceptance is stale",
			[]store.AttemptEvidence{ev(attemptA, model.Unknown), ev(attemptB, model.Unknown), ev(attemptB, model.ProviderAccepted), ev(attemptA, model.Failed, withFailure(final))},
			model.ProviderAccepted,
		},
		{
			"an earlier failure while the latest attempt is open waits",
			[]store.AttemptEvidence{ev(attemptA, model.Unknown), ev(attemptB, model.Unknown), ev(attemptA, model.Failed, withFailure(final))},
			"",
		},
		{
			"the latest attempt failing fails the operation",
			[]store.AttemptEvidence{ev(attemptA, model.Unknown), ev(attemptB, model.Unknown), ev(attemptB, model.Failed, withFailure(final))},
			model.Failed,
		},
		{
			"the latest attempt failing does not fail an operation an earlier attempt carried",
			[]store.AttemptEvidence{ev(attemptA, model.Unknown), ev(attemptA, model.ProviderAccepted), ev(attemptB, model.Unknown), ev(attemptB, model.Failed, withFailure(final))},
			model.ProviderAccepted,
		},
		{
			"an accepted attempt later reported failed is failed",
			[]store.AttemptEvidence{ev(attemptA, model.Unknown), ev(attemptA, model.ProviderAccepted), ev(attemptA, model.Failed, withFailure(final))},
			model.Failed,
		},
		{
			"delivery of an earlier attempt delivers the operation",
			[]store.AttemptEvidence{ev(attemptA, model.Unknown), ev(attemptB, model.Unknown), ev(attemptB, model.ProviderAccepted), ev(attemptA, model.Delivered)},
			model.Delivered,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := operations.ResolveOutcome(tt.evs...); got.State != tt.want {
				t.Fatalf("outcome = %q, want %q", got.State, tt.want)
			}
		})
	}
}

// The provider references of the evidence that counted are kept, once each.
func TestOutcomeKeepsReferences(t *testing.T) {
	o := operations.ResolveOutcome(
		ev(attemptA, model.Unknown),
		ev(attemptA, model.ProviderAccepted, withRefs("m-1")),
		ev(attemptA, model.Delivered, withRefs("m-1", "m-2")),
	)
	if want := []model.ExternalRef{ref("m-1"), ref("m-2")}; !slices.Equal(o.References, want) {
		t.Fatalf("references = %v, want %v", o.References, want)
	}
}

func op(st model.DeliveryState, mut ...func(*model.Operation)) model.Operation {
	o := model.Operation{Ref: model.OperationRef{Tenant: "t", ID: "op"}, State: st}
	for _, m := range mut {
		m(&o)
	}
	return o
}

func readAt(m int) func(*model.Operation) {
	return func(o *model.Operation) { t := at0.Add(time.Duration(m) * time.Minute); o.ReadConfirmedAt = &t }
}

// Advance moves a settled operation up the ranking and never down: every
// pair of operation state and outcome.
func TestAdvanceMovesUpOnly(t *testing.T) {
	outcomes := map[string]operations.Outcome{
		"accepted":  operations.ResolveOutcome(ev(attemptA, model.ProviderAccepted, withRefs("m-1"))),
		"failed":    operations.ResolveOutcome(ev(attemptA, model.Failed, withFailure(final))),
		"delivered": operations.ResolveOutcome(ev(attemptA, model.Delivered, withRefs("m-1"))),
		"read":      operations.ResolveOutcome(ev(attemptA, model.Delivered, withRead(3))),
		"none":      {},
	}
	for _, tt := range []struct {
		op      model.Operation
		moved   map[string]bool
		wantNew map[string]model.DeliveryState
	}{
		{
			op(model.ProviderAccepted),
			map[string]bool{"failed": true, "delivered": true, "read": true},
			map[string]model.DeliveryState{"failed": model.Failed, "delivered": model.Delivered, "read": model.Delivered},
		},
		{
			op(model.Failed, func(o *model.Operation) { o.Failure = final }),
			map[string]bool{"delivered": true, "read": true},
			map[string]model.DeliveryState{"delivered": model.Delivered, "read": model.Delivered},
		},
		{
			op(model.Rejected, func(o *model.Operation) { o.Failure = final }),
			map[string]bool{"delivered": true, "read": true},
			map[string]model.DeliveryState{"delivered": model.Delivered, "read": model.Delivered},
		},
		{op(model.Delivered), map[string]bool{"read": true}, map[string]model.DeliveryState{"read": model.Delivered}},
		{op(model.Delivered, readAt(1)), nil, nil},
		// Any other state is not Advance's to move.
		{op(model.Unknown), nil, nil},
		{op(model.Queued), nil, nil},
		{op(model.RetryPending), nil, nil},
		{op(model.DeadLettered), nil, nil},
		{op(model.Committed), nil, nil},
	} {
		for name, o := range outcomes {
			t.Run(fmt.Sprintf("%s with %s", tt.op.State, name), func(t *testing.T) {
				before := tt.op.Clone()
				got, moved := operations.Advance(tt.op, o)
				if moved != tt.moved[name] {
					t.Fatalf("moved = %v, want %v", moved, tt.moved[name])
				}
				if !moved {
					if got.State != before.State {
						t.Fatalf("state %s -> %s without moving", before.State, got.State)
					}
					return
				}
				if got.State != tt.wantNew[name] {
					t.Fatalf("state = %s, want %s", got.State, tt.wantNew[name])
				}
				if (name == "read") != (got.ReadConfirmedAt != nil) {
					t.Fatalf("read confirmed at %v for %s", got.ReadConfirmedAt, name)
				}
				if got.State == model.Delivered && got.Failure != nil {
					t.Fatalf("a delivered operation keeps the failure %v", got.Failure)
				}
				if got.State == model.Failed && (got.Failure == nil || got.Failure.Retry != model.NoRetry) {
					t.Fatalf("failure = %v, want the final one", got.Failure)
				}
				if tt.op.ReadConfirmedAt != nil != (before.ReadConfirmedAt != nil) {
					t.Fatalf("Advance changed its argument")
				}
			})
		}
	}
}

// Advance keeps the read time an operation recorded and merges references.
func TestAdvanceKeepsWhatIsRecorded(t *testing.T) {
	cur := op(model.Delivered, readAt(1))
	if _, moved := operations.Advance(cur, operations.ResolveOutcome(ev(attemptA, model.Delivered, withRead(9)))); moved {
		t.Fatal("an operation that recorded a read moved on a later read")
	}
	base := op(model.ProviderAccepted, func(o *model.Operation) { o.ProviderReferences = []model.ExternalRef{ref("m-0")} })
	got, moved := operations.Advance(base, operations.ResolveOutcome(ev(attemptA, model.Delivered, withRefs("m-0", "m-1"))))
	if !moved || !slices.Equal(got.ProviderReferences, []model.ExternalRef{ref("m-0"), ref("m-1")}) {
		t.Fatalf("references = %v, want the operation's and the new one once", got.ProviderReferences)
	}
}

// Ingress queues the operation's worker for evidence the operation does not
// already outrank, and only for a reply or a start.
func TestAdvances(t *testing.T) {
	del := ev(attemptA, model.Delivered)
	acc := ev(attemptA, model.ProviderAccepted)
	for _, tt := range []struct {
		name string
		kind string
		op   model.Operation
		ev   store.AttemptEvidence
		want bool
	}{
		{"delivered on a sent reply", model.ActionMessageReply, op(model.ProviderAccepted), del, true},
		{"delivered on a started conversation", model.ActionConversationStart, op(model.ProviderAccepted), del, true},
		{"failed on a sent reply", model.ActionMessageReply, op(model.ProviderAccepted), ev(attemptA, model.Failed, withFailure(final)), true},
		{"read on a delivered reply", model.ActionMessageReply, op(model.Delivered), ev(attemptA, model.Delivered, withRead(1)), true},
		{"delivered on a failed reply", model.ActionMessageReply, op(model.Failed), del, true},
		{"sent on a sent reply", model.ActionMessageReply, op(model.ProviderAccepted), acc, false},
		{"sent on a delivered reply", model.ActionMessageReply, op(model.Delivered), acc, false},
		{"failed on a delivered reply", model.ActionMessageReply, op(model.Delivered), ev(attemptA, model.Failed, withFailure(final)), false},
		{"delivered on a delivered reply", model.ActionMessageReply, op(model.Delivered), del, false},
		{"read on a read reply", model.ActionMessageReply, op(model.Delivered, readAt(1)), ev(attemptA, model.Delivered, withRead(2)), false},
		{"a retried failure is no verdict", model.ActionMessageReply, op(model.ProviderAccepted), ev(attemptA, model.Failed, withFailure(&model.Error{Code: model.Transient, SafeMessage: "x", Retry: model.RetrySameOperation})), false},
		{"delivered on an edit", model.ActionMessageEdit, op(model.ProviderAccepted), del, false},
		{"delivered on a reaction", model.ActionMessageReact, op(model.ProviderAccepted), del, false},
		{"delivered on a note", model.ActionMessageNote, op(model.ProviderAccepted), del, false},
		{"delivered on an unsettled reply", model.ActionMessageReply, op(model.Unknown), del, false},
		{"delivered on a queued reply", model.ActionMessageReply, op(model.Queued), del, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := operations.Advances(tt.kind, tt.op, tt.ev); got != tt.want {
				t.Fatalf("Advances = %v, want %v", got, tt.want)
			}
		})
	}
}
