package operations

import (
	"slices"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Delivery evidence lattice. Provider evidence about a send reaches the
// engine in any order and more than once: a callback answer, a status
// report that beats it, a repeat, a late "sent" after "delivered". The
// operation's state is therefore not the latest evidence applied but the
// strongest evidence held, ranked:
//
//	provider_accepted < failed < delivered < delivered and read
//
// The accepted and sent values of a provider are one rank: the engine has a
// single state for both. Failed is final for an attempt that never
// delivered, so it outranks acceptance: a "sent" that arrives after a
// final failure does not undo it. Delivery is proof the message arrived, so
// it outranks a failure whichever came first, and read qualifies delivered:
// the operation stays delivered and records when read was first confirmed
// (model.Operation.ReadConfirmedAt). Nothing ever moves down.
//
// The ranking is applied to the evidence the store holds, never to what one
// process remembers, so every process and replica reaches the same state
// from the same evidence, a replay of evidence changes nothing, and a
// state change is announced once: by the unit that wrote it.
//
// A failure counts for the attempt that reported it. It fails the operation
// only when that is the latest attempt and no earlier attempt was applied:
// a retry exists because the earlier attempt was shown not applied, and a
// stale failure report about it must not fail an operation a later attempt
// carried.
const (
	rankNone = iota
	rankAccepted
	rankFailed
	rankDelivered
	rankRead
)

// TracksDelivery reports whether operations of kind follow a delivery
// lifecycle after the provider accepted them: a reply and a conversation
// start, which send a message a recipient receives. Every other operation
// ends when the provider applied it, and delivery evidence for one settles
// it but never moves it on.
func TracksDelivery(kind string) bool {
	return kind == model.ActionMessageReply || kind == model.ActionConversationStart
}

// evidenceRank is the rank of one evidence entry. Only a final failure
// counts as one: a failure the runtime retried is not a verdict on the
// message.
func evidenceRank(ev store.AttemptEvidence) int {
	switch ev.State {
	case model.Queued, model.ProviderAccepted:
		return rankAccepted
	case model.Failed, model.Rejected:
		if ev.Failure != nil && ev.Failure.Retry == model.NoRetry {
			return rankFailed
		}
	case model.Delivered:
		if ev.ReadAt != nil {
			return rankRead
		}
		return rankDelivered
	}
	return rankNone
}

// stateRank is the rank of a settled operation, and rankNone for an
// operation in any other state.
func stateRank(op model.Operation) int {
	switch op.State {
	case model.ProviderAccepted:
		return rankAccepted
	case model.Failed, model.Rejected:
		return rankFailed
	case model.Delivered:
		if op.ReadConfirmedAt != nil {
			return rankRead
		}
		return rankDelivered
	}
	return rankNone
}

// Outcome is the strongest provider evidence of an operation, resolved
// across its attempts. The zero Outcome holds none.
type Outcome struct {
	// State is provider_accepted, delivered, failed or rejected; empty when
	// no evidence shows the provider applied the effect or gave a final
	// failure for the latest attempt.
	State model.DeliveryState
	// References are the provider references of the evidence that counted,
	// in first-seen order.
	References []model.ExternalRef
	// Failure is the final failure of a failed or rejected State.
	Failure *model.Error
	// ReadAt is the read time of the first read confirmation recorded, set
	// with a delivered State.
	ReadAt *time.Time

	rank int
}

// Valid reports whether the outcome holds evidence.
func (o Outcome) Valid() bool { return o.State != "" }

// resolver folds the evidence of an operation, in append order, into its
// Outcome.
type resolver struct {
	order  []model.ID
	best   map[model.ID]store.AttemptEvidence
	rank   map[model.ID]int
	refs   []model.ExternalRef
	readAt *time.Time
}

func newResolver() *resolver {
	return &resolver{best: map[model.ID]store.AttemptEvidence{}, rank: map[model.ID]int{}}
}

func (r *resolver) add(ev store.AttemptEvidence) {
	if _, seen := r.rank[ev.Attempt]; !seen {
		r.order = append(r.order, ev.Attempt)
		r.rank[ev.Attempt] = rankNone
	}
	k := evidenceRank(ev)
	if k == rankNone {
		return
	}
	if k >= r.rank[ev.Attempt] {
		r.rank[ev.Attempt], r.best[ev.Attempt] = k, ev.Clone()
	}
	for _, ref := range ev.ProviderReferences {
		if !slices.Contains(r.refs, ref) {
			r.refs = append(r.refs, ref)
		}
	}
	if k == rankRead && r.readAt == nil {
		at := *ev.ReadAt
		r.readAt = &at
	}
}

func (r *resolver) outcome() Outcome {
	top, accepted := rankNone, false
	for _, a := range r.order {
		switch k := r.rank[a]; {
		case k >= rankDelivered:
			top = max(top, k)
		case k == rankAccepted:
			accepted = true
		}
	}
	refs := slices.Clone(r.refs)
	switch {
	case top >= rankDelivered:
		o := Outcome{State: model.Delivered, References: refs, rank: top}
		if top == rankRead {
			o.ReadAt = r.readAt
		}
		return o
	case len(r.order) == 0:
		return Outcome{}
	}
	if last := r.order[len(r.order)-1]; r.rank[last] == rankFailed && !accepted {
		ev := r.best[last]
		f := *ev.Failure
		return Outcome{State: ev.State, References: refs, Failure: &f, rank: rankFailed}
	}
	if accepted {
		return Outcome{State: model.ProviderAccepted, References: refs, rank: rankAccepted}
	}
	return Outcome{}
}

// Advance moves a settled operation on to the outcome when it outranks the
// operation's state: provider_accepted to failed, delivered or delivered
// and read, failed to delivered, delivered to read (the state stays,
// ReadConfirmedAt is set). It returns the moved operation and whether it
// moved; an operation in any other state, an outcome that does not outrank
// it and one held already change nothing. The caller writes the operation
// and announces the change.
func Advance(op model.Operation, o Outcome) (model.Operation, bool) {
	cur := stateRank(op)
	if cur == rankNone || !o.Valid() || o.rank <= cur {
		return op, false
	}
	next := op.Clone()
	next.ProviderReferences = mergeReferences(next.ProviderReferences, o.References)
	if o.State == model.Delivered {
		next.State, next.Failure = model.Delivered, nil
		if o.rank == rankRead && next.ReadConfirmedAt == nil && o.ReadAt != nil {
			at := o.ReadAt.UTC()
			next.ReadConfirmedAt = &at
		}
		return next, true
	}
	next.State = o.State
	if o.Failure != nil {
		f := *o.Failure
		next.Failure = &f
	}
	return next, true
}

// Advances reports whether evidence ev about an operation of kind could
// move op on (see Advance): op is a settled operation of a
// delivery-tracking kind and ev outranks its state. Ingress asks it to
// decide whether the operation's worker has anything to do for the
// evidence.
func Advances(kind string, op model.Operation, ev store.AttemptEvidence) bool {
	cur := stateRank(op)
	return TracksDelivery(kind) && cur != rankNone && evidenceRank(ev) > cur
}

func mergeReferences(have, add []model.ExternalRef) []model.ExternalRef {
	out := slices.Clone(have)
	for _, r := range add {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}
