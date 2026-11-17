package imap_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// structured is a message of parts: a text part, then n attachments with
// the file name of the given length, inside levels nested multiparts.
func structured(id string, levels, n, nameLen int) []byte {
	return structuredIDs(id, levels, n, nameLen, 0)
}

// structuredIDs is structured with a Content-ID of cidLen characters on each
// attachment (none when 0). File names are cut to 255 bytes when recorded, so
// the descriptors of a long message grow past the structured part limit by
// their Content-IDs.
func structuredIDs(id string, levels, n, nameLen, cidLen int) []byte {
	var b strings.Builder
	b.WriteString("From: Alice <" + peer + ">\r\nTo: " + mailUser + "\r\nSubject: parts\r\nDate: Sun, 11 Oct 2026 09:00:00 +0000\r\nMessage-ID: " + id + "\r\nMIME-Version: 1.0\r\n")
	for i := range levels {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"n%d\"\r\n\r\n--n%d\r\n", i, i)
	}
	b.WriteString("Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nthe body\r\n")
	for i := range n {
		name := fmt.Sprintf("%0*d", nameLen, i)
		cid := ""
		if cidLen > 0 {
			cid = fmt.Sprintf("Content-ID: <%0*d@x>\r\n", cidLen, i)
		}
		fmt.Fprintf(&b, "--b\r\nContent-Type: application/octet-stream; name=%q\r\nContent-Disposition: attachment; filename=%q\r\n%sContent-Transfer-Encoding: base64\r\n\r\naGk=\r\n", name, name, cid)
	}
	b.WriteString("--b--\r\n")
	for i := levels - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "--n%d--\r\n", i)
	}
	return []byte(b.String())
}

// delivered runs the receiver over raw followed by a sentinel and returns the
// envelope raw became, failing when the sentinel arrives alone.
func delivered(t *testing.T, raw []byte) model.Envelope {
	t.Helper()
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", raw, time.Now().Add(-time.Minute))
	srv.deliver("INBOX", mail("<end@example.org>", "end"), time.Now().Add(-time.Minute))
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	s := newSink()
	run(t, inst, s)
	var envs []model.Envelope
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if envs = s.envelopes(); slices.Contains(messageIDs(envs), "<end@example.org>") {
			break
		}
	}
	if len(envs) == 0 || envs[0].MessageID == "<end@example.org>" {
		t.Fatalf("the message was dropped: the sink saw %v", messageIDs(envs))
	}
	return envs[0]
}

type partsPayload struct {
	PartsOmitted bool `json:"parts_omitted"`
	Text         struct {
		BodyOmitted bool `json:"body_omitted"`
	} `json:"text"`
}

type partsPart struct {
	Attachments        []struct{} `json:"attachments"`
	AttachmentsOmitted int        `json:"attachments_omitted"`
}

func decodeParts(t *testing.T, e model.Envelope) (p partsPayload, parts partsPart, text string) {
	t.Helper()
	if err := json.Unmarshal(e.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	for _, part := range e.Content.Parts {
		switch part.Kind {
		case model.PartText:
			text = part.Text
		case model.PartStructured:
			if part.Structured.Name == "imap.mail.parts" {
				if err := json.Unmarshal(part.Structured.Data, &parts); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return p, parts, text
}

// A digest of 150 parts, which the limits used to refuse, is delivered whole.
func TestReceiveDigestOfManyPartsIsDelivered(t *testing.T) {
	p, parts, text := decodeParts(t, delivered(t, structured("<digest@example.org>", 0, 150, 8)))
	if p.PartsOmitted || text != "the body" || len(parts.Attachments) != 150 || parts.AttachmentsOmitted != 0 {
		t.Fatalf("omitted %v, text %q, %d parts, %d not described", p.PartsOmitted, text, len(parts.Attachments), parts.AttachmentsOmitted)
	}
}

// A message past a structure limit is delivered with the header, the text
// and the parts read before the limit, and says so: it is not dropped.
func TestReceiveMessagePastAStructureLimitIsDeliveredPartial(t *testing.T) {
	for name, raw := range map[string][]byte{
		"parts": structured("<manyparts@example.org>", 0, 400, 8),
		"depth": structured("<deep@example.org>", 40, 3, 8),
	} {
		t.Run(name, func(t *testing.T) {
			e := delivered(t, raw)
			p, _, text := decodeParts(t, e)
			if !p.PartsOmitted {
				t.Errorf("payload %s does not say parts were omitted", e.Payload.Data)
			}
			if name == "parts" && text != "the body" {
				t.Errorf("text %q, want the text read before the limit", text)
			}
			if err := e.Content.Validate(); err != nil {
				t.Errorf("content: %v", err)
			}
		})
	}
}

// The descriptors of the attachments of one message share the structured
// part limit: past it the descriptors are counted, not the message dropped.
func TestReceiveDescriptorsPastTheContentLimitAreCounted(t *testing.T) {
	e := delivered(t, structuredIDs("<longnames@example.org>", 0, 250, 255, 1000))
	p, parts, text := decodeParts(t, e)
	if text != "the body" || p.PartsOmitted || parts.AttachmentsOmitted == 0 || len(parts.Attachments)+parts.AttachmentsOmitted != 250 {
		t.Fatalf("text %q, %d described + %d omitted, want 250 in all", text, len(parts.Attachments), parts.AttachmentsOmitted)
	}
	if err := e.Content.Validate(); err != nil {
		t.Errorf("content: %v", err)
	}
}
