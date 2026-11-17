package fakeprovider_test

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A window of the fake provider's history is imported into its room's
// conversation once: a second run imports nothing, and a message older
// than the window is never asked for.
func TestRuntimeHistoryImportedOnce(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	rooms := model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000d1"}
	err := h.store.Transact(h.ctx, tenant, func(tx store.Tx) error {
		return tx.PutInbox(h.ctx, model.Inbox{
			Ref: rooms, Channel: chanA, Name: "rooms", Destinations: []string{"rooms"},
			Selection: model.Selection{Mode: model.ModePerRoom}, Revision: "in1",
		}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	room := model.ExternalRef{Channel: chanA, Namespace: fakeprovider.Namespace, Kind: model.RefKindRoom, ID: "room-9"}
	msg := func(id string, at time.Time) model.Envelope {
		return model.Envelope{
			EventID: "history-" + id, MessageID: id, Destination: "rooms", Kind: model.EnvelopeMessage,
			Sender:    model.ExternalRef{Channel: chanA, Namespace: fakeprovider.Namespace, Kind: "user", ID: "carol"},
			Assurance: "verified", References: []model.ExternalRef{room},
			Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "old " + id}}},
			Audience: model.Audience{Visibility: model.Public}, OccurredAt: at,
		}
	}
	since := h.clock.Now().Add(-24 * time.Hour)
	script := h.factory.Script(chanA)
	script.HoldHistory(msg("h-old", since.Add(-time.Hour)), msg("h-1", since.Add(time.Hour)), msg("h-2", since.Add(2*time.Hour)), msg("h-3", since.Add(3*time.Hour)))
	req := uma.ImportRequest{Requester: actor, Channel: &chanA, Since: since, Limit: 10}
	rep, err := h.engine.Runtime().Import(h.ctx, runtimeCall(), req)
	if err != nil || rep != (uma.ImportReport{Imported: 3, Complete: true}) {
		t.Fatalf("first import = %+v, %v; want the 3 messages in the window", rep, err)
	}
	again, err := h.engine.Runtime().Import(h.ctx, runtimeCall(), req)
	if err != nil || again != (uma.ImportReport{Skipped: 3, Complete: true}) {
		t.Fatalf("second import = %+v, %v; want nothing imported", again, err)
	}
	var ids []string
	err = h.store.View(h.ctx, tenant, func(r store.Reader) error {
		p, err := r.Messages(h.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{rooms}}, Page: model.PageRequest{Limit: 100}})
		for _, m := range p.Items {
			ids = append(ids, m.External[0].ID)
		}
		return err
	})
	if err != nil || !slices.Equal(ids, []string{"h-1", "h-2", "h-3"}) {
		t.Fatalf("stored messages = %v, %v; want h-1, h-2, h-3 once each", ids, err)
	}
	for _, r := range script.HistoryReads() {
		if !r.Since.Equal(since) || r.Route != nil {
			t.Fatalf("history read %+v, want the channel's window", r)
		}
	}
}
