package conformance

import (
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// inboundMessagesCases cover Reader.InboundMessages: the live inbound messages of
// one conversation, newest first by recording order, paged by a cursor that
// continues strictly older.
var inboundMessagesCases = []storeCase{
	{"InboundMessagesNewestFirst", func(t *testing.T, e *env) {
		conv, other := e.putConversation(t), e.putConversation(t)
		put := func(c model.ConversationRef, shape func(*model.Message)) model.MessageRef {
			t.Helper()
			m := e.message(model.MessageRef{Conversation: c, ID: e.id(t)}, "m1")
			if shape != nil {
				shape(&m)
			}
			e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, m, "") })
			return m.Ref
		}
		echoed := put(conv, nil)
		first := put(conv, nil)
		put(conv, func(m *model.Message) { m.Origin = &model.Origin{Actor: "agent-1"} })
		put(other, nil)
		second := put(conv, nil)
		put(conv, func(m *model.Message) { m.EchoOf = &echoed })
		redacted := put(conv, nil)
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Message: &redacted, Expected: "m1", Revision: "m2"})
			return err
		})
		put(conv, func(m *model.Message) { m.Deleted = true })
		put(other, nil)
		internal := put(conv, func(m *model.Message) { m.Audience = model.Audience{Visibility: model.Internal} })
		last := put(conv, nil)

		// Audience is not a store concern: an internal inbound message is listed.
		want := []model.MessageRef{last, internal, second, first, echoed}
		if got := e.inboundWalk(t, conv, 2); !slices.Equal(got, want) {
			t.Errorf("InboundMessages walk = %v, want %v", got, want)
		}
		if got := e.inboundWalk(t, conv, 100); !slices.Equal(got, want) {
			t.Errorf("InboundMessages one page = %v, want %v", got, want)
		}
		// A page holding exactly the remaining messages is complete.
		if p := e.inboundPage(t, conv, "", len(want)); !p.Complete || len(p.Items) != len(want) {
			t.Errorf("InboundMessages page of %d = %d items complete=%v, want all and complete", len(want), len(p.Items), p.Complete)
		}
		if p := e.inboundPage(t, conv, "", len(want)-1); p.Complete || len(p.Items) != len(want)-1 {
			t.Errorf("InboundMessages short page = %d items complete=%v, want incomplete", len(p.Items), p.Complete)
		}

		// A rewrite keeps the message's place; a rewrite that makes it
		// redacted or an echo of another drops it.
		m := e.getMessage(t, second)
		m.Content.Parts[0].Text, m.Revision = "edited", "m2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, m, "m1") })
		if got := e.inboundWalk(t, conv, 2); !slices.Equal(got, want) {
			t.Errorf("after an in-place rewrite = %v, want %v", got, want)
		}
		m.Redacted, m.Revision = true, "m3"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, m, "m2") })
		want = slices.DeleteFunc(slices.Clone(want), func(r model.MessageRef) bool { return r == second })
		if got := e.inboundWalk(t, conv, 2); !slices.Equal(got, want) {
			t.Errorf("after a redacting rewrite = %v, want %v", got, want)
		}

		// A message recorded after a cursor was issued is not in the
		// pages that follow it, and heads the next walk.
		p := e.inboundPage(t, conv, "", 1)
		newest := put(conv, nil)
		rest := e.inboundPage(t, conv, p.Next, 100)
		if got := messageRefs(rest.Items); !slices.Equal(got, want[1:]) || !rest.Complete {
			t.Errorf("page after the cursor = %v complete=%v, want %v", got, rest.Complete, want[1:])
		}
		if got := e.inboundWalk(t, conv, 3); !slices.Equal(got, append([]model.MessageRef{newest}, want...)) {
			t.Errorf("walk after a new message = %v, want it first", got)
		}

		// No inbound message, or no conversation: an empty complete page.
		none := e.putConversation(t)
		put(none, func(m *model.Message) { m.Origin = &model.Origin{Actor: "agent-1"} })
		for _, c := range []model.ConversationRef{none, {Inbox: e.inbox, ID: e.id(t)}} {
			if p := e.inboundPage(t, c, "", 5); len(p.Items) != 0 || !p.Complete {
				t.Errorf("InboundMessages of %v = %d items complete=%v, want none and complete", c, len(p.Items), p.Complete)
			}
		}
	}},
	{"InboundMessagesRejectsBadRequests", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		for name, page := range map[string]model.PageRequest{
			"zero limit":     {},
			"negative limit": {Limit: -1},
			"bad cursor":     {Limit: 1, After: "not-a-cursor"},
		} {
			t.Run(name, func(t *testing.T) {
				err := e.s.View(e.ctx, tenantA, func(r store.Reader) error {
					_, err := r.InboundMessages(e.ctx, conv, page)
					return err
				})
				wantCode(t, err, model.Validation)
			})
		}
		err := e.s.View(e.ctx, tenantA, func(r store.Reader) error {
			_, err := r.InboundMessages(e.ctx, model.ConversationRef{}, model.PageRequest{Limit: 1})
			return err
		})
		wantCode(t, err, model.Validation)
		e.wantForeign(t, func(r store.Reader) error {
			_, err := r.InboundMessages(e.ctx, conv, model.PageRequest{Limit: 1})
			return err
		})
	}},
}

func (e *env) inboundPage(t *testing.T, conv model.ConversationRef, after model.Cursor, limit int) model.Page[model.Message] {
	t.Helper()
	var p model.Page[model.Message]
	e.mustView(t, func(r store.Reader) (err error) {
		p, err = r.InboundMessages(e.ctx, conv, model.PageRequest{Limit: limit, After: after})
		return err
	})
	return p
}

// inboundWalk follows InboundMessages to its end, limit at a time.
func (e *env) inboundWalk(t *testing.T, conv model.ConversationRef, limit int) []model.MessageRef {
	t.Helper()
	return pageAll(t, func(after model.Cursor) (model.Page[model.Message], error) {
		return e.inboundPage(t, conv, after, limit), nil
	}, func(v model.Message) model.MessageRef { return v.Ref })
}

func (e *env) getMessage(t *testing.T, ref model.MessageRef) (m model.Message) {
	t.Helper()
	e.mustView(t, func(r store.Reader) (err error) {
		m, err = r.GetMessage(e.ctx, ref)
		return err
	})
	return m
}

func messageRefs(ms []model.Message) []model.MessageRef {
	out := make([]model.MessageRef, len(ms))
	for i, m := range ms {
		out[i] = m.Ref
	}
	return out
}
