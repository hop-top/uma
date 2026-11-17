package google

import (
	"io"
	"net/http"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

// A sender's path-bearing filename reaches neither the descriptor nor the
// recorded attachment name: the base name is recorded.
func TestEngineRecordsBaseNameOfPathFilenames(t *testing.T) {
	r := newEngineRigBlobs(t, &portstest.MemoryBlobs{})
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	r.receive(testChan, testMailbox)
	r.g.deliver(testMailbox, "m1", "thread-1", withAttachments("<f1@example.org>", "../../etc/passwd", `..\..\x.txt`, "/abs/path/report.pdf"))
	if code := r.push(testChan, testMailbox); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	msg := r.waitMessages(engineInboxA, 1)[0]
	refs := attachmentRefs(msg.Content)
	want := []string{"passwd", "x.txt", "report.pdf"}
	if len(refs) != len(want) {
		t.Fatalf("staged %d, want %d", len(refs), len(want))
	}
	client, err := r.engine.For(model.Principal{Tenant: testChan.Tenant, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	ds := descriptors(t, msg.Content)
	for i, w := range want {
		att, rc, err := client.Attachments().Open(r.ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: refs[i]})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
		if att.Metadata.Name != w || ds[i].Filename != w {
			t.Errorf("attachment %d: recorded %q, described %q, want %q", i, att.Metadata.Name, ds[i].Filename, w)
		}
	}
}
