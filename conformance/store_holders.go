package conformance

import (
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// holderCases cover Reader.MessagesByAttachment: the messages whose
// current parts name an attachment.
var holderCases = []storeCase{
	{"MessagesByAttachmentFindsHolders", func(t *testing.T, e *env) {
		att := model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)}
		other := model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)}
		in2 := model.InboxRef{Tenant: tenantA, ID: e.id(t)}
		conv2 := model.ConversationRef{Inbox: in2, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error {
			if err := tx.PutInbox(e.ctx, model.Inbox{Ref: in2, Channel: e.channel, Name: "other", Revision: "r1"}, ""); err != nil {
				return err
			}
			return tx.PutConversation(e.ctx, e.conversation(conv2, "r1"), "")
		})
		first := e.putHolder(t, e.putConversation(t), att, att)
		e.putHolder(t, e.putConversation(t), other)
		second := e.putHolder(t, conv2, other, att)
		e.mustView(t, func(r store.Reader) error {
			var got []model.ID
			page := model.PageRequest{Limit: 1}
			for {
				p, err := r.MessagesByAttachment(e.ctx, att, page)
				if err != nil {
					return err
				}
				got = append(got, messageIDs(p.Items)...)
				if p.Complete {
					break
				}
				page.After = p.Next
			}
			if !slices.Equal(got, []model.ID{first.ID, second.ID}) {
				t.Errorf("holders = %v, want [%s %s] once each in recording order", got, first.ID, second.ID)
			}
			none, err := r.MessagesByAttachment(e.ctx, model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)}, model.PageRequest{Limit: 10})
			if err == nil && (len(none.Items) != 0 || !none.Complete) {
				t.Errorf("an attachment no message names has holders %+v", none)
			}
			return err
		})
	}},
	{"MessagesByAttachmentFollowsRewriteAndRedact", func(t *testing.T, e *env) {
		att := model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)}
		conv := e.putConversation(t)
		kept := e.putHolder(t, conv, att)
		dropped := e.putHolder(t, conv, att)
		redacted := e.putHolder(t, conv, att)
		// A rolled-back rewrite leaves the holder.
		_ = e.txErr(func(tx store.Tx) error {
			if err := tx.PutMessage(e.ctx, e.message(dropped, "r2"), "r1"); err != nil {
				return err
			}
			return errAbort
		})
		e.requireHolders(t, att, kept, dropped, redacted)
		e.mustTx(t, func(tx store.Tx) error {
			if err := tx.PutMessage(e.ctx, e.message(dropped, "r2"), "r1"); err != nil {
				return err
			}
			got, err := tx.MessagesByAttachment(e.ctx, att, model.PageRequest{Limit: 10})
			if err == nil && !slices.Equal(messageIDs(got.Items), []model.ID{kept.ID, redacted.ID}) {
				t.Errorf("own rewrite: holders %v, want the rewrite dropped", messageIDs(got.Items))
			}
			if err != nil {
				return err
			}
			_, err = tx.Redact(e.ctx, store.Redaction{Message: &redacted, Expected: "r1", Revision: "r2"})
			return err
		})
		e.requireHolders(t, att, kept)
	}},
	{"MessagesByAttachmentScopedAndBounded", func(t *testing.T, e *env) {
		e.mustView(t, func(r store.Reader) error {
			_, err := r.MessagesByAttachment(e.ctx, model.AttachmentRef{Inbox: model.InboxRef{Tenant: tenantB, ID: e.inbox.ID}, ID: e.id(t)}, model.PageRequest{Limit: 10})
			wantCode(t, err, model.NotFound)
			_, err = r.MessagesByAttachment(e.ctx, model.AttachmentRef{}, model.PageRequest{Limit: 10})
			wantCode(t, err, model.Validation)
			_, err = r.MessagesByAttachment(e.ctx, model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)}, model.PageRequest{})
			wantCode(t, err, model.Validation)
			return nil
		})
	}},
}

// putHolder commits a new message in conv whose parts name atts.
func (e *env) putHolder(t *testing.T, conv model.ConversationRef, atts ...model.AttachmentRef) model.MessageRef {
	t.Helper()
	ref := model.MessageRef{Conversation: conv, ID: e.id(t)}
	msg := e.message(ref, "r1")
	for i := range atts {
		msg.Content.Parts = append(msg.Content.Parts, model.Part{Kind: model.PartAttachment, Attachment: &atts[i]})
	}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, msg, "") })
	return ref
}

func (e *env) requireHolders(t *testing.T, att model.AttachmentRef, want ...model.MessageRef) {
	t.Helper()
	ids := make([]model.ID, len(want))
	for i, m := range want {
		ids[i] = m.ID
	}
	e.mustView(t, func(r store.Reader) error {
		got, err := r.MessagesByAttachment(e.ctx, att, model.PageRequest{Limit: 10})
		if err == nil && !slices.Equal(messageIDs(got.Items), ids) {
			t.Errorf("holders = %v, want %v", messageIDs(got.Items), ids)
		}
		return err
	})
}
