package conformance

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// streamsOfCases cover Reader.StreamsOf: the conversation's streams that
// are not terminal, ordered by ID, every field round-tripped, never another
// conversation's and never a foreign tenant's.
var streamsOfCases = []storeCase{
	{"StreamsOfListsNonTerminalStreams", func(t *testing.T, e *env) {
		conv, other := e.putConversation(t), e.putConversation(t)
		var want []model.Stream
		for i, state := range []string{
			model.StreamOpening, model.StreamFinished, model.StreamOpen, model.StreamCancelled,
			model.StreamEnding, model.StreamPartial, model.StreamFailed, model.StreamUnknown,
		} {
			st := model.Stream{
				Ref: model.StreamRef{Conversation: conv, ID: e.id(t)}, Mode: model.StreamEdit, State: state,
				Audience:           model.Audience{Visibility: model.Public},
				ProviderReferences: []model.ExternalRef{e.externalRef("message", "pm-"+string(rune('a'+i)))},
				Revision:           "s1",
			}
			e.mustTx(t, func(tx store.Tx) error { return tx.PutStream(e.ctx, st, "") })
			if state == model.StreamOpening || state == model.StreamOpen || state == model.StreamEnding {
				want = append(want, st)
			}
		}
		elsewhere := model.Stream{
			Ref: model.StreamRef{Conversation: other, ID: e.id(t)}, Mode: model.StreamNative, State: model.StreamOpen,
			Audience: model.Audience{Visibility: model.Public}, Revision: "s1",
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutStream(e.ctx, elsewhere, "") })
		slices.SortFunc(want, func(a, b model.Stream) int { return strings.Compare(string(a.Ref.ID), string(b.Ref.ID)) })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.StreamsOf(e.ctx, conv)
			if err == nil && !reflect.DeepEqual(got, want) {
				t.Errorf("StreamsOf = %+v, want the non-terminal streams %+v", got, want)
			}
			return err
		})
		// Ending a stream drops it from the set.
		ended := want[0].Clone()
		ended.State, ended.Revision = model.StreamFinished, "s2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutStream(e.ctx, ended, "s1") })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.StreamsOf(e.ctx, conv)
			if err == nil && (len(got) != len(want)-1 || slices.ContainsFunc(got, func(s model.Stream) bool { return s.Ref == ended.Ref })) {
				t.Errorf("StreamsOf after the end = %+v, want it gone", got)
			}
			return err
		})
		e.mustView(t, func(r store.Reader) error {
			got, err := r.StreamsOf(e.ctx, model.ConversationRef{Inbox: e.inbox, ID: e.id(t)})
			if err == nil && len(got) != 0 {
				t.Errorf("StreamsOf of a conversation without streams = %+v", got)
			}
			return err
		})
		e.wantForeign(t, func(r store.Reader) error { _, err := r.StreamsOf(e.ctx, conv); return err })
	}},
}
