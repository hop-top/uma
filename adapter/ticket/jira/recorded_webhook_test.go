package jira_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"hop.top/uma/model"
)

// TestRecordedWebhooks replays webhook bodies captured from a sandbox site,
// one file per event in testdata/webhooks, named as below. Each is signed
// here with the fixture secret (the body is what Jira sent; the signature
// is not part of a capture), delivered to the handler, and must be accepted
// and map to the envelope kind its name says. A file that does not exist is
// skipped: capture them as docs/recording/jira.md describes.
func TestRecordedWebhooks(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
	}{
		{"issue_created", model.EnvelopeMessage},
		{"issue_updated", model.EnvelopeWorkItem},
		{"issue_deleted", model.EnvelopeWorkItem},
		{"comment_created", model.EnvelopeMessage},
		{"comment_updated", model.EnvelopeMessageEdited},
		{"comment_deleted", model.EnvelopeMessageDeleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", "webhooks", tc.name+".json")) // #nosec G304 -- fixed fixture directory
			if os.IsNotExist(err) {
				t.Skip("no captured " + tc.name + " webhook; see docs/recording/jira.md")
			}
			if err != nil {
				t.Fatal(err)
			}
			h := newHookRig(t)
			got := h.deliver(body)
			if len(got) == 0 {
				t.Fatalf("%s reported nothing", tc.name)
			}
			env := got[0]
			if env.Kind != tc.kind || env.Destination == "" || len(env.References) == 0 || env.References[0].Kind != model.RefKindWorkItem {
				t.Fatalf("%s mapped to %+v", tc.name, env)
			}
			if tc.name != "issue_deleted" && tc.name != "comment_deleted" {
				if env.SourceRevision == "" || (env.Sender.ID == "" && tc.kind != model.EnvelopeWorkItem) {
					t.Fatalf("%s lacks a revision or sender: %+v", tc.name, env)
				}
			}
			if status := h.serve(hookRequest(body)); status != http.StatusOK {
				t.Fatalf("redelivery = %d", status)
			}
		})
	}
}
