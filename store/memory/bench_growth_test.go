package memory

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// growthTenant holds the pinned fixture of the growth benchmarks.
const growthTenant model.TenantID = "growth-tenant"

// growthBatch is how many records the fixture writes per unit. Every unit
// copies a table once, so the batch bounds the fixture's build cost.
const growthBatch = 1000

// errRollback ends a measured unit after its write, so nothing commits and
// the fixture keeps its size across iterations.
var errRollback = errors.New("rollback")

// growthStore builds a store holding one channel, one inbox, conversations
// conversations and messages messages spread evenly over them.
func growthStore(tb testing.TB, conversations, messages int) (*Store, model.InboxRef, []model.ConversationRef) {
	tb.Helper()
	ctx := context.Background()
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	s := New(Options{Clock: clock})
	ch := model.ChannelRef{Tenant: growthTenant, ID: benchID(10, 0)}
	in := model.InboxRef{Tenant: growthTenant, ID: benchID(11, 0)}
	convs := make([]model.ConversationRef, conversations)
	for i := range convs {
		convs[i] = model.ConversationRef{Inbox: in, ID: benchID(12, i)}
	}
	if err := s.Transact(ctx, growthTenant, func(tx store.Tx) error {
		if err := tx.PutChannel(ctx, model.Channel{Ref: ch, Revision: "1"}, ""); err != nil {
			return err
		}
		return tx.PutInbox(ctx, model.Inbox{Ref: in, Channel: ch, Revision: "1"}, "")
	}); err != nil {
		tb.Fatalf("fixture inbox: %v", err)
	}
	for lo := 0; lo < conversations; lo += growthBatch {
		if err := s.Transact(ctx, growthTenant, func(tx store.Tx) error {
			for _, c := range convs[lo:min(lo+growthBatch, conversations)] {
				if err := tx.PutConversation(ctx, growthConversation(c), ""); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			tb.Fatalf("fixture conversations: %v", err)
		}
	}
	for lo := 0; lo < messages; lo += growthBatch {
		if err := s.Transact(ctx, growthTenant, func(tx store.Tx) error {
			for i := lo; i < min(lo+growthBatch, messages); i++ {
				if err := tx.PutMessage(ctx, growthMessage(convs[i%conversations], i), ""); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			tb.Fatalf("fixture messages: %v", err)
		}
	}
	return s, in, convs
}

func growthConversation(c model.ConversationRef) model.Conversation {
	return model.Conversation{Ref: c, Mode: model.ModePerContact, Status: "open", Revision: "1"}
}

// growthMessage is a short inbound text message with one provider
// reference, about the size of a chat message.
func growthMessage(c model.ConversationRef, i int) model.Message {
	return model.Message{
		Ref:    model.MessageRef{Conversation: c, ID: benchID(13, i)},
		Author: model.Author{Kind: "user", Assurance: "verified"},
		Content: model.Content{Parts: []model.Part{{
			Kind: model.PartText, Text: fmt.Sprintf("benchmark message %d, a short line of chat text", i),
		}}},
		External: []model.ExternalRef{{
			Channel: model.ChannelRef{Tenant: growthTenant, ID: benchID(10, 0)},
			Kind:    "message", ID: fmt.Sprintf("provider-%d", i),
		}},
		OccurredAt: time.Unix(1_700_000_000, 0).UTC(), RecordedAt: time.Unix(1_700_000_000, 0).UTC(),
		Revision: "1",
	}
}

// heapInUse is the live heap after a collection.
func heapInUse() uint64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// BenchmarkWriteGrowth measures what the first write of a unit costs as the
// tenant's tables grow, on a fixture of a pinned size. The unit's write is
// rolled back, so the fixture does not grow between iterations and each
// ns/op is the cost of one write against exactly that many records; a
// commit adds one mutex acquisition and a pointer swap.
//
// put_message writes a new message into a tenant with messages messages in
// messages/10 conversations. put_conversation writes a new conversation
// into a tenant with conversations conversations and one message in each.
// heap-B/record is the live heap of the built fixture per record it holds.
func BenchmarkWriteGrowth(b *testing.B) {
	ctx := context.Background()
	b.Run("put_message", func(b *testing.B) {
		for _, n := range []int{100, 1_000, 10_000, 100_000} {
			b.Run(fmt.Sprintf("messages=%d", n), func(b *testing.B) {
				before := heapInUse()
				s, _, convs := growthStore(b, max(n/10, 1), n)
				heap := heapInUse() - before
				next := growthMessage(convs[0], n)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					err := s.Transact(ctx, growthTenant, func(tx store.Tx) error {
						if err := tx.PutMessage(ctx, next, ""); err != nil {
							return err
						}
						return errRollback
					})
					if !errors.Is(err, errRollback) {
						b.Fatalf("Transact: %v", err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(heap)/float64(n), "heap-B/record")
				runtime.KeepAlive(s)
			})
		}
	})
	b.Run("put_conversation", func(b *testing.B) {
		for _, n := range []int{100, 1_000, 10_000, 100_000} {
			b.Run(fmt.Sprintf("conversations=%d", n), func(b *testing.B) {
				s, in, _ := growthStore(b, n, n)
				next := growthConversation(model.ConversationRef{Inbox: in, ID: benchID(12, n)})
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					err := s.Transact(ctx, growthTenant, func(tx store.Tx) error {
						if err := tx.PutConversation(ctx, next, ""); err != nil {
							return err
						}
						return errRollback
					})
					if !errors.Is(err, errRollback) {
						b.Fatalf("Transact: %v", err)
					}
				}
				b.StopTimer()
				runtime.KeepAlive(s)
			})
		}
	})
}

// BenchmarkReadGrowth measures one page read as the tenant's tables grow.
// Reads copy nothing, but a list builds and sorts the whole table before it
// cuts a page, so even the first page of 50 costs time proportional to the
// messages held. first_page reads the first page of one conversation;
// last_page reads the page after the cursor of the final message.
func BenchmarkReadGrowth(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{100, 1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("messages=%d", n), func(b *testing.B) {
			s, _, convs := growthStore(b, max(n/10, 1), n)
			req := model.ListRequest{
				Filter: model.Filter{Conversation: &convs[0]},
				Page:   model.PageRequest{Limit: 50},
			}
			var tail model.Cursor
			if err := s.View(ctx, growthTenant, func(r store.Reader) error {
				all := req
				all.Filter = model.Filter{}
				all.Page.Limit = 1000
				for {
					p, err := r.Messages(ctx, all)
					if err != nil {
						return err
					}
					tail = p.Next
					if p.Complete {
						return nil
					}
					all.Page.After = p.Next
				}
			}); err != nil {
				b.Fatal(err)
			}
			for _, tc := range []struct {
				name string
				req  model.ListRequest
			}{
				{"first_page", req},
				{"last_page", model.ListRequest{Page: model.PageRequest{Limit: 50, After: tail}}},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						if err := s.View(ctx, growthTenant, func(r store.Reader) error {
							_, err := r.Messages(ctx, tc.req)
							return err
						}); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

// TestGrowthFixture pins the fixture the growth benchmarks measure: a
// benchmark over a store of the wrong size would report the wrong cost.
func TestGrowthFixture(t *testing.T) {
	ctx := context.Background()
	s, _, convs := growthStore(t, 12, 2500)
	if len(convs) != 12 {
		t.Fatalf("conversations: got %d, want 12", len(convs))
	}
	if err := s.View(ctx, growthTenant, func(r store.Reader) error {
		got := 0
		req := model.ListRequest{Page: model.PageRequest{Limit: 1000}}
		for {
			p, err := r.Messages(ctx, req)
			if err != nil {
				return err
			}
			got += len(p.Items)
			if p.Complete {
				break
			}
			req.Page.After = p.Next
		}
		if got != 2500 {
			t.Errorf("messages: got %d, want 2500", got)
		}
		p, err := r.Messages(ctx, model.ListRequest{
			Filter: model.Filter{Conversation: &convs[0]}, Page: model.PageRequest{Limit: 1000},
		})
		if err != nil {
			return err
		}
		if want := 209; len(p.Items) != want { // 2500 over 12: 208 or 209, conversation 0 holds 209
			t.Errorf("conversation 0 messages: got %d, want %d", len(p.Items), want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
