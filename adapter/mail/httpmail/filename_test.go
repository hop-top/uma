package httpmail_test

import (
	"testing"

	"hop.top/uma/adapter/mail/httpmail"
)

// A filename a provider's JSON document carries is as sender-controlled as
// one in a MIME header: the descriptor holds its base name only.
func TestJSONAttachmentNamesAreSanitized(t *testing.T) {
	cfg := baseConfig()
	cfg.JSONFields = httpmail.JSONFields{
		From: "From", To: "To", Subject: "Subject", Body: "TextBody", Attachments: "Attachments",
		AttachmentFields: httpmail.AttachmentFields{Name: "Name", MimeType: "ContentType"},
	}
	r := newRig(t, cfg)
	wantStatus(t, r.postJSON(`{
	  "From": "alice@peers.test", "To": "support@example.com", "Subject": "Invoice", "TextBody": "See attached.",
	  "Attachments": [
	    {"Name": "../../etc/passwd", "ContentType": "application/pdf"},
	    {"Name": "..\\..\\x.txt", "ContentType": "application/pdf"},
	    {"Name": "invoice\u202egpj.exe", "ContentType": "application/pdf"},
	    {"Name": "a\r\nX-Evil: 1.txt", "ContentType": "application/pdf"}
	  ]
	}`), 200)
	atts := partsOf(t, only(t, r))["attachments"].([]any)
	want := []string{"passwd", "x.txt", "invoicegpj.exe", "aX-Evil: 1.txt"}
	if len(atts) != len(want) {
		t.Fatalf("attachments %v", atts)
	}
	for i, w := range want {
		if got := atts[i].(map[string]any)["filename"]; got != w {
			t.Errorf("attachment %d filename %v, want %q", i, got, w)
		}
	}
}

// The same holds for the MIME headers of a raw message.
func TestRFC822AttachmentNamesAreSanitized(t *testing.T) {
	r := newRig(t, rawConfig())
	wantStatus(t, r.postRaw(`From: Alice <alice@peers.test>
To: support@example.com
Subject: files
Message-ID: <f1@peers.test>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary=XX

--XX
Content-Type: text/plain

See attached.
--XX
Content-Type: application/pdf
Content-Disposition: attachment; filename*=UTF-8''..%2F..%2Fetc%2Fpasswd

x
--XX--
`), 200)
	atts := partsOf(t, only(t, r))["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["filename"] != "passwd" {
		t.Errorf("attachments %v", atts)
	}
}
