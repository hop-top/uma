package services_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"hop.top/uma/internal/services"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// inboundReader serves InboundMessages newest first from msgs, counting
// the messages it hands out; every other Reader method is absent.
type inboundReader struct {
	store.Reader
	msgs []model.Message
	read int
}

func (r *inboundReader) InboundMessages(_ context.Context, _ model.ConversationRef, page model.PageRequest) (model.Page[model.Message], error) {
	start := 0
	if page.After != "" {
		start, _ = strconv.Atoi(string(page.After))
	}
	end := min(start+page.Limit, len(r.msgs))
	r.read += end - start
	pg := model.Page[model.Message]{Items: r.msgs[start:end], Complete: end == len(r.msgs)}
	if !pg.Complete {
		pg.Next = model.Cursor(strconv.Itoa(end))
	}
	return pg, nil
}

// TestNewestLiveInboundSkipsImported: imported messages are passed over,
// page after page, to the newest live one; with a bound the read stops at
// the first message recorded at or before it instead of paging through
// the older history.
func TestNewestLiveInboundSkipsImported(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	msg := func(id string, at int, imported bool) model.Message {
		m := model.Message{Ref: model.MessageRef{ID: model.ID(id)}, RecordedAt: base.Add(time.Duration(at) * time.Second)}
		if imported {
			m.ImportedBy = "desk-1"
		}
		return m
	}
	var msgs []model.Message
	for i := range 60 {
		msgs = append(msgs, msg("imported-"+strconv.Itoa(i), 100-i, true))
	}
	msgs = append(msgs, msg("live", 10, false))
	for i := range 60 {
		msgs = append(msgs, msg("old-"+strconv.Itoa(i), 9-i, false))
	}
	r := &inboundReader{msgs: msgs}
	got, err := services.NewestLiveInbound(t.Context(), r, model.ConversationRef{}, nil)
	if err != nil || got == nil || got.Ref.ID != "live" {
		t.Fatalf("NewestLiveInbound = %+v, %v; want the live message under 60 imported ones", got, err)
	}
	after := base.Add(50 * time.Second)
	r = &inboundReader{msgs: msgs}
	got, err = services.NewestLiveInbound(t.Context(), r, model.ConversationRef{}, &after)
	if err != nil || got != nil {
		t.Fatalf("NewestLiveInbound after %v = %+v, %v; want none recorded after it", after, got, err)
	}
	if r.read > 1+50 {
		t.Fatalf("read %d messages, want the read to stop at the bound", r.read)
	}
	r = &inboundReader{msgs: msgs[60:]}
	got, err = services.NewestLiveInbound(t.Context(), r, model.ConversationRef{}, &after)
	if err != nil || got != nil || r.read != 1 {
		t.Fatalf("NewestLiveInbound without imports = %+v, %v after %d reads; want none in one read", got, err, r.read)
	}
}
