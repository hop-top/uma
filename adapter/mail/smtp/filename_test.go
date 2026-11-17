package smtp_test

import (
	"testing"

	"hop.top/uma/ports/portstest"
)

// A sender's path-bearing filename reaches neither the descriptor, the
// recorded attachment name nor the blob key: the base name is recorded and
// the key is the engine's.
func TestEngineRecordsBaseNameOfPathFilenames(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	r := newEngineRigBlobs(t, blobs)
	r.seed(channelA, inboxA, "", "support")
	names := []string{"../../etc/passwd", `..\..\x.txt`, "/abs/path/report.pdf"}
	if err := r.receive(channelA).send(t, janeTx, withAttachments("<f1@example.org>", names...)); err != nil {
		t.Fatal(err)
	}
	msg := r.messages(inboxA)[0]
	refs := attachmentRefs(msg)
	want := []string{"passwd", "x.txt", "report.pdf"}
	if len(refs) != len(want) {
		t.Fatalf("staged %d, want %d", len(refs), len(want))
	}
	ds := descriptors(t, msg)
	for i, w := range want {
		att, _ := r.readAttachment(refs[i])
		if att.Metadata.Name != w || ds[i].Filename != w {
			t.Errorf("attachment %d: recorded %q, described %q, want %q", i, att.Metadata.Name, ds[i].Filename, w)
		}
	}
	// The blob keys are the engine's canonical IDs, whatever the names.
	if keys := blobs.Keys(); len(keys) != len(names) {
		t.Errorf("blob keys %+v, want %d", keys, len(names))
	} else {
		for _, k := range keys {
			if err := k.Validate(); err != nil {
				t.Errorf("key %+v: %v", k, err)
			}
		}
	}
}
