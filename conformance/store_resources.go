package conformance

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Conversation- and inbox-scoped resources the engine keeps beside its
// messages: streams, work items, attachment records and adapter protocol
// state. Each round-trips every field, writes under a compare-and-set on its
// revision, and is found only under its full ownership chain.
var resourceCases = []storeCase{
	{"StreamRoundTripAndCAS", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		ref := model.StreamRef{Conversation: conv, ID: e.id(t)}
		op1 := model.OperationRef{Tenant: tenantA, ID: e.id(t)}
		op2 := model.OperationRef{Tenant: tenantA, ID: e.id(t)}
		st := model.Stream{
			Ref: ref, Mode: model.StreamNative, State: model.StreamOpen,
			Audience:     model.Audience{Visibility: model.Internal, PolicyRef: "staff"},
			NextSequence: 1, LastOperation: &op1, ProviderStreamRef: "provider-stream-1",
			ProviderReferences: []model.ExternalRef{e.externalRef("message", "pm-1")},
			Revision:           "s1",
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutStream(e.ctx, st, "") })
		e.wantStream(t, st)

		next := st.Clone()
		next.NextSequence, next.AppliedSequence, next.LastOperation, next.Revision = 2, 1, &op2, "s2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutStream(e.ctx, next, "s1") })

		// A writer that read the stream before the advance cannot overwrite
		// it: the sequence moves only under the revision it was read at.
		stale := st.Clone()
		stale.NextSequence, stale.Revision = 9, "s3"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutStream(e.ctx, stale, "s1") }), model.Conflict)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutStream(e.ctx, stale, "") }), model.Conflict)
		e.wantStream(t, next)

		other := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Stream(e.ctx, model.StreamRef{Conversation: other, ID: ref.ID})
			wantCode(t, err, model.NotFound)
			return nil
		})
		orphan := st.Clone()
		orphan.Ref = model.StreamRef{Conversation: other, ID: e.id(t)}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutStream(e.ctx, orphan, "") }), model.NotFound)
		e.wantForeign(t, func(r store.Reader) error { _, err := r.Stream(e.ctx, ref); return err })
	}},
	{"WorkItemRoundTripAndFilters", func(t *testing.T, e *env) {
		c1, c2 := e.putConversation(t), e.putConversation(t)
		w1 := e.workItem(model.WorkItemRef{Conversation: c1, ID: e.id(t)}, model.WorkItemOpen, "w1")
		w2 := e.workItem(model.WorkItemRef{Conversation: c2, ID: e.id(t)}, model.WorkItemClosed, "w1")
		e.mustTx(t, func(tx store.Tx) error {
			return firstErr(tx.PutWorkItem(e.ctx, w1, ""), tx.PutWorkItem(e.ctx, w2, ""))
		})
		e.wantWorkItem(t, w1)

		e.wantWorkItems(t, model.Filter{Conversation: &c1}, w1)
		e.wantWorkItems(t, model.Filter{Conversation: &c2}, w2)
		e.wantWorkItems(t, model.Filter{Statuses: []string{model.WorkItemClosed}}, w2)
		e.wantWorkItems(t, model.Filter{Inboxes: []model.InboxRef{e.inbox}}, w1, w2)
		e.wantWorkItems(t, model.Filter{Inboxes: []model.InboxRef{{Tenant: tenantA, ID: e.id(t)}}})
		e.wantWorkItems(t, model.Filter{Conversation: &c1, Statuses: []string{model.WorkItemClosed}})

		// A local status change keeps the binding history and local fields.
		next := w1.Clone()
		next.Status, next.Revision = model.WorkItemPending, "w2"
		next.History = append(next.History, model.WorkItemBindingChange{
			From: model.WorkItemBindingActive, To: model.WorkItemBindingMoved,
			OccurredAt: time.Unix(1_700_000_100, 0).UTC(), SourceRevision: "src-2",
		})
		e.mustTx(t, func(tx store.Tx) error { return tx.PutWorkItem(e.ctx, next, "w1") })
		stale := w1.Clone()
		stale.Status, stale.Revision = model.WorkItemResolved, "w3"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutWorkItem(e.ctx, stale, "w1") }), model.Conflict)
		e.wantWorkItem(t, next)
		e.wantWorkItems(t, model.Filter{Statuses: []string{model.WorkItemOpen}})
		e.wantWorkItems(t, model.Filter{Statuses: []string{model.WorkItemPending}}, next)

		other := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.GetWorkItem(e.ctx, model.WorkItemRef{Conversation: c2, ID: w1.Ref.ID})
			wantCode(t, err, model.NotFound)
			return nil
		})
		orphan := e.workItem(model.WorkItemRef{Conversation: other, ID: e.id(t)}, model.WorkItemOpen, "w1")
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutWorkItem(e.ctx, orphan, "") }), model.NotFound)
		e.wantForeign(t, func(r store.Reader) error { _, err := r.GetWorkItem(e.ctx, w1.Ref); return err })
	}},
	{"WorkItemOnePerConversation", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		first := e.workItem(model.WorkItemRef{Conversation: conv, ID: e.id(t)}, model.WorkItemOpen, "w1")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutWorkItem(e.ctx, first, "") })
		// A second work item for the conversation is rejected, and the unit
		// that tried it writes nothing.
		second := e.workItem(model.WorkItemRef{Conversation: conv, ID: e.id(t)}, model.WorkItemOpen, "w1")
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		err := e.txErr(func(tx store.Tx) error {
			if err := tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), ""); err != nil {
				return err
			}
			return tx.PutWorkItem(e.ctx, second, "")
		})
		wantCode(t, err, model.Conflict)
		e.requireAbsent(t, ch)
		e.mustView(t, func(r store.Reader) error {
			_, err := r.GetWorkItem(e.ctx, second.Ref)
			wantCode(t, err, model.NotFound)
			return nil
		})
		e.wantWorkItems(t, model.Filter{Conversation: &conv}, first)
		// The bound work item itself still updates.
		next := first.Clone()
		next.Subject, next.Revision = "Build fails on main", "w2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutWorkItem(e.ctx, next, "w1") })
		e.wantWorkItem(t, next)
	}},
	{"AttachmentRecordLifecycle", func(t *testing.T, e *env) {
		ref := model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)}
		prepared := store.AttachmentRecord{
			Attachment: model.Attachment{Ref: ref, Revision: "a1", Metadata: model.AttachmentMetadata{
				Name: "report.pdf", MediaType: "application/pdf", Size: 42,
				Audience: model.Audience{Visibility: model.Restricted, PolicyRef: "legal"},
			}},
			State: "prepared",
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutAttachment(e.ctx, prepared, "") })
		e.wantAttachment(t, prepared)

		// Staging binds the blob, the provider locator and the staging
		// operation to the record.
		provider := e.externalRef("file", "pf-1")
		stored := prepared.Clone()
		stored.Attachment.Revision = "a2"
		stored.Attachment.Metadata.Digest = "sha256:" + strings.Repeat("0123456789abcdef", 4)
		stored.Blob = &ports.BlobKey{Tenant: tenantA, ID: "blob-1"}
		stored.Provider = &provider
		stored.StagingOperation = model.OperationRef{Tenant: tenantA, ID: e.id(t)}
		stored.State = "stored"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutAttachment(e.ctx, stored, "a1") })
		again := prepared.Clone()
		again.Attachment.Revision = "a3"
		again.Blob = &ports.BlobKey{Tenant: tenantA, ID: "blob-2"}
		again.State = "stored"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutAttachment(e.ctx, again, "a1") }), model.Conflict)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutAttachment(e.ctx, again, "") }), model.Conflict)
		e.wantAttachment(t, stored)

		otherInbox := model.InboxRef{Tenant: tenantA, ID: e.id(t)}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Attachment(e.ctx, model.AttachmentRef{Inbox: otherInbox, ID: ref.ID})
			wantCode(t, err, model.NotFound)
			return nil
		})
		orphan := prepared.Clone()
		orphan.Attachment.Ref = model.AttachmentRef{Inbox: otherInbox, ID: e.id(t)}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutAttachment(e.ctx, orphan, "") }), model.NotFound)
		foreignBlob := prepared.Clone()
		foreignBlob.Attachment.Ref = model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)}
		foreignBlob.Blob = &ports.BlobKey{Tenant: tenantB, ID: "blob-3"}
		if err := e.txErr(func(tx store.Tx) error { return tx.PutAttachment(e.ctx, foreignBlob, "") }); err == nil {
			t.Fatal("attachment bound to another tenant's blob was stored")
		}
		e.wantForeign(t, func(r store.Reader) error { _, err := r.Attachment(e.ctx, ref); return err })
	}},
	{"ProtocolStatePerChannel", func(t *testing.T, e *env) {
		second := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, e.channelRecord(second, "r1"), "") })
		mk := func(ch model.ChannelRef, data string, rev model.Revision) store.ProtocolState {
			return store.ProtocolState{
				Channel: ch, Key: "cursor", Revision: rev,
				Value: model.Extension{Name: "conformance.chat.cursor", Version: 2, Data: []byte(data)},
			}
		}
		a, b := mk(e.channel, `{"at":1}`, "p1"), mk(second, `{"at":7}`, "q1")
		e.mustTx(t, func(tx store.Tx) error {
			return firstErr(tx.PutProtocol(e.ctx, a, ""), tx.PutProtocol(e.ctx, b, ""))
		})
		// One key per channel instance: writing one leaves the other.
		a2 := mk(e.channel, `{"at":2}`, "p2")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutProtocol(e.ctx, a2, "p1") })
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutProtocol(e.ctx, mk(second, `{"at":8}`, "q2"), "p1") }), model.Conflict)
		e.mustView(t, func(r store.Reader) error {
			for _, want := range []store.ProtocolState{a2, b} {
				got, err := r.Protocol(e.ctx, want.Channel, want.Key)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got, want) {
					wantEqual(t, "protocol state of channel "+string(want.Channel.ID), got, want)
				}
			}
			return nil
		})
		missing := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutProtocol(e.ctx, mk(missing, `{}`, "p1"), "") }), model.NotFound)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutProtocol(e.ctx, mk(e.channel, `{}`, "p9"), "") }), model.Conflict)
		e.wantForeign(t, func(r store.Reader) error { _, err := r.Protocol(e.ctx, e.channel, "cursor"); return err })
	}},
}

// externalRef is a provider reference on the env's channel.
func (e *env) externalRef(kind, id string) model.ExternalRef {
	return model.ExternalRef{Channel: e.channel, Namespace: "conformance.chat", Kind: kind, ID: id, ParentID: "room-1"}
}

// workItem is a work item with every field set, local fields and binding
// history included.
func (e *env) workItem(ref model.WorkItemRef, status string, rev model.Revision) model.WorkItem {
	successor := e.externalRef("work_item", "issue-8")
	return model.WorkItem{
		Ref: ref, External: e.externalRef("work_item", "issue-"+string(ref.ID)),
		Subject: "Build fails", URL: "https://tracker.example/issue", Status: status, RawStatus: "raw-" + status,
		Requester: e.externalRef("user", "reporter"),
		Assignees: []model.ExternalRef{e.externalRef("user", "triager")},
		Fields:    []model.Extension{{Name: "conformance.tracker.labels", Version: 1, Data: []byte(`["bug"]`)}},
		Binding:   model.WorkItemBindingActive, Successor: &successor,
		History: []model.WorkItemBindingChange{{
			From: model.WorkItemBindingMerged, To: model.WorkItemBindingActive, Successor: &successor,
			OccurredAt: time.Unix(1_700_000_000, 0).UTC(), SourceRevision: "src-1",
		}},
		LocalFields: []string{"status", "assignees"},
		Source:      model.WorkItemSource{Revision: "src-1", OccurredAt: time.Unix(1_700_000_000, 0).UTC()},
		Revision:    rev,
	}
}

func (e *env) wantStream(t *testing.T, want model.Stream) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.Stream(e.ctx, want.Ref)
		if err == nil && !reflect.DeepEqual(got, want) {
			wantEqual(t, "stream "+string(want.Ref.ID), got, want)
		}
		return err
	})
}

func (e *env) wantWorkItem(t *testing.T, want model.WorkItem) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.GetWorkItem(e.ctx, want.Ref)
		if err == nil && !reflect.DeepEqual(got, want) {
			wantEqual(t, "work item "+string(want.Ref.ID), got, want)
		}
		return err
	})
}

// wantWorkItems lists work items under f, paging one at a time, and
// requires exactly want in insertion order.
func (e *env) wantWorkItems(t *testing.T, f model.Filter, want ...model.WorkItem) {
	t.Helper()
	var got []model.WorkItem
	req := model.ListRequest{Filter: f, Page: page(1)}
	for range len(want) + 2 {
		var p model.Page[model.WorkItem]
		e.mustView(t, func(r store.Reader) error {
			var err error
			p, err = r.WorkItems(e.ctx, req)
			return err
		})
		got = append(got, p.Items...)
		if p.Complete {
			break
		}
		req.Page.After = p.Next
	}
	if len(got) != len(want) {
		t.Fatalf("work items under %s = %d, want %d", brief(f), len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			wantEqual(t, fmt.Sprintf("work item %d under %s", i, brief(f)), got[i], want[i])
		}
	}
}

func (e *env) wantAttachment(t *testing.T, want store.AttachmentRecord) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.Attachment(e.ctx, want.Attachment.Ref)
		if err == nil && !reflect.DeepEqual(got, want) {
			wantEqual(t, "attachment record "+string(want.Attachment.Ref.ID), got, want)
		}
		return err
	})
}

// wantForeign requires read, run in tenantB against a tenantA reference, to
// fail with model.NotFound.
func (e *env) wantForeign(t *testing.T, read func(store.Reader) error) {
	t.Helper()
	err := e.s.View(e.ctx, tenantB, read)
	wantCode(t, err, model.NotFound)
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
