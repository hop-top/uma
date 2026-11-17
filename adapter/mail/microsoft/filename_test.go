package microsoft

import "testing"

// A sender's filename is whatever Graph reports; a path-bearing or
// control-laden one reaches neither the descriptor nor the staged name.
func TestGraphAttachmentNamesAreSanitized(t *testing.T) {
	e := newEnv(t, Config{})
	hostile := []string{"../../etc/passwd", `..\..\x.txt`, "/abs/path/report.pdf", "invoice\u202egpj.exe", "..", "a\r\nX-Evil: 1.txt"}
	want := []string{"passwd", "x.txt", "report.pdf", "invoicegpj.exe", "attachment", "aX-Evil: 1.txt"}
	var atts []gAttachment
	for i, n := range hostile {
		id := "att-" + string(rune('a'+i))
		atts = append(atts, gAttachment{ODataType: fileType, ID: id, Name: n, ContentType: "application/pdf", Size: 5})
		e.g.files[id] = []byte("bytes")
	}
	e.g.deliver(e.box.name, gMessage{
		ID: "m1", InternetMessageID: "<m1@peer.test>", Subject: "files",
		From: &gRecipient{EmailAddress: gEmailAddress{Address: "peer@example.test"}},
		Body: &gItemBody{ContentType: "text", Content: "see attached"},
	}, atts...)
	s := &stagingSink{}
	in, err := fetchWith(t, e, s, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.reqs) != len(want) {
		t.Fatalf("staged %d, want %d", len(s.reqs), len(want))
	}
	ds := described(t, in.Envelope.Content)
	for i, w := range want {
		wantDescribed := w
		if w == "attachment" {
			wantDescribed = ""
		}
		if s.reqs[i].Metadata.Name != w || ds[i].Name != wantDescribed {
			t.Errorf("attachment %d (%q): staged as %q, described %q; want %q, %q", i, hostile[i], s.reqs[i].Metadata.Name, ds[i].Name, w, wantDescribed)
		}
	}
}
