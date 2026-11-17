package operations

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Attempt evidence is append-only. An attempt records one evidence entry
// with state model.Unknown before its provider call, then one per answer:
// the provider outcome, a reconciliation result, or a late receipt from a
// worker that has since lost its lease. The latest entry of an attempt
// decides whether it is resolved.

// evidencePage bounds one Attempts read while loading an operation's history.
const evidencePage = 100

// History summarizes an operation's attempt evidence.
type History struct {
	// Attempts counts distinct attempts, each a provider effect issued or
	// about to be issued.
	Attempts int
	// Latest is the most recently started attempt and Last its latest
	// evidence.
	Latest model.ID
	Last   store.AttemptEvidence
	// LastCount counts the latest attempt's evidence entries.
	LastCount int
	// LatestAt is when the latest attempt was recorded, before its provider
	// call.
	LatestAt time.Time
	// Outcome is the strongest evidence of any attempt, ranked (see
	// Outcome). A late receipt from a stale owner lands here.
	Outcome Outcome
}

// Unresolved reports whether the latest attempt has no answer yet: it may
// or may not have been applied, so it must be reconciled before any retry.
func (h History) Unresolved() bool {
	return h.Latest != "" && h.Last.State == model.Unknown
}

// AppliedState reports whether evidence state s shows the provider applied
// the effect. A provider-side queue is provider acceptance.
func AppliedState(s model.DeliveryState) bool {
	switch s {
	case model.Queued, model.ProviderAccepted, model.Delivered:
		return true
	}
	return false
}

// LoadHistory reads every attempt evidence entry of op.
func LoadHistory(ctx context.Context, r store.Reader, op model.OperationRef) (History, error) {
	var h History
	latest := map[model.ID]store.AttemptEvidence{}
	counts := map[model.ID]int{}
	started := map[model.ID]time.Time{}
	res := newResolver()
	after := model.Cursor("")
	for {
		p, err := r.Attempts(ctx, op, model.PageRequest{Limit: evidencePage, After: after})
		if err != nil {
			return h, err
		}
		for _, ev := range p.Items {
			if _, seen := latest[ev.Attempt]; !seen {
				h.Attempts++
				h.Latest = ev.Attempt
				started[ev.Attempt] = ev.ObservedAt
			}
			latest[ev.Attempt] = ev
			counts[ev.Attempt]++
			res.add(ev)
		}
		if p.Complete || p.Next == "" || p.Next == after {
			break
		}
		after = p.Next
	}
	h.Outcome = res.outcome()
	if h.Latest != "" {
		h.Last, h.LastCount, h.LatestAt = latest[h.Latest], counts[h.Latest], started[h.Latest]
	}
	return h, nil
}

// AppendEvidence records attempt evidence in its own unit, so it survives
// even when the authoritative update that follows is fenced out.
func AppendEvidence(ctx context.Context, e *engine.Engine, ev store.AttemptEvidence) error {
	return Transact(ctx, e, ev.Operation.Tenant, func(tx store.Tx) error {
		return tx.AppendAttemptEvidence(ctx, ev)
	})
}

// deliveryPayload is the version 1 delivery.updated payload.
type deliveryPayload struct {
	Operation          model.OperationRef  `json:"operation"`
	State              model.DeliveryState `json:"state"`
	PreviousState      model.DeliveryState `json:"previous_state"`
	ProviderReferences []model.ExternalRef `json:"provider_references,omitempty"`
	ReadConfirmedAt    *time.Time          `json:"read_confirmed_at,omitempty"`
	Failure            *deliveryFailure    `json:"failure,omitempty"`
	Trigger            string              `json:"trigger"`
}

type deliveryFailure struct {
	Code        model.ErrorCode   `json:"code"`
	Retry       model.RetryAdvice `json:"retry"`
	SafeMessage string            `json:"safe_message"`
}

// DeliveryEvent returns the delivery.updated event of a state transition.
func DeliveryEvent(id model.ID, rec store.OperationRecord, prev model.DeliveryState, next model.Operation,
	audience model.Audience, trigger string, at time.Time,
) (model.Event, error) {
	p := deliveryPayload{
		Operation: next.Ref, State: next.State, PreviousState: prev,
		ProviderReferences: slices.Clone(next.ProviderReferences),
		ReadConfirmedAt:    next.ReadConfirmedAt, Trigger: trigger,
	}
	if f := next.Failure; f != nil {
		p.Failure = &deliveryFailure{Code: f.Code, Retry: f.Retry, SafeMessage: f.SafeMessage}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return model.Event{}, model.NewError(model.Permanent, "delivery event cannot be encoded")
	}
	return model.Event{
		Version: model.EventEnvelopeVersion, ID: id, Kind: model.EventDeliveryUpdated,
		Tenant: next.Ref.Tenant, Resource: next.Ref.Ref(), Actor: rec.Key.Caller,
		OccurredAt: at, RecordedAt: at, Audience: audience, Revision: next.Revision,
		Payload: model.Extension{Name: model.SchemaDeliveryUpdated, Version: model.PayloadVersion1, Data: data},
	}, nil
}
