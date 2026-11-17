package services_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Operations.Get derives Pending from the intent of an unsettled mutation:
// the operation, the base revision and LastEdit, and the change. A caller
// who may not read the target sees the operation without it; once the
// operation settles there is none.
func TestOperationPendingShowsEditContent(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	a, err := w.edit(w.edited, w.cmd(w.edited.Revision), "fixed")
	if err != nil {
		t.Fatal(err)
	}
	op := w.get(a.Operation.Ref)
	p := op.Pending
	if p == nil {
		t.Fatal("queued edit shows no pending change")
	}
	if p.Operation != a.Operation.Ref || p.BaseRevision != w.edited.Revision || p.BaseLastEdit == nil ||
		*p.BaseLastEdit.Operation != *w.edited.LastEdit.Operation || p.Content == nil || p.Content.Parts[0].Text != "fixed" ||
		p.Delete || p.Reaction != nil {
		t.Fatalf("pending = %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []model.DeliveryState{model.RetryPending, model.Unknown, model.DeadLettered} {
		w.settle(a.Operation.Ref, s)
		if w.get(a.Operation.Ref).Pending == nil {
			t.Fatalf("%s edit shows no pending change", s)
		}
	}
	w.policy.hide(w.edited.Ref.Ref())
	if got := w.get(a.Operation.Ref); got.Pending != nil || got.Ref != a.Operation.Ref {
		t.Fatalf("pending revealed without message.read: %+v", got.Pending)
	}
	w.policy.hide(model.Ref{})
	w.settle(a.Operation.Ref, model.Delivered)
	if got := w.get(a.Operation.Ref); got.Pending != nil {
		t.Fatalf("settled edit still pending: %+v", got.Pending)
	}
}

// Delete and React show their own change kind.
func TestOperationPendingShowsDeleteAndReaction(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	d, err := w.remove(w.target, w.cmd(w.target.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if p := w.get(d.Operation.Ref).Pending; p == nil || !p.Delete || p.Content != nil || p.Reaction != nil || p.BaseLastEdit != nil {
		t.Fatalf("delete pending = %+v", p)
	}
	r, err := w.react(w.target, w.cmd(""), "👍", true)
	if err != nil {
		t.Fatal(err)
	}
	if p := w.get(r.Operation.Ref).Pending; p == nil || p.Reaction == nil || *p.Reaction != (model.PendingReaction{Emoji: "👍", Remove: true}) || p.Delete {
		t.Fatalf("react pending = %+v", p)
	}
}

// A target redacted after the claim shows no pending change, nor does a
// non-mutation operation.
func TestOperationPendingHiddenForRedactedTarget(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	a, err := w.edit(w.target, w.cmd(w.target.Revision), "fixed")
	if err != nil {
		t.Fatal(err)
	}
	gone := w.message(w.target.Ref)
	gone.Redacted, gone.Content, gone.Revision = true, model.Content{}, "m9"
	w.seed(tenantA, func(ctx context.Context, tx store.Tx) error { return tx.PutMessage(ctx, gone, w.target.Revision) })
	if p := w.get(a.Operation.Ref).Pending; p != nil {
		t.Fatalf("pending on a redacted target: %+v", p)
	}
	reply := w.Reply("op-reply", "hello")
	if p := w.get(reply.Operation.Ref).Pending; p != nil {
		t.Fatalf("reply shows pending: %+v", p)
	}
}

// Messages.Get and Messages.List show canonical content only while an
// edit is queued: no pending field, the original text, the revision
// unchanged.
func TestMessagesGetNeverReturnsPendingContent(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	if _, err := w.edit(w.target, w.cmd(w.target.Revision), "pending text"); err != nil {
		t.Fatal(err)
	}
	m, err := w.C.Messages().Get(w.Ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "g"}, Ref: w.target.Ref})
	if err != nil {
		t.Fatal(err)
	}
	conv := w.Conv.Ref
	page, err := w.C.Messages().List(w.Ctx, model.ListRequest{Call: model.Call{Correlation: "l"}, Filter: model.Filter{Conversation: &conv}, Page: model.PageRequest{Limit: 50}})
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, got := range append(page.Items, m) {
		if got.Ref != w.target.Ref {
			continue
		}
		listed = listed || got.Revision == w.target.Revision
		raw, _ := json.Marshal(got)
		if got.Revision != w.target.Revision || strings.Contains(string(raw), "pending") || got.Content.Parts[0].Text != w.target.Content.Parts[0].Text {
			t.Fatalf("message carries pending state: %s", raw)
		}
	}
	if !listed {
		t.Fatal("target not listed")
	}
}
