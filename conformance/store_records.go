package conformance

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

var recordCases = []storeCase{
	{"RevisionCreateOnly", func(t *testing.T, e *env) {
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), "") })
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.PutChannel(e.ctx, e.channelRecord(ch, "r2"), "")
		}), model.Conflict)
	}},
	{"RevisionStaleExpected", func(t *testing.T, e *env) {
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), "") })
		e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, e.channelRecord(ch, "r2"), "r1") })
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.PutChannel(e.ctx, e.channelRecord(ch, "r3"), "r1")
		}), model.Conflict)
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.PutChannel(e.ctx, e.channelRecord(model.ChannelRef{Tenant: tenantA, ID: e.id(t)}, "r1"), "r0")
		}), model.Conflict)
		e.mustView(t, func(r store.Reader) error {
			got, err := r.GetChannel(e.ctx, ch)
			if err == nil && got.Revision != "r2" {
				t.Errorf("revision = %q, want r2", got.Revision)
			}
			return err
		})
	}},
	{"RevisionRequired", func(t *testing.T, e *env) {
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.PutChannel(e.ctx, e.channelRecord(ch, ""), "")
		}), model.Validation)
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.PutChannel(e.ctx, e.channelRecord(model.ChannelRef{Tenant: tenantA, ID: "not-an-id"}, "r1"), "")
		}), model.Validation)
	}},
	{"FullChainLookup", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		msg := model.MessageRef{Conversation: conv, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, e.message(msg, "r1"), "") })
		other := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.GetMessage(e.ctx, model.MessageRef{Conversation: other, ID: msg.ID})
			wantCode(t, err, model.NotFound)
			_, err = r.GetMessage(e.ctx, msg)
			return err
		})
		// A child under a missing parent is rejected.
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.PutMessage(e.ctx, e.message(model.MessageRef{Conversation: other, ID: e.id(t)}, "r1"), "")
		}), model.NotFound)
	}},
	{"Snapshots", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		msg := model.MessageRef{Conversation: conv, ID: e.id(t)}
		in := e.message(msg, "r1")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, in, "") })
		in.Content.Parts[0].Text = "changed after put"
		e.mustView(t, func(r store.Reader) error {
			got, err := r.GetMessage(e.ctx, msg)
			if err != nil {
				return err
			}
			if got.Content.Parts[0].Text != "hello" {
				t.Errorf("stored text = %q: store retained caller memory", got.Content.Parts[0].Text)
			}
			got.Content.Parts[0].Text = "changed after get"
			again, err := r.GetMessage(e.ctx, msg)
			if err == nil && again.Content.Parts[0].Text != "hello" {
				t.Errorf("stored text = %q: store returned retained memory", again.Content.Parts[0].Text)
			}
			return err
		})
	}},
	{"ListPagesInStableOrder", func(t *testing.T, e *env) {
		const n = 5
		e.mustTx(t, func(tx store.Tx) error {
			for range n - 1 {
				ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
				if err := tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), ""); err != nil {
					return err
				}
			}
			return nil
		})
		var seen []model.ID
		req := list(2)
		for range n + 2 {
			var p model.Page[model.Channel]
			e.mustView(t, func(r store.Reader) error {
				var err error
				p, err = r.Channels(e.ctx, req)
				return err
			})
			for _, c := range p.Items {
				seen = append(seen, c.Ref.ID)
			}
			if p.Complete {
				break
			}
			req.Page.After = p.Next
		}
		if len(seen) != n {
			t.Fatalf("paged %d channels, want %d: %v", len(seen), n, seen)
		}
		dup := map[model.ID]bool{}
		for _, id := range seen {
			if dup[id] {
				t.Fatalf("channel %s returned twice", id)
			}
			dup[id] = true
		}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Channels(e.ctx, model.ListRequest{Page: page(0)})
			wantCode(t, err, model.Validation)
			_, err = r.Channels(e.ctx, model.ListRequest{Filter: model.Filter{Statuses: []string{"open"}}, Page: page(2)})
			wantCode(t, err, model.Validation)
			_, err = r.Channels(e.ctx, model.ListRequest{Filter: model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{{Tenant: tenantA, ID: e.id(t)}}}}, Page: page(2)})
			wantCode(t, err, model.Validation)
			// A read that does not honour a filter field refuses it, never
			// ignores it: only Messages honours Task, only Tasks Identity.
			tk := model.TaskRef{Conversation: model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, ID: e.id(t)}
			_, err = r.Conversations(e.ctx, model.ListRequest{Filter: model.Filter{Task: &tk}, Page: page(2)})
			wantCode(t, err, model.Validation)
			owner := model.IdentityRef{Inbox: e.inbox, ID: e.id(t)}
			_, err = r.Messages(e.ctx, model.ListRequest{Filter: model.Filter{Identity: &owner}, Page: page(2)})
			wantCode(t, err, model.Validation)
			return nil
		})
	}},
	{"ResolveDestination", func(t *testing.T, e *env) {
		e.mustView(t, func(r store.Reader) error {
			got, err := r.ResolveDestination(e.ctx, e.channel, "support@example.test")
			if err == nil && got.Ref != e.inbox {
				t.Errorf("resolved %v, want %v", got.Ref, e.inbox)
			}
			_, nf := r.ResolveDestination(e.ctx, e.channel, "nobody@example.test")
			wantCode(t, nf, model.NotFound)
			return err
		})
		e.mustTx(t, func(tx store.Tx) error {
			return tx.PutInbox(e.ctx, model.Inbox{
				Ref: model.InboxRef{Tenant: tenantA, ID: e.id(t)}, Channel: e.channel,
				Destinations: []string{"support@example.test"}, Revision: "r1",
			}, "")
		})
		e.mustView(t, func(r store.Reader) error {
			_, err := r.ResolveDestination(e.ctx, e.channel, "support@example.test")
			wantCode(t, err, model.Conflict)
			return nil
		})
	}},
	{"MessageMutationFieldsRoundTrip", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		ref := model.MessageRef{Conversation: conv, ID: e.id(t)}
		by := model.IdentityRef{Inbox: e.inbox, ID: e.id(t)}
		op := model.OperationRef{Tenant: tenantA, ID: e.id(t)}
		in, want := e.mutatedMessage(ref, by, op), e.mutatedMessage(ref, by, op)
		e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, in, "") })
		// The store keeps a snapshot: changing the caller's value after the
		// write changes nothing stored.
		in.Reactions[0].By.ID, in.Reactions[2].External[0].ID, in.LastEdit.Operation.ID = e.id(t), "changed", e.id(t)
		e.wantMessage(t, want)
		next := e.mutatedMessage(ref, by, op)
		next.Revision, next.Deleted, next.Content = "r2", true, model.Content{}
		next.Reactions, next.ReactionsComplete = next.Reactions[1:], false
		e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, next, "r1") })
		e.wantMessage(t, next)
	}},
	{"ProtocolTrustBucketsRoundtrip", func(t *testing.T, e *env) {
		// Trust counters: 64 hash buckets "uma.trust.00".."uma.trust.3f", one
		// record each, counted by compare-and-set on the revision.
		key := func(i int) string { return fmt.Sprintf("uma.trust.%02x", i) }
		bucket := func(i, n int) store.ProtocolState {
			return store.ProtocolState{
				Channel: e.channel, Key: key(i),
				Value:    model.Extension{Name: "conformance.trust.bucket", Version: 1, Data: []byte(fmt.Sprintf(`{"b":%d,"n":%d}`, i, n))},
				Revision: model.Revision(fmt.Sprintf("t%d-%d", i, n)),
			}
		}
		e.mustTx(t, func(tx store.Tx) error {
			for i := range 64 {
				if err := tx.PutProtocol(e.ctx, bucket(i, 1), ""); err != nil {
					return err
				}
			}
			return nil
		})
		e.mustTx(t, func(tx store.Tx) error { return tx.PutProtocol(e.ctx, bucket(5, 2), bucket(5, 1).Revision) })
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutProtocol(e.ctx, bucket(5, 3), bucket(5, 1).Revision) }), model.Conflict)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutProtocol(e.ctx, bucket(6, 2), "") }), model.Conflict)
		e.mustView(t, func(r store.Reader) error {
			for i := range 64 {
				want := bucket(i, 1)
				if i == 5 {
					want = bucket(5, 2)
				}
				got, err := r.Protocol(e.ctx, e.channel, key(i))
				if err != nil {
					return err
				}
				if got.Revision != want.Revision || got.Key != want.Key || !reflect.DeepEqual(got.Value, want.Value) {
					t.Errorf("bucket %d = %+v, want %+v", i, got, want)
				}
			}
			_, err := r.Protocol(e.ctx, e.channel, "uma.trust.40")
			wantCode(t, err, model.NotFound)
			return nil
		})
	}},
	{"ProtocolStateCAS", func(t *testing.T, e *env) {
		st := store.ProtocolState{
			Channel: e.channel, Key: "cursor",
			Value: model.Extension{Name: "conformance.chat.cursor", Version: 1, Data: []byte(`{"at":1}`)}, Revision: "p1",
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutProtocol(e.ctx, st, "") })
		st.Revision = "p2"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutProtocol(e.ctx, st, "p0") }), model.Conflict)
		e.mustTx(t, func(tx store.Tx) error { return tx.PutProtocol(e.ctx, st, "p1") })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Protocol(e.ctx, e.channel, "cursor")
			if err == nil && got.Revision != "p2" {
				t.Errorf("revision = %q, want p2", got.Revision)
			}
			_, nf := r.Protocol(e.ctx, e.channel, "other")
			wantCode(t, nf, model.NotFound)
			return err
		})
	}},
}

func (e *env) putConversation(t *testing.T) model.ConversationRef {
	t.Helper()
	ref := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, e.conversation(ref, "r1"), "") })
	return ref
}

func (e *env) conversation(ref model.ConversationRef, rev model.Revision) model.Conversation {
	return model.Conversation{
		Ref: ref, Mode: model.ModePerContact, Status: model.ConversationOpen,
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Revision: rev,
	}
}

func (e *env) message(ref model.MessageRef, rev model.Revision) model.Message {
	return model.Message{
		Ref:        ref,
		Content:    model.Content{Parts: []model.Part{{Kind: "text", Text: "hello"}}},
		Audience:   model.Audience{Visibility: model.Public},
		RecordedAt: time.Unix(1_700_000_000, 0).UTC(),
		Revision:   rev,
	}
}

// mutatedMessage is a message carrying every mutation field: an edit,
// attributed and count-only reactions with provider references, a complete
// reaction set.
// Each call builds fresh memory.
func (e *env) mutatedMessage(ref model.MessageRef, by model.IdentityRef, op model.OperationRef) model.Message {
	at := time.Unix(1_700_000_100, 0).UTC()
	m := e.message(ref, "r1")
	m.LastEdit = &model.Edit{Actor: "conformance-editor", By: &by, At: at, Operation: &op}
	m.Reactions = []model.MessageReaction{
		{Emoji: "👍", By: &by, Count: 1, At: at},
		{Emoji: "👍", Actor: "conformance-agent", Count: 1, At: at},
		{Emoji: "🎉\uFE0F", Count: 4, At: at, External: []model.ExternalRef{
			{Channel: e.channel, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: "reaction-1"},
		}},
	}
	m.ReactionsComplete = true
	return m
}
