package conformance

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// digestHex is a well-formed sha256 digest in hex.
const digestHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var errRollback = errors.New("conformance: roll the unit back")

// redactionCases cover Redact: the tombstone of the target and of every
// record holding a copy of its content, the references and audit fields
// that survive, revision and tenant scope, and refusal while a worker may
// still read the originating operation's intent.
var redactionCases = []storeCase{
	{"MessageRedactionTombstonesEveryCopy", func(t *testing.T, e *env) {
		fx := e.redactionFixture(t)
		var got store.RedactResult
		e.mustTx(t, func(tx store.Tx) error {
			var err error
			got, err = tx.Redact(e.ctx, store.Redaction{Message: &fx.msg.Ref, Expected: "m1", Revision: "m2"})
			return err
		})
		if want := []ports.BlobKey{*fx.att.Blob}; !reflect.DeepEqual(got.Blobs, want) {
			t.Errorf("unbound blobs = %+v, want %+v", got.Blobs, want)
		}
		e.wantStaging(t, *fx.att.Blob, fx.att.Attachment.Ref)

		want := fx.msg.Clone()
		want.Content = model.Content{}
		want.Metadata = []model.Extension{fx.msg.Metadata[0]}
		want.Redacted, want.Revision = true, "m2"
		e.wantMessage(t, want)

		att := fx.att.Clone()
		att.Attachment.Metadata = model.AttachmentMetadata{Audience: fx.att.Attachment.Metadata.Audience}
		att.Attachment.Revision = "m2"
		att.Blob, att.Provider, att.State = nil, nil, store.AttachmentRedacted
		e.wantAttachment(t, att)

		for _, rec := range []store.IngressRecord{fx.created, fx.edited} {
			want := rec.Clone()
			want.Input.Envelope.Content = model.Content{}
			want.Input.Envelope.Payload.Data = nil
			want.Input.Envelope.Interaction.Fields.Data = nil
			e.wantIngress(t, want)
		}
		e.wantIngress(t, fx.otherMessage)
		e.wantIngress(t, fx.otherChannel)

		op := fx.op.Clone()
		op.Command.Data = nil
		op.Operation.UpdatedAt = micro(e.fx.Now()) // the write time; the store clock has not moved
		e.mustView(t, func(r store.Reader) error {
			gotOp, err := r.Operation(e.ctx, fx.op.Operation.Ref)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(gotOp, op) {
				t.Errorf("operation = %+v, want %+v", gotOp, op)
			}
			attempts, err := r.Attempts(e.ctx, fx.op.Operation.Ref, page(10))
			if err == nil && (len(attempts.Items) != 1 || !reflect.DeepEqual(attempts.Items[0], fx.attempt)) {
				t.Errorf("attempt evidence = %+v, want it unchanged", attempts.Items)
			}
			return err
		})
		if evs := e.allEvents(t, model.Filter{}, 10); len(evs) != 1 || !reflect.DeepEqual(evs[0], fx.event) {
			t.Errorf("events = %+v, want the one event unchanged", evs)
		}

		// The tombstone keeps answering replay correlation: its provider
		// reference still finds it and the ingress dedup key still holds.
		e.mustView(t, func(r store.Reader) error {
			ms, err := r.MessagesByReference(e.ctx, e.inbox, model.ProviderMessageRef(e.channel, "pm-1"), 2)
			if err == nil && (len(ms) != 1 || ms[0].Ref != fx.msg.Ref) {
				t.Errorf("messages by provider ID = %d, want the tombstone", len(ms))
			}
			return err
		})
		redelivered := e.ingressRecord(t, fx.created.DedupKey)
		redelivered.Input = fx.created.Input
		if back, created := e.claimIngress(t, redelivered); created || back.ID != fx.created.ID {
			t.Errorf("redelivery claim = %s created %v, want the tombstoned record", back.ID, created)
		}

		// A tombstone can be redacted again; nothing is left to unbind.
		e.mustTx(t, func(tx store.Tx) error {
			again, err := tx.Redact(e.ctx, store.Redaction{Message: &fx.msg.Ref, Expected: "m2", Revision: "m3"})
			if err == nil && len(again.Blobs) != 0 {
				t.Errorf("second redaction unbound %+v, want nothing", again.Blobs)
			}
			return err
		})
	}},
	{"StaleRevisionConflictsAndWritesNothing", func(t *testing.T, e *env) {
		fx := e.redactionFixture(t)
		att := fx.att.Attachment.Ref
		for _, c := range []struct {
			name string
			req  store.Redaction
		}{
			{"message", store.Redaction{Message: &fx.msg.Ref, Expected: "m0", Revision: "m2"}},
			{"attachment", store.Redaction{Attachment: &att, Expected: "a1", Revision: "a3"}},
		} {
			req := c.req
			err := e.txErr(func(tx store.Tx) error {
				_, err := tx.Redact(e.ctx, req)
				wantCode(t, err, model.Conflict, "Redact of a ", c.name, " at a stale revision")
				return nil
			})
			if err != nil {
				t.Fatalf("Transact: %v", err)
			}
			fx.requireIntact(t, e)
		}
	}},
	{"RedactionChecksRevisionScopeAndTarget", func(t *testing.T, e *env) {
		fx := e.redactionFixture(t)
		other := model.MessageRef{Conversation: model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, ID: fx.msg.Ref.ID}
		att := fx.att.Attachment.Ref
		for name, c := range map[string]struct {
			tenant model.TenantID
			req    store.Redaction
			code   model.ErrorCode
		}{
			"stale revision":     {tenantA, store.Redaction{Message: &fx.msg.Ref, Expected: "m0", Revision: "m2"}, model.Conflict},
			"no revision":        {tenantA, store.Redaction{Message: &fx.msg.Ref, Expected: "m1"}, model.Validation},
			"unchanged revision": {tenantA, store.Redaction{Message: &fx.msg.Ref, Expected: "m1", Revision: "m1"}, model.Validation},
			"no target":          {tenantA, store.Redaction{Expected: "m1", Revision: "m2"}, model.Validation},
			"two targets":        {tenantA, store.Redaction{Message: &fx.msg.Ref, Attachment: &att, Expected: "m1", Revision: "m2"}, model.Validation},
			"other conversation": {tenantA, store.Redaction{Message: &other, Expected: "m1", Revision: "m2"}, model.NotFound},
			"foreign tenant":     {tenantB, store.Redaction{Message: &fx.msg.Ref, Expected: "m1", Revision: "m2"}, model.NotFound},
			"foreign attachment": {tenantB, store.Redaction{Attachment: &att, Expected: "a2", Revision: "a3"}, model.NotFound},
		} {
			t.Run(name, func(t *testing.T) {
				// The unit commits whatever Redact reports, so a refused call
				// must not have written anything.
				err := e.s.Transact(e.ctx, c.tenant, func(tx store.Tx) error {
					_, err := tx.Redact(e.ctx, c.req)
					wantCode(t, err, c.code)
					return nil
				})
				if err != nil {
					t.Fatalf("Transact: %v", err)
				}
				fx.requireIntact(t, e)
			})
		}
	}},
	{"RedactionRefusesAnOperationStillAtWork", func(t *testing.T, e *env) {
		fx := e.redactionFixture(t)
		redact := func() error {
			return e.txErr(func(tx store.Tx) error {
				_, err := tx.Redact(e.ctx, store.Redaction{Message: &fx.msg.Ref, Expected: "m1", Revision: "m2"})
				wantCode(t, err, model.Conflict)
				return nil
			})
		}
		work := store.Work{ID: e.id(t), Kind: workKind, Resource: fx.op.Operation.Ref.Ref()}
		e.mustTx(t, func(tx store.Tx) error { return tx.EnqueueWork(e.ctx, work) })
		if err := redact(); err != nil {
			t.Fatal(err)
		}
		fx.requireIntact(t, e)
		claimed := e.claimWork(t, "worker", 1)
		if len(claimed) != 1 {
			t.Fatalf("claimed %d items, want 1", len(claimed))
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.CompleteWork(e.ctx, claimed[0]) })
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.AcquireLease(e.ctx, store.LeaseRequest{
				Resource: fx.op.Operation.Ref.Ref(), Owner: "worker", Until: e.fx.Now().Add(time.Hour),
			})
			return err
		})
		if err := redact(); err != nil {
			t.Fatal(err)
		}
		fx.requireIntact(t, e)
	}},
	{"AttachmentRedactionTombstonesTheRecordOnly", func(t *testing.T, e *env) {
		fx := e.redactionFixture(t)
		ref := fx.att.Attachment.Ref
		wantCode(t, e.txErr(func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Attachment: &ref, Expected: "a1", Revision: "a3"})
			return err
		}), model.Conflict)
		var got store.RedactResult
		e.mustTx(t, func(tx store.Tx) error {
			var err error
			got, err = tx.Redact(e.ctx, store.Redaction{Attachment: &ref, Expected: "a2", Revision: "a3"})
			return err
		})
		if want := []ports.BlobKey{*fx.att.Blob}; !reflect.DeepEqual(got.Blobs, want) {
			t.Errorf("unbound blobs = %+v, want %+v", got.Blobs, want)
		}
		e.wantStaging(t, *fx.att.Blob, ref)
		att := fx.att.Clone()
		att.Attachment.Metadata = model.AttachmentMetadata{Audience: fx.att.Attachment.Metadata.Audience}
		att.Attachment.Revision = "a3"
		att.Blob, att.Provider, att.State = nil, nil, store.AttachmentRedacted
		e.wantAttachment(t, att)
		e.wantMessage(t, fx.msg)
		e.wantIngress(t, fx.created)
	}},
	{"RedactionRollsBackWithItsUnit", func(t *testing.T, e *env) {
		fx := e.redactionFixture(t)
		err := e.txErr(func(tx store.Tx) error {
			if _, err := tx.Redact(e.ctx, store.Redaction{Message: &fx.msg.Ref, Expected: "m1", Revision: "m2"}); err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("Transact = %v, want the callback error", err)
		}
		fx.requireIntact(t, e)
	}},
}

// redactionFixture is one message with every kind of record that holds a
// copy of its content, plus records that must stay untouched.
type redactionFixture struct {
	msg          model.Message
	att          store.AttachmentRecord
	op           store.OperationRecord
	attempt      store.AttemptEvidence
	created      store.IngressRecord
	edited       store.IngressRecord
	otherMessage store.IngressRecord
	otherChannel store.IngressRecord
	event        model.Event
}

func (e *env) redactionFixture(t *testing.T) *redactionFixture {
	t.Helper()
	fx := &redactionFixture{}
	conv := e.putConversation(t)
	second := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
	attRef := model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)}
	provider := e.externalRef("file", "pf-1")
	fx.att = store.AttachmentRecord{
		Attachment: model.Attachment{Ref: attRef, Revision: "a2", Metadata: model.AttachmentMetadata{
			Name: "scan.pdf", MediaType: "application/pdf", Size: 42, Digest: "sha256:" + digestHex,
			Audience: model.Audience{Visibility: model.Restricted, PolicyRef: "legal"},
		}},
		Blob: &ports.BlobKey{Tenant: tenantA, ID: e.id(t)}, Provider: &provider,
		StagingOperation: model.OperationRef{Tenant: tenantA, ID: e.id(t)}, State: "stored",
	}
	fx.op = e.operation(t, "op-redact", []byte{7})
	fx.op.Operation.State = model.ProviderAccepted
	fx.op.Command = model.Extension{Name: "conformance.intent", Version: 1, Data: []byte(`{"text":"secret body"}`)}
	fx.attempt = store.AttemptEvidence{
		Operation: fx.op.Operation.Ref, Attempt: e.id(t), State: model.ProviderAccepted,
		ProviderReferences: []model.ExternalRef{model.ProviderMessageRef(e.channel, "pm-1")},
		ObservedAt:         time.Unix(1_700_000_000, 0).UTC(),
	}
	msgRef := model.MessageRef{Conversation: conv, ID: e.id(t)}
	identity := model.IdentityRef{Inbox: e.inbox, ID: e.id(t)}
	fx.msg = model.Message{
		Ref: msgRef,
		Author: model.Author{
			Identity: identity, External: e.externalRef("user", "u-1"), Kind: model.ContactKindPerson, Assurance: "verified",
		},
		Content: model.Content{Parts: []model.Part{
			{Kind: model.PartText, Text: "secret body"},
			{Kind: model.PartAttachment, Text: "scan.pdf", Attachment: &attRef},
		}},
		Audience:   model.Audience{Visibility: model.Internal, PolicyRef: "desk"},
		External:   []model.ExternalRef{model.ProviderMessageRef(e.channel, "pm-1"), e.externalRef("thread", "t-1")},
		OccurredAt: time.Unix(1_700_000_000, 0).UTC(), RecordedAt: time.Unix(1_700_000_001, 0).UTC(),
		Revision:   "m1",
		Origin:     &model.Origin{Actor: "staff-1", Operation: fx.op.Operation.Ref, Application: "desk"},
		ImportedBy: "importer", Source: "conformance.chat",
		Metadata: []model.Extension{
			{Name: model.CoreNamespace + ".ingress.source", Version: 1, Data: []byte(`{"source_revision":"v1"}`)},
			{Name: "conformance.chat.raw", Version: 1, Data: []byte(`{"subject":"secret subject"}`)},
		},
	}
	envelope := func(event, msgID string) model.Ingress {
		return model.Ingress{
			Call: model.Call{Correlation: "corr-" + model.CorrelationID(event)},
			Envelope: model.Envelope{
				EventID: event, MessageID: msgID, Destination: "support@example.test", Kind: model.EnvelopeMessage,
				Sender: e.externalRef("user", "u-1"), Assurance: "verified",
				References: []model.ExternalRef{e.externalRef("thread", "t-1")},
				Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "secret body"}}},
				Audience:   model.Audience{Visibility: model.Public},
				OccurredAt: time.Unix(1_700_000_000, 0).UTC(), SourceRevision: "v1",
				Payload: model.Extension{Name: "conformance.chat.raw", Version: 1, Data: []byte(`{"raw":"secret"}`)},
				Interaction: &model.InteractionInput{
					ID: "i-" + event, Kind: model.InteractionSubmission, Actor: e.externalRef("user", "u-1"),
					Fields: model.Extension{Name: "conformance.form", Version: 1, Data: []byte(`{"f":"secret"}`)},
				},
			},
			ReceiverCursor: "rc-" + event,
		}
	}
	ingress := func(ch model.ChannelRef, event, msgID string) store.IngressRecord {
		rec := e.ingressRecord(t, "dedup-"+event)
		rec.Channel, rec.Input = ch, envelope(event, msgID)
		return rec
	}
	fx.created = ingress(e.channel, "e-created", "pm-1")
	fx.created.Receipt = model.IngressReceipt{ID: fx.created.ID, Durable: true, Committed: true}
	fx.edited = ingress(e.channel, "e-edited", "pm-1")
	fx.edited.Input.Envelope.Kind = model.EnvelopeMessageEdited
	fx.otherMessage = ingress(e.channel, "e-other", "pm-2")
	fx.otherChannel = ingress(second, "e-second", "pm-1")
	fx.event = e.event(t, "conformance.message")
	fx.event.Resource = msgRef.Ref()
	fx.event.Inbox = msgRef.Conversation.Inbox.ID
	e.mustTx(t, func(tx store.Tx) error {
		if err := tx.PutChannel(e.ctx, e.channelRecord(second, "c1"), ""); err != nil {
			return err
		}
		if err := tx.PutAttachment(e.ctx, fx.att, ""); err != nil {
			return err
		}
		if _, _, err := tx.ClaimOperation(e.ctx, fx.op); err != nil {
			return err
		}
		if err := tx.AppendAttemptEvidence(e.ctx, fx.attempt); err != nil {
			return err
		}
		if err := tx.PutMessage(e.ctx, fx.msg, ""); err != nil {
			return err
		}
		for _, rec := range []store.IngressRecord{fx.created, fx.edited, fx.otherMessage, fx.otherChannel} {
			claim := rec.Clone()
			claim.Receipt = model.IngressReceipt{}
			if _, _, err := tx.ClaimIngress(e.ctx, claim); err != nil {
				return err
			}
		}
		if err := tx.CompleteIngress(e.ctx, fx.created.ID, fx.created.Receipt); err != nil {
			return err
		}
		return tx.AppendEvents(e.ctx, []model.Event{fx.event})
	})
	return fx
}

// requireIntact fails unless every fixture record is as written.
func (fx *redactionFixture) requireIntact(t *testing.T, e *env) {
	t.Helper()
	e.wantMessage(t, fx.msg)
	e.wantAttachment(t, fx.att)
	for _, rec := range []store.IngressRecord{fx.created, fx.edited, fx.otherMessage, fx.otherChannel} {
		e.wantIngress(t, rec)
	}
	e.mustView(t, func(r store.Reader) error {
		op, err := r.Operation(e.ctx, fx.op.Operation.Ref)
		if err == nil && !reflect.DeepEqual(op.Command, fx.op.Command) {
			t.Errorf("operation intent = %+v, want it unchanged", op.Command)
		}
		return err
	})
}

func (e *env) wantMessage(t *testing.T, want model.Message) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.GetMessage(e.ctx, want.Ref)
		if err == nil && !reflect.DeepEqual(got, want) {
			wantEqual(t, "message "+string(want.Ref.ID), got, want)
		}
		return err
	})
}

func (e *env) wantIngress(t *testing.T, want store.IngressRecord) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.Ingress(e.ctx, want.ID)
		if err == nil && !reflect.DeepEqual(got, want) {
			wantEqual(t, "ingress "+want.Input.Envelope.EventID, got, want)
		}
		return err
	})
}

// wantStaging requires a staging intent for key naming attachment, so the
// retention sweep deletes bytes a redaction unbound if the caller cannot.
func (e *env) wantStaging(t *testing.T, key ports.BlobKey, attachment model.AttachmentRef) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		in, err := r.Staging(e.ctx, key)
		if err == nil && (in.Key != key || in.Attachment != attachment || in.Abandoned) {
			t.Errorf("staging intent = %+v, want one for %+v", in, attachment)
		}
		return err
	})
}
