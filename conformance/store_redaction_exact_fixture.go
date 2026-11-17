package conformance

import (
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// exactRedaction is one message with every field set, and every record that
// holds a copy of its content, each with every field set too: a store that
// clears, drops or invents any field fails the exact comparison, and
// requireSet fails the fixture itself when a new field is left unexercised.
type exactRedaction struct {
	msg     model.Message
	parent  model.Message
	echoed  model.Message
	att     store.AttachmentRecord
	op      store.OperationRecord
	created store.IngressRecord
	edited  store.IngressRecord
	other   store.IngressRecord
}

func (e *env) exactRedactionFixture(t *testing.T) *exactRedaction {
	t.Helper()
	fx := &exactRedaction{}
	conv := e.putConversation(t)
	at := time.Unix(1_700_000_000, 7).UTC()
	attRef := model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)}
	provider := e.externalRef("file", "pf-exact")
	fx.att = store.AttachmentRecord{
		Attachment: model.Attachment{Ref: attRef, Revision: "a1", Metadata: model.AttachmentMetadata{
			Name: "scan.pdf", MediaType: "application/pdf", Size: 42, Digest: "sha256:" + digestHex,
			Audience: model.Audience{Visibility: model.Restricted, PolicyRef: "legal"},
		}},
		Blob: &ports.BlobKey{Tenant: tenantA, ID: e.id(t)}, Provider: &provider,
		StagingOperation: model.OperationRef{Tenant: tenantA, ID: e.id(t)}, Stager: "conformance-stager", State: "stored",
	}
	fx.parent = e.message(model.MessageRef{Conversation: conv, ID: e.id(t)}, "p1")
	fx.echoed = e.message(model.MessageRef{Conversation: conv, ID: e.id(t)}, "o1")

	fx.op = e.operation(t, "op-exact", []byte{7})
	fx.op.Operation.State = model.ProviderAccepted
	fx.op.Command = model.Extension{Name: "conformance.intent", Version: 3, Data: []byte(`{"text":"secret body"}`)}
	fx.op.ExpiresAt = at.Add(24 * time.Hour)
	msgRef := model.MessageRef{Conversation: conv, ID: e.id(t)}
	confirmed := at.Add(time.Minute)
	fx.op.Operation.ResultMessage = &msgRef
	fx.op.Operation.ProviderReferences = []model.ExternalRef{model.ProviderMessageRef(e.channel, "pm-1")}
	fx.op.Operation.ReadConfirmedAt = &confirmed
	next := at.Add(2 * time.Minute)
	fx.op.Operation.NextAttemptAt = &next
	fx.op.Operation.Progress = &model.OperationProgress{Done: 3, Partial: true}
	fx.op.Operation.Held = &model.HeldBlock{
		Proposer: "plugin:exact", OnBehalfOf: "actor-exact", Reason: "exact", TrustKey: "exact/key",
		TrustLabels: map[string]string{"kind": "exact"}, ProposedFingerprint: []byte{7},
		Preview: &model.Extension{Name: "conformance.preview", Version: 1, Data: []byte(`{"text":"preview"}`)},
	}
	// The store sets the write time itself and ignores this one; once the
	// claim has returned it, the fixture holds the time redaction must leave.
	fx.op.Operation.UpdatedAt = at.Add(-time.Hour)

	by := model.IdentityRef{Inbox: e.inbox, ID: e.id(t)}
	editOp := model.OperationRef{Tenant: tenantA, ID: e.id(t)}
	task := model.TaskRef{Conversation: conv, ID: e.id(t)}
	respondedTo := model.InteractionRef{Conversation: conv, ID: e.id(t)}
	fx.msg = e.mutatedMessage(msgRef, by, editOp)
	fx.msg.Author = model.Author{Identity: by, External: e.externalRef("user", "u-1"), Kind: model.ContactKindPerson, Assurance: "verified"}
	fx.msg.Content = model.Content{Parts: []model.Part{
		{Kind: model.PartText, Text: "secret body"},
		{Kind: model.PartAttachment, Text: "scan.pdf", Attachment: &attRef},
	}}
	fx.msg.Audience = model.Audience{Visibility: model.Internal, PolicyRef: "desk"}
	fx.msg.External = []model.ExternalRef{model.ProviderMessageRef(e.channel, "pm-1"), e.externalRef("thread", "t-1")}
	fx.msg.Task = &task
	fx.msg.OccurredAt, fx.msg.RecordedAt = at, at.Add(time.Second)
	fx.msg.Revision, fx.msg.Deleted = "m1", true
	fx.msg.Origin = &model.Origin{Actor: "staff-1", Operation: fx.op.Operation.Ref, Application: "desk"}
	fx.msg.ImportedBy, fx.msg.Source = "importer", "conformance.chat"
	fx.msg.Metadata = []model.Extension{
		{Name: model.CoreNamespace + ".ingress.source", Version: 1, Data: []byte(`{"source_revision":"v1"}`)},
		{Name: "conformance.chat.raw", Version: 1, Data: []byte(`{"subject":"secret subject"}`)},
	}
	fx.msg.EchoOf, fx.msg.InReplyTo, fx.msg.InResponseTo = &fx.echoed.Ref, &fx.parent.Ref, &respondedTo

	ingress := func(event, msgID string) store.IngressRecord {
		rec := e.ingressRecord(t, "dedup-"+event)
		rec.Input = model.Ingress{
			Call: model.Call{Correlation: "corr-" + model.CorrelationID(event)},
			Envelope: model.Envelope{
				EventID: event, MessageID: msgID, InReplyTo: "pm-parent", Destination: "support@example.test", Kind: model.EnvelopeMessage,
				Sender: e.externalRef("user", "u-1"), Assurance: "verified", SystemActor: true, SenderKind: model.ContactKindPerson,
				SenderAttributes: []model.Attribute{{Kind: model.AttributePhone, Value: "+15550100199", Verified: true}},
				References:       []model.ExternalRef{e.externalRef("thread", "t-1")},
				Content:          model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "secret body"}}},
				Audience:         model.Audience{Visibility: model.Public},
				OccurredAt:       at, SourceRevision: "v1",
				Payload:  model.Extension{Name: "conformance.chat.raw", Version: 2, Data: []byte(`{"raw":"secret"}`)},
				Delivery: &model.DeliveryReport{State: model.ProviderAccepted},
				Event:    &model.Extension{Name: "conformance.chat.event", Version: 1, Data: []byte(`{"ref":"t-1"}`)},
				Interaction: &model.InteractionInput{
					ID: "i-" + event, Kind: model.InteractionSubmission, Actor: e.externalRef("user", "u-1"),
					Action: "approve", Command: "triage", Arguments: "secret arguments",
					Fields:   model.Extension{Name: "conformance.form", Version: 4, Data: []byte(`{"f":"secret"}`)},
					Deadline: at.Add(time.Hour),
				},
			},
			ReceiverCursor: "rc-" + event,
		}
		return rec
	}
	fx.created = ingress("e-created", "pm-1")
	fx.created.Receipt = model.IngressReceipt{ID: fx.created.ID, Durable: true, Committed: true}
	fx.edited = ingress("e-edited", "pm-1")
	fx.edited.Input.Envelope.Kind = model.EnvelopeMessageEdited
	fx.other = ingress("e-other", "pm-2")

	e.mustTx(t, func(tx store.Tx) error {
		if err := tx.PutAttachment(e.ctx, fx.att, ""); err != nil {
			return err
		}
		claimed, _, err := tx.ClaimOperation(e.ctx, fx.op)
		if err != nil {
			return err
		}
		fx.op.Operation.UpdatedAt = claimed.Operation.UpdatedAt
		for _, m := range []model.Message{fx.parent, fx.echoed, fx.msg} {
			if err := tx.PutMessage(e.ctx, m, ""); err != nil {
				return err
			}
		}
		for _, rec := range []store.IngressRecord{fx.created, fx.edited, fx.other} {
			claim := rec.Clone()
			claim.Receipt = model.IngressReceipt{}
			if _, _, err := tx.ClaimIngress(e.ctx, claim); err != nil {
				return err
			}
		}
		return tx.CompleteIngress(e.ctx, fx.created.ID, fx.created.Receipt)
	})

	// Redaction happens later than the writes, so a redaction that moved the
	// operation's write time would show.
	if e.fx.Advance != nil {
		e.fx.Advance(time.Second)
	}
	requireSet(t, "message", fx.msg, "Redacted")
	requireSet(t, "attachment record", fx.att)
	requireSet(t, "operation record", fx.op)
	// Pending, PendingState and Key are derived when an operation is read, never stored.
	requireSet(t, "operation", fx.op.Operation, "Result", "ResultStream", "ResultTask", "Failure", "Pending", "PendingState", "Key")
	requireSet(t, "edit", *fx.msg.LastEdit)
	requireSet(t, "envelope", fx.created.Input.Envelope, "WorkItem", "Task", "State")
	requireSet(t, "interaction input", *fx.created.Input.Envelope.Interaction,
		"Bot", "Context", "Message", "ResponseToken", "ResponseExpiresAt")
	return fx
}

// requireSet fails unless every top-level field of v outside skip is
// non-zero, so a field added to a record is exercised by redaction here
// before the fixture can pass.
func requireSet(t *testing.T, what string, v any, skip ...string) {
	t.Helper()
	rv := reflect.ValueOf(v)
	for i := 0; i < rv.NumField(); i++ {
		name := rv.Type().Field(i).Name
		if rv.Field(i).IsZero() && !containsString(skip, name) {
			t.Fatalf("%s fixture leaves %s unset: set it so redaction is checked against it", what, name)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (fx *exactRedaction) wantMessages(t *testing.T, e *env, want model.Message) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.GetMessage(e.ctx, want.Ref)
		if err == nil {
			diffFields(t, "message", got, want)
		}
		return err
	})
}

func (fx *exactRedaction) wantIngress(t *testing.T, e *env, want store.IngressRecord) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.Ingress(e.ctx, want.ID)
		if err == nil {
			diffFields(t, "ingress "+want.Input.Envelope.EventID, got, want)
			diffFields(t, "ingress "+want.Input.Envelope.EventID+" envelope", got.Input.Envelope, want.Input.Envelope)
		}
		return err
	})
}

func (fx *exactRedaction) wantOperation(t *testing.T, e *env, want store.OperationRecord) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.Operation(e.ctx, want.Operation.Ref)
		if err == nil {
			diffFields(t, "operation record", got, want)
			diffFields(t, "operation", got.Operation, want.Operation)
		}
		return err
	})
}

func (fx *exactRedaction) wantAttachment(t *testing.T, e *env, want store.AttachmentRecord) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.Attachment(e.ctx, want.Attachment.Ref)
		if err == nil {
			diffFields(t, "attachment record", got, want)
			diffFields(t, "attachment", got.Attachment, want.Attachment)
		}
		return err
	})
}
