package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// Pinned message search fixture: benchSearchMessages messages in
// benchSearchConversations conversations of one inbox, written by position
// i, never random, so every run searches the same data.
//
//   - text: every message holds "update"; one in 10 also "common"; one in
//     1000 also "rare"
//   - time: OccurredAt is benchSearchBase + i seconds
//   - attachments: one in 20 holds an attachment part, "image/png" and
//     every second of those "application/pdf"
//   - labels: the conversations numbered 0 mod 50 hold the label "vip"
//   - nothing is hidden: the policy admits every message, so a page
//     measures the query and the per-message admission, not the scan bound
const (
	benchSearchMessages      = 20_000
	benchSearchConversations = 200
	benchSearchPage          = 50
)

var benchSearchBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// BenchmarkMessageSearch is Messages.List with message search fields on
// PostgreSQL, through the public API: one page of benchSearchPage messages
// per op, from the beginning (first_page) or the walk of the whole result
// (walk). ns/op is wall time per page, or per walk; hits is the messages
// the query lists.
func BenchmarkMessageSearch(b *testing.B) {
	st := benchDatabase(b)
	vip := seedMessageSearch(b, st)
	e, err := uma.New(uma.Config{Store: st, Authorizer: benchAllow(), Clock: benchClock{}, IDs: portstest.NewSequenceIDs(0xb5)})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = e.Close(context.Background()) })
	cl, err := e.For(model.Principal{Tenant: benchTenant, Actor: "bench-agent"})
	if err != nil {
		b.Fatal(err)
	}
	yes := true
	since := benchSearchBase.Add(10_000 * time.Second)
	before := since.Add(2_000 * time.Second)
	cases := []struct {
		name string
		f    model.Filter
	}{
		{"text_rare", model.Filter{Text: "rare"}},
		{"text_common", model.Filter{Text: "common"}},
		{"text_two_tokens", model.Filter{Text: "common update"}},
		{"text_and_dates", model.Filter{Text: "common", Since: &since, Before: &before}},
		{"has_attachment", model.Filter{Attachments: &model.AttachmentFilter{Has: &yes}}},
		{"media_type", model.Filter{Attachments: &model.AttachmentFilter{MediaTypes: []string{"application/pdf"}}}},
		{"label", model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{vip}}}},
		{"label_and_text", model.Filter{Text: "rare", Labels: &model.LabelSelector{Any: []model.LabelRef{vip}}}},
		{"no_search", model.Filter{}},
	}
	list := func(f model.Filter, after model.Cursor) model.Page[model.Message] {
		p, err := cl.Messages().List(b.Context(), model.ListRequest{
			Call: model.Call{Correlation: "bench"}, Filter: f, Page: model.PageRequest{Limit: benchSearchPage, After: after},
		})
		if err != nil {
			b.Fatalf("Messages.List: %v", err)
		}
		return p
	}
	for _, c := range cases {
		b.Run("first_page/"+c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				list(c.f, "")
			}
		})
	}
	for _, c := range cases[:2] {
		b.Run("walk/"+c.name, func(b *testing.B) {
			hits := 0
			for b.Loop() {
				hits = 0
				for after := model.Cursor(""); ; {
					p := list(c.f, after)
					hits += len(p.Items)
					if p.Complete {
						break
					}
					after = p.Next
				}
			}
			b.ReportMetric(float64(hits), "hits")
		})
	}
}

// seedMessageSearch writes the pinned fixture and returns its label.
func seedMessageSearch(b *testing.B, st *Store) model.LabelRef {
	b.Helper()
	ctx := b.Context()
	ch := model.ChannelRef{Tenant: benchTenant, ID: benchID(0x5e, 1)}
	in := model.InboxRef{Tenant: benchTenant, ID: benchID(0x5e, 2)}
	vip := model.LabelRef{Tenant: benchTenant, ID: benchID(0x5e, 3)}
	png := model.AttachmentRef{Inbox: in, ID: benchID(0x5e, 4)}
	pdf := model.AttachmentRef{Inbox: in, ID: benchID(0x5e, 5)}
	conv := func(c int) model.ConversationRef { return model.ConversationRef{Inbox: in, ID: benchID(0x5c, c)} }
	attachment := func(ref model.AttachmentRef, mediaType string) store.AttachmentRecord {
		return store.AttachmentRecord{State: "stored", Attachment: model.Attachment{Ref: ref, Revision: "a1", Metadata: model.AttachmentMetadata{
			Name: "file", MediaType: mediaType, Size: 1, Audience: model.Audience{Visibility: model.Public},
		}}}
	}
	if err := st.Transact(ctx, benchTenant, func(tx store.Tx) error {
		if err := firstErr(
			tx.PutChannel(ctx, model.Channel{Ref: ch, Type: benchSendType, Configuration: model.Extension{Name: string(benchSendType) + ".config", Version: 1}, Revision: "ch1"}, ""),
			tx.PutInbox(ctx, model.Inbox{Ref: in, Channel: ch, Name: "bench", Revision: "in1"}, ""),
			tx.PutLabel(ctx, model.Label{Ref: vip, Name: "vip", Revision: "l1"}, ""),
			tx.PutAttachment(ctx, attachment(png, "image/png"), ""),
			tx.PutAttachment(ctx, attachment(pdf, "application/pdf"), ""),
		); err != nil {
			return err
		}
		for c := range benchSearchConversations {
			cv := model.Conversation{Ref: conv(c), Mode: model.ModePerRoom, Status: model.ConversationOpen, CreatedAt: benchSearchBase, Revision: "cv1"}
			if c%50 == 0 {
				cv.Labels = []model.LabelRef{vip}
			}
			if err := tx.PutConversation(ctx, cv, ""); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatalf("seed: %v", err)
	}
	for lo := 0; lo < benchSearchMessages; lo += 500 {
		if err := st.Transact(ctx, benchTenant, func(tx store.Tx) error {
			for i := lo; i < lo+500; i++ {
				text := "update"
				if i%10 == 0 {
					text += " common"
				}
				if i%1000 == 0 {
					text += " rare"
				}
				parts := []model.Part{{Kind: model.PartText, Text: text}}
				if i%20 == 0 {
					a := png
					if i%40 == 0 {
						a = pdf
					}
					parts = append(parts, model.Part{Kind: model.PartAttachment, Attachment: &a})
				}
				m := model.Message{
					Ref:      model.MessageRef{Conversation: conv(i % benchSearchConversations), ID: benchID(0x5d, i)},
					Audience: model.Audience{Visibility: model.Public}, Content: model.Content{Parts: parts},
					OccurredAt: benchSearchBase.Add(time.Duration(i) * time.Second), RecordedAt: benchSearchBase, Revision: "m1",
				}
				if err := tx.PutMessage(ctx, m, ""); err != nil {
					return fmt.Errorf("message %d: %w", i, err)
				}
			}
			return nil
		}); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	return vip
}
