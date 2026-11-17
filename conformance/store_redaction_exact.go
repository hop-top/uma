package conformance

import (
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// redactionExactCases pin what Redact leaves, field by field, on records
// that have every field set: a message (reactions, last edit, echo and reply
// links, deletion and response correlation included), an interaction, an
// attachment, the ingress copies and the originating operation. Reactions
// and the last edit are not content: they name reactors and editors, not
// what was said, and the tombstone keeps them. A backend that clears or
// rewrites any other field than the ones listed for Redact fails here
// instead of passing unnoticed.
var redactionExactCases = []storeCase{
	{"MessageRedactionLeavesExactlyTheDocumentedRecords", func(t *testing.T, e *env) {
		fx := e.exactRedactionFixture(t)
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Message: &fx.msg.Ref, Expected: "m1", Revision: "m2"})
			return err
		})

		// Content and non-core metadata go; every other field stays as
		// stored, Deleted, Reactions, ReactionsComplete, LastEdit, EchoOf,
		// InReplyTo, InResponseTo and Task included.
		msg := fx.msg.Clone()
		msg.Content = model.Content{}
		msg.Metadata = []model.Extension{fx.msg.Metadata[0]}
		msg.Redacted, msg.Revision = true, "m2"
		fx.wantMessages(t, e, msg)
		fx.wantMessages(t, e, fx.parent)
		fx.wantMessages(t, e, fx.echoed)

		// Attachments the parts reference lose their metadata and locators,
		// and keep the staging operation, the stager and the audience.
		att := fx.att.Clone()
		att.Attachment.Metadata = model.AttachmentMetadata{Audience: fx.att.Attachment.Metadata.Audience}
		att.Attachment.Revision = "m2"
		att.Blob, att.Provider, att.State = nil, nil, store.AttachmentRedacted
		fx.wantAttachment(t, e, att)

		// Ingress copies naming the message's provider ID lose content, raw
		// payload data and interaction data; everything else stays.
		for _, rec := range []store.IngressRecord{fx.created, fx.edited} {
			want := rec.Clone()
			want.Input.Envelope.Content = model.Content{}
			want.Input.Envelope.Payload.Data = nil
			want.Input.Envelope.Interaction.Fields.Data = nil
			want.Input.Envelope.Interaction.Arguments = ""
			fx.wantIngress(t, e, want)
		}
		fx.wantIngress(t, e, fx.other)

		// The originating operation loses Command.Data only.
		op := fx.op.Clone()
		op.Command.Data = nil
		fx.wantOperation(t, e, op)
	}},
	{"AttachmentRedactionLeavesExactlyTheDocumentedRecords", func(t *testing.T, e *env) {
		fx := e.exactRedactionFixture(t)
		ref := fx.att.Attachment.Ref
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Attachment: &ref, Expected: "a1", Revision: "a2"})
			return err
		})
		att := fx.att.Clone()
		att.Attachment.Metadata = model.AttachmentMetadata{Audience: fx.att.Attachment.Metadata.Audience}
		att.Attachment.Revision = "a2"
		att.Blob, att.Provider, att.State = nil, nil, store.AttachmentRedacted
		fx.wantAttachment(t, e, att)

		// Nothing else changes: the message still holds its parts and
		// metadata, and the copies and the intent are whole.
		fx.wantMessages(t, e, fx.msg)
		for _, rec := range []store.IngressRecord{fx.created, fx.edited, fx.other} {
			fx.wantIngress(t, e, rec)
		}
		fx.wantOperation(t, e, fx.op)
	}},
	{"InteractionRedactionLeavesExactlyTheDocumentedRecords", func(t *testing.T, e *env) {
		fx := e.interactionRedactionFixture(t)
		ref := fx.rec.Interaction.Ref
		initial, followUp := e.operationRef(t), e.operationRef(t)
		e.mustTx(t, func(tx store.Tx) error {
			if _, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: initial, Revision: "i2"}); err != nil {
				return err
			}
			_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: followUp, FollowUp: true, Revision: "i3"})
			return err
		})
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Interaction: &ref, Expected: "i3", Revision: "i4"})
			return err
		})

		// Field data and arguments go; references, actor, bot, message,
		// context, audience, times, deadline, response state, metadata and
		// the token expiry stay. The token itself is gone.
		want := fx.rec.Interaction.Clone()
		want.Fields.Data, want.Arguments = nil, ""
		want.Response = model.InteractionResponse{Initial: &initial, FollowUps: 1}
		want.Redacted, want.Revision = true, "i4"
		requireSet(t, "interaction", fx.rec.Interaction, "Redacted", "Response")
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Interaction(e.ctx, ref)
			if err == nil {
				diffFields(t, "interaction", got, want)
			}
			return err
		})
		e.wantToken(t, ref, store.ResponseToken{ExpiresAt: fx.rec.TokenExpiresAt})

		// The ingress copy loses the same data and nothing else.
		cleared := fx.ingress.Clone()
		cleared.Input.Envelope.Interaction.Fields.Data = nil
		cleared.Input.Envelope.Interaction.Arguments = ""
		(&exactRedaction{}).wantIngress(t, e, cleared)

		// The sibling interaction and the message it sits on are untouched.
		e.wantInteraction(t, fx.other.Interaction)
		e.wantToken(t, fx.other.Interaction.Ref, store.ResponseToken{Token: "other-token", ExpiresAt: fx.other.TokenExpiresAt})
		e.wantMessage(t, fx.msg)
	}},
}
