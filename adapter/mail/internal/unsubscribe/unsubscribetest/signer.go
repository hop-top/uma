// Package unsubscribetest holds the stand-ins the unsubscribe tests of the
// mail adapters share: a DKIM signer with the DNS resolver that publishes its
// key, and a message carrying the unsubscribe headers. RFC 6376
// (https://www.rfc-editor.org/rfc/rfc6376) defines the signature and key
// record; a recorded message from a real sender replaces the stand-in.
package unsubscribetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

// Selector is the DKIM selector of every signature made here.
const Selector = "s1"

// Covering is a header set that covers the unsubscribe fields.
var Covering = []string{"From", "To", "Subject", "Date", "Message-ID", "List-Unsubscribe", "List-Unsubscribe-Post", "MIME-Version", "Content-Type"}

// Signer signs messages and publishes its key.
type Signer struct {
	key *rsa.PrivateKey
	txt string
}

// NewSigner makes a signer with a fresh key.
func NewSigner(t testing.TB) *Signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &Signer{key: k, txt: "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)}
}

// Resolver answers the key record of domain's Selector and nothing else.
func (s *Signer) Resolver(domain string) func(context.Context, string) ([]string, error) {
	return func(_ context.Context, name string) ([]string, error) {
		if name == Selector+"._domainkey."+domain {
			return []string{s.txt}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
}

// Sign signs raw as domain over the header keys and returns the message with
// its DKIM-Signature field.
func (s *Signer) Sign(t testing.TB, raw string, domain string, keys []string) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := dkim.Sign(&out, strings.NewReader(raw), &dkim.SignOptions{
		Domain: domain, Selector: Selector, Signer: s.key, HeaderKeys: keys,
	}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Message is a newsletter with the given extra header lines (each without
// its line break), from news@mail.example.com.
func Message(extra ...string) string {
	var b strings.Builder
	b.WriteString("From: News <news@mail.example.com>\r\nTo: me@example.net\r\nSubject: Weekly\r\nDate: Sun, 11 Oct 2026 12:00:00 +0000\r\nMessage-ID: <n1@mail.example.com>\r\n")
	for _, h := range extra {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhello\r\n")
	return b.String()
}
