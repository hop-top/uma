package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// searchLabelCases pin Reader.Messages evaluating a label selector against
// the labels the message's conversation holds now: a join to the
// conversation, never a copy on the message.
var searchLabelCases = []storeCase{
	{"MessageConversationLabelFilter", func(t *testing.T, e *env) {
		l1, l2, l3 := e.putLabel(t, "one"), e.putLabel(t, "two"), e.putLabel(t, "three")
		unknown := e.labelRef(t)
		at := time.Unix(1_700_000_000, 0).UTC()
		c0 := e.labelled(t)
		c1 := e.labelled(t, l1)
		c12 := e.labelled(t, l1, l2)
		c123 := e.labelled(t, l1, l2, l3)
		c2 := e.labelled(t, l2)
		// Two messages per conversation, interleaved, so recording order
		// differs from conversation order and a page can split a conversation.
		var m0, m1, m12, m123, m2 [2]model.MessageRef
		for i := range 2 {
			m2[i] = e.putSearchMessage(t, c2, at, textParts("note")...)
			m0[i] = e.putSearchMessage(t, c0, at, textParts("note")...)
			m123[i] = e.putSearchMessage(t, c123, at, textParts("note")...)
			m1[i] = e.putSearchMessage(t, c1, at, textParts("note")...)
			m12[i] = e.putSearchMessage(t, c12, at, textParts("note")...)
		}
		// inOrder returns the messages of the given sets in recording order.
		inOrder := func(sets ...[2]model.MessageRef) []model.MessageRef {
			var out []model.MessageRef
			for i := range 2 {
				for _, group := range [][2]model.MessageRef{m2, m0, m123, m1, m12} {
					for _, s := range sets {
						if s == group {
							out = append(out, group[i])
						}
					}
				}
			}
			return out
		}
		ls := func(refs ...model.LabelRef) []model.LabelRef { return refs }
		for _, tc := range []struct {
			name string
			sel  model.LabelSelector
			want []model.MessageRef
		}{
			{"any one", model.LabelSelector{Any: ls(l1)}, inOrder(m1, m12, m123)},
			{"any many", model.LabelSelector{Any: ls(l1, l2)}, inOrder(m1, m12, m123, m2)},
			{"any held by none", model.LabelSelector{Any: ls(unknown)}, nil},
			{"any with unknown", model.LabelSelector{Any: ls(l3, unknown)}, inOrder(m123)},
			{"all one", model.LabelSelector{All: ls(l2)}, inOrder(m12, m123, m2)},
			{"all many", model.LabelSelector{All: ls(l1, l2)}, inOrder(m12, m123)},
			{"all with unknown", model.LabelSelector{All: ls(l1, unknown)}, nil},
			{"none one", model.LabelSelector{None: ls(l1)}, inOrder(m0, m2)},
			{"none many", model.LabelSelector{None: ls(l1, l2)}, inOrder(m0)},
			{"none unknown", model.LabelSelector{None: ls(unknown)}, inOrder(m0, m1, m12, m123, m2)},
			{"any and none", model.LabelSelector{Any: ls(l2), None: ls(l3)}, inOrder(m12, m2)},
			{"every part", model.LabelSelector{Any: ls(l2), All: ls(l1), None: ls(l3)}, inOrder(m12)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sel := tc.sel
				e.wantSearch(t, model.Filter{Labels: &sel}, tc.want...)
			})
		}
		// The selector composes with the other message filter fields.
		only := model.LabelSelector{Any: ls(l1)}
		e.wantSearch(t, model.Filter{Labels: &only, Conversation: &c12}, m12[0], m12[1])
		e.wantSearch(t, model.Filter{Labels: &only, Conversation: &c2})
		e.wantSearch(t, model.Filter{Labels: &only, Inboxes: []model.InboxRef{e.inbox}, Text: "note"}, inOrder(m1, m12, m123)...)
		e.wantSearch(t, model.Filter{Labels: &only, Text: "absent"})
		// A label change shows at once: no copy on the message goes stale.
		relabel := func(c model.ConversationRef, rev, expected model.Revision, labels ...model.LabelRef) {
			cv := e.conversation(c, rev)
			cv.Labels = sortedLabels(labels...)
			e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, cv, expected) })
		}
		relabel(c0, "r2", "r1", l1)
		e.wantSearch(t, model.Filter{Labels: &only}, inOrder(m0, m1, m12, m123)...)
		relabel(c1, "r2", "r1")
		e.wantSearch(t, model.Filter{Labels: &only}, inOrder(m0, m12, m123)...)
		none := model.LabelSelector{None: ls(l1)}
		e.wantSearch(t, model.Filter{Labels: &none}, inOrder(m1, m2)...)
		// An empty selector is malformed.
		wantCode(t, e.viewErr(func(r store.Reader) error {
			_, err := r.Messages(e.ctx, listOf(model.Filter{Labels: &model.LabelSelector{}}))
			return err
		}), model.Validation)
	}},
}
