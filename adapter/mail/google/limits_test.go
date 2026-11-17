package google

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// nested is a message whose text sits levels multiparts deep.
func nested(msgID string, levels int) []byte {
	var b strings.Builder
	b.WriteString("From: jane@example.org\r\nTo: support@example.test\r\nSubject: deep\r\nDate: Sun, 11 Oct 2026 12:00:00 +0000\r\nMessage-ID: " + msgID + "\r\nMIME-Version: 1.0\r\n")
	for i := range levels {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"n%d\"\r\n\r\n--n%d\r\n", i, i)
	}
	b.WriteString("Content-Type: text/plain\r\n\r\nthe body\r\n")
	for i := levels - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "--n%d--\r\n", i)
	}
	return []byte(b.String())
}

// put stores raw as message id of the inbox, bypassing the fake's own parse,
// which has the default limits too.
func put(e *env, id string, raw []byte) {
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	e.box.messages[id] = &fakeMsg{id: id, threadID: "thread-1", labels: []string{labelInbox}, raw: raw, internal: testNow}
}

// A message past a MIME structure limit is delivered with its header and
// what was read before the limit, and the payload says parts were omitted:
// it is not skipped.
func TestMessagePastAStructureLimitIsDeliveredPartial(t *testing.T) {
	var names []string
	for i := range 400 {
		names = append(names, fmt.Sprintf("f%03d.bin", i))
	}
	for name, raw := range map[string][]byte{
		"parts": withAttachments("<many@example.org>", names...),
		"depth": nested("<deep@example.org>", 40),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, Config{})
			put(e, "m1", raw)
			in, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), &stagingSink{}, "m1")
			if err != nil || skip {
				t.Fatalf("fetchIngress = skip %v, %v; want the message delivered", skip, err)
			}
			var p messagePayload
			if err := json.Unmarshal(in.Envelope.Payload.Data, &p); err != nil {
				t.Fatal(err)
			}
			if !p.PartsOmitted {
				t.Errorf("payload %s does not say parts were omitted", in.Envelope.Payload.Data)
			}
			if err := in.Envelope.Content.Validate(); err != nil {
				t.Errorf("content: %v", err)
			}
			if name == "parts" && (len(in.Envelope.Content.Parts) == 0 || in.Envelope.Content.Parts[0].Text != "please see attached") {
				t.Errorf("content %+v, want the text read before the limit", in.Envelope.Content.Parts)
			}
		})
	}
}

// A message within the limits says nothing of omitted parts, and one that
// the parser cannot read at all is still skipped.
func TestStructureWithinLimitsOmitsNothing(t *testing.T) {
	var names []string
	for i := range 150 {
		names = append(names, fmt.Sprintf("f%03d.bin", i))
	}
	e := newEnv(t, Config{})
	e.g.deliver(e.box.user, "m1", "thread-1", withAttachments("<digest@example.org>", names...))
	in, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), &stagingSink{}, "m1")
	if err != nil || skip {
		t.Fatalf("fetchIngress = skip %v, %v", skip, err)
	}
	var p messagePayload
	if err := json.Unmarshal(in.Envelope.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.PartsOmitted || len(descriptors(t, in.Envelope.Content))+p.AttachmentsOmitted != 150 {
		t.Errorf("payload %s: parts omitted or attachments lost", in.Envelope.Payload.Data)
	}
}
