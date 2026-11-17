package gitlab_test

import (
	"strconv"
	"testing"
	"time"

	"hop.top/uma/adapter"
	gitlab "hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/model"
)

func (r *rig) refetch(target model.ExternalRef, refs ...model.ExternalRef) (model.Envelope, error) {
	return r.inst.(adapter.Refetcher).Refetch(r.t.Context(), adapter.RefetchRequest{Target: target, References: refs})
}

func TestRefetchWorkItem(t *testing.T) {
	r := newRig(t)
	r.gl.setIssue("a", sandboxProject, 7, 5000001, "closed", t0.Add(-time.Hour))
	env, err := r.refetch(issueRoute("7"))
	if err != nil {
		t.Fatal(err)
	}
	if env.Kind != model.EnvelopeWorkItem || env.WorkItem == nil || env.WorkItem.Status != model.WorkItemClosed || env.WorkItem.RawStatus != "closed" ||
		env.Destination != sandboxPath || env.SourceRevision != "2026-10-11T11:00:00Z" {
		t.Fatalf("envelope = %+v state %+v", env, env.WorkItem)
	}
}

func TestRefetchNoteAndDescription(t *testing.T) {
	r := newRig(t)
	n := r.gl.seed("a", sandboxProject, 7, 5000001, "edited text", t0.Add(-time.Hour))
	id := "note:" + strconv.FormatInt(n.ID, 10)
	env, err := r.refetch(model.ProviderMessageRef(chanA, id), issueRoute("7"))
	if err != nil {
		t.Fatal(err)
	}
	if env.Kind != model.EnvelopeMessageEdited || env.MessageID != id || env.Content.Parts[0].Text != "edited text" ||
		env.Sender.ID != "5000001" || env.Destination != sandboxPath || env.References[0] != issueRoute("7") {
		t.Fatalf("envelope = %+v", env)
	}
	iss, _ := r.gl.issue("a", sandboxProject, 7)
	iss.Description = "Body.\n\n<!-- uma:" + string(opA.ID) + ":" + string(tryOne) + " -->"
	r.gl.setIssue("a", sandboxProject, 7, 5000001, "opened", t0)
	r.gl.edit("a", sandboxProject, 7, iss.Description)
	env, err = r.refetch(model.ProviderMessageRef(chanA, "issue:3000007"), issueRoute("7"))
	if err != nil || env.Kind != model.EnvelopeMessageEdited || env.Content.Parts[0].Text != "Body." {
		t.Fatalf("description = %+v, %v; want the text without the marker", env, err)
	}
	_, err = r.refetch(model.ProviderMessageRef(chanA, "issue:99"), issueRoute("7"))
	wantCode(t, err, model.NotFound)
}

func TestRefetchRefusals(t *testing.T) {
	r := newRig(t)
	for name, fn := range map[string]func() error{
		"no work item reference": func() error { _, err := r.refetch(model.ProviderMessageRef(chanA, "note:1")); return err },
		"other channel": func() error {
			_, err := r.refetch(model.ProviderMessageRef(model.ChannelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000c0a2"}, "note:1"), issueRoute("7"))
			return err
		},
		"bad note id": func() error {
			_, err := r.refetch(model.ProviderMessageRef(chanA, "note:x"), issueRoute("7"))
			return err
		},
		"other kind": func() error {
			_, err := r.refetch(model.ProviderMessageRef(chanA, "thing:1"), issueRoute("7"))
			return err
		},
	} {
		if err := fn(); err == nil {
			t.Errorf("%s: want an error", name)
		} else {
			wantCode(t, err, model.Validation)
		}
	}
	r.gl.respond("a", statusAnswer(404, notFound, nil))
	_, err := r.refetch(issueRoute("7"))
	wantCode(t, err, model.NotFound)
	r.gl.respond("a", statusAnswer(503, "", nil))
	_, err = r.refetch(issueRoute("7"))
	wantCode(t, err, model.Transient)
	_ = gitlab.Type
}
