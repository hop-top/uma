package fakeprovider

import (
	"context"
	"slices"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// HoldHistory appends envelopes to the provider's message history, which
// the full variant serves from History oldest first, as given.
func (s *Script) HoldHistory(envs ...model.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range envs {
		s.history = append(s.history, e.Clone())
	}
}

// HistoryReads returns the history requests the provider answered.
func (s *Script) HistoryReads() []adapter.HistoryRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.historyReads)
}

// historyPage answers one history request: the held envelopes occurring at
// or after Since and before Until (zero is open), those referencing Route (or sent by it) when it is set,
// from the cursor's position, at most Limit of them. The cursor is the
// position in that window.
func (s *Script) historyPage(r adapter.HistoryRequest) (adapter.HistoryPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyReads = append(s.historyReads, r)
	var window []model.Envelope
	for _, e := range s.history {
		if e.OccurredAt.Before(r.Since) || (!r.Until.IsZero() && !e.OccurredAt.Before(r.Until)) {
			continue
		}
		if r.Route != nil && e.Sender != *r.Route && !slices.Contains(e.References, *r.Route) {
			continue
		}
		window = append(window, e.Clone())
	}
	start := 0
	if r.Cursor != "" {
		n, err := strconv.Atoi(r.Cursor)
		if err != nil || n < 0 || n > len(window) {
			return adapter.HistoryPage{}, model.NewError(model.Validation, "fake provider: unknown history cursor")
		}
		start = n
	}
	end := min(start+max(r.Limit, 1), len(window))
	page := adapter.HistoryPage{Envelopes: window[start:end]}
	if end < len(window) {
		page.Next = strconv.Itoa(end)
	}
	return page, nil
}

// History serves a page of the script's held history.
func (x *Full) History(ctx context.Context, r adapter.HistoryRequest) (adapter.HistoryPage, error) {
	if err := x.i.ready(ctx, model.CapabilityHistory); err != nil {
		return adapter.HistoryPage{}, err
	}
	if err := x.i.authenticate(ctx); err != nil {
		return adapter.HistoryPage{}, err
	}
	return x.i.script.historyPage(r)
}
