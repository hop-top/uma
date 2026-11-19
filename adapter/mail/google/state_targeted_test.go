package google

import (
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// A targeted change is one users.messages call per named message and never
// a threads call: m2, in the thread of m1, is left alone.
func TestTargetedStateNeverWidensToTheThread(t *testing.T) {
	for _, c := range stateCasesFor(true) {
		t.Run(c.name, func(t *testing.T) {
			e := threadEnv(t)
			if c.name == "untrash" || c.name == "unflag star" || c.name == "unflag important" {
				e.g.relabelThread(testMailbox, "t1", []string{"TRASH", "STARRED", "IMPORTANT"}, nil)
			}
			// messages.modify, unlike the thread call in the fake, knows only labels a message holds.
			e.g.relabelThread(testMailbox, "t2", []string{"Label_7", "Label_9"}, nil)
			before := slices.Clone(e.labelsOf("m2"))
			before3 := slices.Clone(e.labelsOf("m3"))
			reqs0 := len(e.g.requests())
			out, err := c.call(e.inst, items("m1"))
			if err != nil || out.State != model.ProviderAccepted {
				t.Fatalf("outcome = %+v, %v; want provider_accepted", out, err)
			}
			got := e.g.requests()[reqs0:]
			want := "POST " + mbox + "/messages/m1/" + c.want
			if !slices.Equal(got, []string{want}) {
				t.Fatalf("requests = %q, want exactly %q", got, want)
			}
			if len(e.box.threadOps) != 0 {
				t.Fatalf("thread requests = %q, want none", e.box.threadOps)
			}
			c.after(t, e, "m1")
			if after := e.labelsOf("m2"); !slices.Equal(after, before) {
				t.Errorf("m2, same thread, changed: %v -> %v", before, after)
			}
			if after := e.labelsOf("m3"); !slices.Equal(after, before3) {
				t.Errorf("m3 changed: %v -> %v", before3, after)
			}
		})
	}
}

// Several targeted messages are one call each, in order, each once.
func TestTargetedStateOneCallPerMessage(t *testing.T) {
	e := threadEnv(t)
	out, err := e.inst.Archive(t.Context(), adapter.ArchiveRequest{
		Effect: effect(route(testPeer, "m1"), "a"), Items: items("m1", "m3", "m1"), Targeted: true,
	})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Archive = %+v, %v", out, err)
	}
	var posts []string
	for _, r := range e.g.requests() {
		if strings.HasPrefix(r, "POST ") {
			posts = append(posts, strings.TrimPrefix(r, "POST "+mbox))
		}
	}
	if want := []string{"/messages/m1/modify", "/messages/m3/modify"}; !slices.Equal(posts, want) {
		t.Fatalf("posts = %q, want %q", posts, want)
	}
	if e.has("m1", "INBOX") || e.has("m3", "INBOX") || !e.has("m2", "INBOX") {
		t.Errorf("labels m1=%v m2=%v m3=%v", e.labelsOf("m1"), e.labelsOf("m2"), e.labelsOf("m3"))
	}
}

// A targeted change with no usable item is refused before any call, and
// never falls back to the route's message.
func TestTargetedStateRefusals(t *testing.T) {
	e := threadEnv(t)
	eff := effect(route(testPeer, "m1"), "a")
	reqs0 := len(e.g.requests())
	for _, it := range [][]model.ExternalRef{nil, {model.ProviderMessageRef(model.ChannelRef{Tenant: testChan.Tenant, ID: "other"}, "m1")}} {
		out, err := e.inst.Archive(t.Context(), adapter.ArchiveRequest{Effect: eff, Items: it, Targeted: true})
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
			t.Fatalf("Archive(%v) = %+v, %v; want rejected validation", it, out, err)
		}
	}
	if n := len(e.g.requests()); n != reqs0 {
		t.Fatalf("%d requests made for refused changes", n-reqs0)
	}
}

// A lost targeted change reconciles on the targeted messages only: a
// sibling in the thread that does not show the change does not fail it.
func TestTargetedStateReconcilesTheMessagesOnly(t *testing.T) {
	e := threadEnv(t)
	if _, err := e.inst.Archive(t.Context(), adapter.ArchiveRequest{Effect: effect(route(testPeer, "m1"), "a"), Items: items("m1"), Targeted: true}); err != nil {
		t.Fatal(err)
	}
	req := adapter.ReconcileRequest{Effect: effect(route(testPeer, "m1"), "a"), Capability: model.CapabilityArchive, Items: items("m1"), Targeted: true}
	out, err := e.inst.Reconcile(t.Context(), req)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("targeted Reconcile = %+v, %v; want provider_accepted", out, err)
	}
	req.Targeted = false
	out, err = e.inst.Reconcile(t.Context(), req)
	if err != nil || out.State != model.Failed {
		t.Fatalf("thread Reconcile = %+v, %v; want failed: m2 still in the inbox", out, err)
	}
}
