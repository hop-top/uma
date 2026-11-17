package memory

import (
	"context"
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// TestForkSharesEveryTable fails when a state field is left out of fork: a
// forgotten table would be empty in every later unit and drop its data on the
// next commit.
func TestForkSharesEveryTable(t *testing.T) {
	ctx := context.Background()
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	ids := portstest.NewSequenceIDs(7)
	id := func() model.ID {
		v, err := ids.New(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const tenant model.TenantID = "fork-tenant"
	s := New(Options{Clock: clock})
	ch := model.ChannelRef{Tenant: tenant, ID: id()}
	in := model.InboxRef{Tenant: tenant, ID: id()}
	conv := model.ConversationRef{Inbox: in, ID: id()}
	op := model.OperationRef{Tenant: tenant, ID: id()}
	sub := model.SubscriptionRef{Tenant: tenant, ID: id()}
	ident := model.Identity{
		Ref: model.IdentityRef{Inbox: in, ID: id()}, Contact: model.ContactRef{Tenant: tenant, ID: id()},
		Namespace: "fork.user", SourceID: "u1", Revision: "1",
	}
	key := model.IdentityKey{Inbox: in, Namespace: ident.Namespace, SourceID: ident.SourceID}
	until := clock.Now().Add(time.Minute)
	err := s.Transact(ctx, tenant, func(tx store.Tx) error {
		ev := model.Event{
			Version: 1, ID: id(), Kind: "fork.event", Tenant: tenant, Inbox: in.ID, Resource: conv.Ref(),
			Audience: model.Audience{Visibility: model.Internal}, RecordedAt: until,
		}
		link := store.LinkRecord{
			ID: id(), Tenant: tenant, Identity: ident.Ref, IdentityKey: key, Class: model.LinkClassLink,
			From: ident.Contact, To: model.ContactRef{Tenant: tenant, ID: id()},
			Evidence:  model.Evidence{Verifier: "attestation", Reference: "r1"},
			Assurance: model.LinkAssuranceAsserted, Operation: op, RevisionBefore: "1", RevisionAfter: "2",
		}
		return firstErr(
			tx.PutChannel(ctx, model.Channel{Ref: ch, Revision: "1"}, ""),
			tx.PutInbox(ctx, model.Inbox{Ref: in, Channel: ch, Revision: "1"}, ""),
			tx.PutContact(ctx, model.Contact{Ref: ident.Contact, Revision: "1"}, ""),
			third(tx.ClaimIdentity(ctx, key, ident)),
			third(tx.ClaimConversation(ctx, store.ConversationClaim{
				Selection:    store.SelectionKey{Inbox: in, Mode: model.ModePerContact, Key: "g"},
				CreateKey:    "k",
				Conversation: model.Conversation{Ref: conv, Mode: model.ModePerContact, Revision: "1"},
			})),
			tx.PutMessage(ctx, model.Message{Ref: model.MessageRef{Conversation: conv, ID: id()}, Revision: "1"}, ""),
			third(tx.ClaimMessage(ctx, store.MessageClaim{
				Reference: model.ProviderMessageRef(ch, "p1"),
				Message: model.Message{
					Ref: model.MessageRef{Conversation: conv, ID: id()}, Revision: "1",
					External: []model.ExternalRef{model.ProviderMessageRef(ch, "p1")},
				},
			})),
			tx.PutWorkItem(ctx, model.WorkItem{Ref: model.WorkItemRef{Conversation: conv, ID: id()}, Revision: "1"}, ""),
			func() error {
				task := model.TaskRef{Conversation: conv, ID: id()}
				return tx.PutTask(ctx, model.Task{Ref: task, External: model.TaskExternalRef(ch, task.ID), Revision: "1"}, "")
			}(),
			tx.PutStream(ctx, model.Stream{Ref: model.StreamRef{Conversation: conv, ID: id()}, Revision: "1"}, ""),
			tx.PutInteraction(ctx, store.InteractionRecord{
				Interaction: model.Interaction{
					Ref: model.InteractionRef{Conversation: conv, ID: id()}, Kind: model.InteractionAction, Revision: "1",
				},
				Ingress: id(),
			}),
			tx.PutLabel(ctx, model.Label{Ref: model.LabelRef{Tenant: tenant, ID: id()}, Name: "fork", Revision: "1"}, ""),
			tx.PutReadMarker(ctx, model.ReadMarker{Conversation: conv, Actor: "fork-actor", Revision: "1"}, ""),
			tx.PutAttachment(ctx, store.AttachmentRecord{Attachment: model.Attachment{
				Ref: model.AttachmentRef{Inbox: in, ID: id()}, Revision: "1",
			}}, ""),
			tx.RecordStaging(ctx, store.StagingIntent{
				Key: ports.BlobKey{Tenant: tenant, ID: id()}, Attachment: model.AttachmentRef{Inbox: in, ID: id()},
			}),
			tx.PutExport(ctx, store.ExportRecord{
				Export: model.Export{Ref: model.ExportRef{Tenant: tenant, ID: id()}, Revision: "1"},
				Blob:   ports.BlobKey{Tenant: tenant, ID: id()},
			}, ""),
			tx.PutProtocol(ctx, store.ProtocolState{Channel: ch, Key: "k", Revision: "1"}, ""),
			second(tx.AppendRecipientLog(ctx, store.RecipientLogKey{Channel: ch, Namespace: "n", Recipient: "r"},
				model.Extension{Name: "fork.entry", Version: 1}, 1)),
			third(tx.ClaimIngress(ctx, store.IngressRecord{ID: id(), Channel: ch, DedupKey: "d"})),
			third(tx.ClaimOperation(ctx, store.OperationRecord{
				Operation: model.Operation{Ref: op, Revision: "1"}, FingerprintVersion: 1, Fingerprint: []byte{1},
				Key: store.OperationKey{Tenant: tenant, Caller: "a", Kind: "k", Target: in.Ref(), ID: "o"},
			})),
			tx.AppendAttemptEvidence(ctx, store.AttemptEvidence{Operation: op, Attempt: id()}),
			tx.PutSubscription(ctx, store.SubscriptionRecord{
				Subscription: model.Subscription{Ref: sub, Revision: "1"},
				Principal:    model.Principal{Tenant: tenant, Actor: "a"}, Consumer: "c",
			}, ""),
			tx.EnqueueWork(ctx, store.Work{ID: id(), Kind: "k", Resource: op.Ref()}),
			second(tx.AcquireLease(ctx, store.LeaseRequest{Resource: sub.Ref(), Owner: "o", Until: until})),
			tx.AppendEvents(ctx, []model.Event{ev}),
			tx.LinkIdentity(ctx, link),
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Quarantine a second item; purge nothing, so the event base stays 0.
	err = s.Transact(ctx, tenant, func(tx store.Tx) error {
		q := store.Work{ID: id(), Kind: "q", Resource: model.OperationRef{Tenant: tenant, ID: id()}.Ref()}
		if err := tx.EnqueueWork(ctx, q); err != nil {
			return err
		}
		got, err := tx.ClaimWork(ctx, store.WorkClaim{Kinds: []string{"q"}, Limit: 1, Owner: "o", Until: until})
		if err != nil {
			return err
		}
		return tx.QuarantineWork(ctx, got[0])
	})
	if err != nil {
		t.Fatal(err)
	}
	committed := s.load(tenant)
	forked := committed.fork()
	cv, fv := reflect.ValueOf(committed).Elem(), reflect.ValueOf(forked).Elem()
	for i := range cv.NumField() {
		f := cv.Type().Field(i)
		if f.Type.Kind() != reflect.Struct || f.Type == reflect.TypeFor[time.Time]() {
			continue
		}
		data := cv.Field(i).Field(0) // table.m or log.items
		if data.IsNil() {
			t.Errorf("%s is empty after the setup unit; extend this test to write it", f.Name)
			continue
		}
		if data.Pointer() != fv.Field(i).Field(0).Pointer() {
			t.Errorf("fork does not share %s", f.Name)
		}
	}
	if forked.eventBase != committed.eventBase {
		t.Errorf("fork eventBase = %d, want %d", forked.eventBase, committed.eventBase)
	}
	if committed.eventRecordedMax.IsZero() || !forked.eventRecordedMax.Equal(committed.eventRecordedMax) {
		t.Errorf("fork eventRecordedMax = %s, want the committed %s", forked.eventRecordedMax, committed.eventRecordedMax)
	}
	if forked.seq != committed.seq {
		t.Errorf("fork seq = %d, want %d", forked.seq, committed.seq)
	}
}

func second[A any](_ A, err error) error { return err }

func third[A any](_ A, _ bool, err error) error { return err }
