package mime_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"hop.top/uma/adapter/mail/mime"
)

// nestedMail is a message whose body text sits levels multiparts deep.
func nestedMail(levels int) string {
	var b strings.Builder
	b.WriteString("From: a@example.org\r\nTo: b@example.org\r\nMIME-Version: 1.0\r\n")
	for i := range levels {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"b%d\"\r\n\r\n--b%d\r\n", i, i)
	}
	b.WriteString("Content-Type: text/plain\r\n\r\nhello body\r\n")
	for i := levels - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "--b%d--\r\n", i)
	}
	return b.String()
}

// digestMail is a text part followed by n small inline images.
func digestMail(n int) string {
	var b strings.Builder
	b.WriteString("From: a@example.org\r\nTo: b@example.org\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"b\"\r\n\r\n")
	b.WriteString("--b\r\nContent-Type: text/plain\r\n\r\nhello body\r\n")
	for i := range n {
		fmt.Fprintf(&b, "--b\r\nContent-Type: image/png; name=\"i%d.png\"\r\nContent-Disposition: inline\r\nContent-ID: <i%d@x>\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw==\r\n", i, i)
	}
	b.WriteString("--b--\r\n")
	return b.String()
}

// forwardChain is a message forwarded as an attachment (message/rfc822)
// levels times over.
func forwardChain(levels int) string {
	inner := "From: z@example.org\r\nSubject: orig\r\nContent-Type: text/plain\r\n\r\noriginal\r\n"
	for i := range levels {
		inner = fmt.Sprintf("From: a@example.org\r\nSubject: Fwd %d\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"f%d\"\r\n\r\n--f%d\r\nContent-Type: text/plain\r\n\r\nsee below\r\n--f%d\r\nContent-Type: message/rfc822\r\nContent-Disposition: attachment\r\n\r\n%s\r\n--f%d--\r\n", i, i, i, i, inner, i)
	}
	return inner
}

// Ordinary mail is admitted with headroom: a body nested as deep as the
// clients nest it and well past, a forward chain of any length (the
// forwarded message is one part, not walked) and a digest of a few hundred
// parts.
func TestRealWorldShapesAreWithinTheDefaultLimits(t *testing.T) {
	for name, c := range map[string]struct {
		raw         string
		attachments int
	}{
		"outlook nesting, 5 levels":     {nestedMail(5), 0},
		"signed forward, 8 levels":      {nestedMail(8), 0},
		"nesting, 12 levels (was over)": {nestedMail(12), 0},
		"nesting at the limit":          {nestedMail(mime.DefaultMaxDepth), 0},
		"forward chain, 8 levels":       {forwardChain(8), 1},
		"forward chain, 100 levels":     {forwardChain(100), 1},
		"digest of 150 parts":           {digestMail(150), 150},
		"digest at the limit":           {digestMail(mime.DefaultMaxParts - 2), mime.DefaultMaxParts - 2},
		"digest of 99 parts (was over)": {digestMail(99), 99},
	} {
		t.Run(name, func(t *testing.T) {
			m, err := mime.Parse(strings.NewReader(c.raw), mime.Limits{})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if m.PartsOmitted || m.Text == "" || len(m.Attachments) != c.attachments {
				t.Fatalf("text %q, %d attachments, omitted %v; want text, %d attachments", m.Text, len(m.Attachments), m.PartsOmitted, c.attachments)
			}
		})
	}
}

// A message past a structure limit is a partial message with ErrLimit, not
// nothing: the header, and the text and parts read before the limit. The
// parts after it are not read, so a bomb costs what the limit allows.
func TestStructureLimitsReturnThePartialMessage(t *testing.T) {
	for name, c := range map[string]struct {
		raw         string
		text        string
		attachments int
	}{
		"parts": {digestMail(mime.DefaultMaxParts), "hello body", mime.DefaultMaxParts - 2},
		"depth": {nestedMail(mime.DefaultMaxDepth + 1), "", 0},
		// 100000 parts, 14 MiB: stopped after the limit.
		"parts bomb": {digestMail(100000), "hello body", mime.DefaultMaxParts - 2},
		"depth bomb": {nestedMail(5000), "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			m, err := mime.Parse(strings.NewReader(c.raw), mime.Limits{})
			if !errors.Is(err, mime.ErrLimit) || !errors.Is(err, mime.ErrMalformed) {
				t.Fatalf("Parse = %v, want ErrLimit wrapping ErrMalformed", err)
			}
			if m == nil || !m.PartsOmitted {
				t.Fatalf("message = %+v, want a partial message with PartsOmitted", m)
			}
			if len(m.Header.From) != 1 || m.Text != c.text || len(m.Attachments) != c.attachments {
				t.Fatalf("from %v, text %q, %d attachments; want one from, %q, %d", m.Header.From, m.Text, len(m.Attachments), c.text, c.attachments)
			}
		})
	}
}

// Other malformed messages still come with no message, and are not limits.
func TestOtherMalformedMessagesAreNotLimits(t *testing.T) {
	m, err := mime.Parse(strings.NewReader("From: a@example.org\r\nX-Long: "+strings.Repeat("a", 200)+"\r\n\r\nbody\r\n"), mime.Limits{MaxHeaderBytes: 64})
	if !errors.Is(err, mime.ErrMalformed) || errors.Is(err, mime.ErrLimit) || m != nil {
		t.Fatalf("Parse = %+v, %v; want ErrMalformed only, no message", m, err)
	}
}
