package imap_test

import (
	"io"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

// A sender's path-bearing filename reaches neither the descriptor nor the
// recorded attachment name: the base name is recorded.
func TestEngineRecordsBaseNameOfPathFilenames(t *testing.T) {
	r := newEngineRigBlobs(t, passwordHost(), nil, &portstest.MemoryBlobs{})
	r.srv.deliver("INBOX", withAttachments("<f1@example.org>", "../../etc/passwd", `..\..\x.txt`, "/abs/path/report.pdf"), time.Now())
	r.receive()
	msg := r.awaitMessages(1)[0]
	refs := attachmentRefs(msg.Content)
	want := []string{"passwd", "x.txt", "report.pdf"}
	if len(refs) != len(want) {
		t.Fatalf("staged %d, want %d", len(refs), len(want))
	}
	ds := descriptors(t, msg.Content)
	for i, w := range want {
		att, rc, err := r.client.Attachments().Open(r.ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: refs[i]})
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
