package unsubscribe

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"hop.top/uma/adapter/mail/internal/unsubscribe/unsubscribetest"
	"hop.top/uma/model"
)

const (
	rawHeaders = "From: News <news@mail.example.com>\r\nTo: me@example.net\r\nSubject: Hi\r\nDate: Mon, 02 Jan 2006 15:04:05 +0000\r\nMessage-ID: <1@mail.example.com>\r\n" +
		"List-Unsubscribe: <https://u.example.net/o?id=1>, <mailto:unsub@example.com>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n"
	rawBody = "MIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\nhello\r\n"
)

// signer adapts the shared stand-in: DKIM keys and DNS records generated
// locally (see package unsubscribetest).
type signer struct{ *unsubscribetest.Signer }

func newSigner(t *testing.T) signer { return signer{unsubscribetest.NewSigner(t)} }

func (s signer) resolver(domain string) Resolver { return s.Resolver(domain) }

func (s signer) sign(t *testing.T, domain string, keys []string) []byte {
	return s.Sign(t, rawHeaders+rawBody, domain, keys)
}

var covering = unsubscribetest.Covering

func TestEvaluateOneClickDKIM(t *testing.T) {
	s := newSigner(t)
	pub := func(string) bool { return true }
	for _, c := range []struct {
		name      string
		raw       func() []byte
		resolver  Resolver
		refuse    string
		temporary bool
	}{
		{"covered and aligned", func() []byte { return s.sign(t, "example.com", covering) }, s.resolver("example.com"), "", false},
		{"signing subdomain of the organization", func() []byte { return s.sign(t, "mail.example.com", covering) }, s.resolver("mail.example.com"), "", false},
		{"unsigned", func() []byte { return []byte(rawHeaders + rawBody) }, s.resolver("example.com"), "no valid DKIM signature", false},
		{"List-Unsubscribe-Post not covered", func() []byte {
			return s.sign(t, "example.com", []string{"From", "To", "Subject", "Date", "List-Unsubscribe", "Message-ID"})
		}, s.resolver("example.com"), "covering List-Unsubscribe and List-Unsubscribe-Post", false},
		{"List-Unsubscribe not covered", func() []byte {
			return s.sign(t, "example.com", []string{"From", "To", "Subject", "Date", "List-Unsubscribe-Post", "Message-ID"})
		}, s.resolver("example.com"), "covering List-Unsubscribe and List-Unsubscribe-Post", false},
		{"signed by an unrelated domain", func() []byte { return s.sign(t, "esp.example.org", covering) }, s.resolver("esp.example.org"), "by the sender's organization", false},
		{"altered after signing", func() []byte {
			return bytes.Replace(s.sign(t, "example.com", covering), []byte("https://u.example.net/o?id=1"), []byte("https://evil.example.net/o?id=1"), 1)
		}, s.resolver("example.com"), "no valid DKIM signature", false},
		{"key not published", func() []byte { return s.sign(t, "example.com", covering) }, s.resolver("other.example.com"), "no valid DKIM signature", false},
		{"key lookup times out", func() []byte { return s.sign(t, "example.com", covering) }, func(context.Context, string) ([]string, error) {
			return nil, &net.DNSError{Err: "timeout", IsTimeout: true, IsTemporary: true}
		}, "key lookup failed", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan, ref, temp := Evaluate(t.Context(), c.raw(), Action{Method: MethodAuto}, Options{Public: pub, Resolver: c.resolver})
			if c.refuse == "" {
				if ref != nil || plan.Method != MethodOneClick || plan.URL != "https://u.example.net/o?id=1" {
					t.Fatalf("plan %+v refusal %v", plan, ref)
				}
				return
			}
			if ref == nil || !strings.Contains(ref.Message, c.refuse) || temp != c.temporary {
				t.Fatalf("plan %+v refusal %v temporary %v, want %q temporary %v", plan, ref, temp, c.refuse, c.temporary)
			}
		})
	}
}

// A message that carries a mailto the sender's organization owns needs no
// DKIM: the resolver is never asked.
func TestEvaluateMailtoMakesNoLookup(t *testing.T) {
	raw := []byte(strings.Replace(rawHeaders, "<https://u.example.net/o?id=1>, ", "", 1) + rawBody)
	plan, ref, _ := Evaluate(t.Context(), raw, Action{Method: MethodAuto}, Options{
		Resolver: func(context.Context, string) ([]string, error) {
			t.Error("DNS lookup for a mailto plan")
			return nil, errors.New("no")
		},
	})
	if ref != nil || plan.Method != MethodMailto || plan.Mailto.Address != "unsub@example.com" {
		t.Fatalf("plan %+v refusal %v", plan, ref)
	}
}

func standIn(tg *unsubscribetest.Target) Poster {
	return Poster{Resolver: tg.Resolve, Dial: tg.Dial, RootCAs: tg.RootCAs()}
}

// Stand-in: a local TLS server plays the sender's one-click endpoint of
// RFC 8058 (https://www.rfc-editor.org/rfc/rfc8058); a real sender's answers
// replace it when recorded.
func TestPost(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		state  model.DeliveryState
		code   model.ErrorCode // of the failure or the returned error
		isErr  bool
	}{
		{"200", 200, model.ProviderAccepted, "", false},
		{"202", 202, model.ProviderAccepted, "", false},
		{"redirect is not followed", http.StatusFound, model.Rejected, model.Validation, false},
		{"permanent redirect is not followed", http.StatusMovedPermanently, model.Rejected, model.Validation, false},
		{"404", 404, model.Rejected, model.Permanent, false},
		{"429", 429, "", model.Transient, true},
		{"503", 503, "", model.Transient, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tg := unsubscribetest.NewTarget(t)
			tg.SetStatus(c.status)
			out, err := standIn(tg).Post(t.Context(), tg.URI("/o?id=1"))
			reqs := tg.Requests()
			if len(reqs) != 1 || reqs[0] != "POST /o?id=1 application/x-www-form-urlencoded cookie= auth=" || tg.Bodies()[0] != OneClickBody {
				t.Errorf("requests = %q bodies %q", reqs, tg.Bodies())
			}
			if c.isErr {
				if !model.IsCode(err, c.code) {
					t.Fatalf("err = %v, want %s", err, c.code)
				}
				return
			}
			if err != nil || out.State != c.state || (c.code != "" && (out.Failure == nil || out.Failure.Code != c.code)) {
				t.Fatalf("outcome %+v err %v", out, err)
			}
		})
	}
}

// The connect-time guard: a name whose answers include a non-public address
// is refused as a whole, with no connection made, for every form of such an
// address; a literal one the same.
func TestPostGuardRefusesNonPublicAnswers(t *testing.T) {
	for _, c := range []struct {
		name  string
		addrs []string
	}{
		{"loopback", []string{"127.0.0.1"}},
		{"private 10.x", []string{"10.1.2.3"}},
		{"private 172.16", []string{"172.16.0.9"}},
		{"private 192.168", []string{"192.168.1.1"}},
		{"metadata", []string{"169.254.169.254"}},
		{"shared 100.64", []string{"100.64.0.1"}},
		{"unspecified", []string{"0.0.0.0"}},
		{"broadcast", []string{"255.255.255.255"}},
		{"multicast", []string{"224.0.0.1"}},
		{"documentation", []string{"192.0.2.7"}},
		{"IPv6 loopback", []string{"::1"}},
		{"IPv6 unspecified", []string{"::"}},
		{"IPv6 ULA", []string{"fd00::1"}},
		{"IPv6 link-local", []string{"fe80::1"}},
		{"IPv4-mapped loopback", []string{"::ffff:127.0.0.1"}},
		{"IPv4-mapped metadata", []string{"::ffff:169.254.169.254"}},
		{"IPv4-compatible loopback", []string{"::127.0.0.1"}},
		{"NAT64 of loopback", []string{"64:ff9b::7f00:1"}},
		{"public then private (rebinding mix)", []string{"93.184.216.34", "10.0.0.1"}},
		{"no answer", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			tg := unsubscribetest.NewTarget(t)
			p := standIn(tg)
			p.Resolver = func(context.Context, string) ([]netip.Addr, error) {
				var out []netip.Addr
				for _, a := range c.addrs {
					out = append(out, netip.MustParseAddr(a))
				}
				return out, nil
			}
			out, err := p.Post(t.Context(), tg.URI("/o"))
			if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
				t.Fatalf("outcome %+v err %v, want rejected validation", out, err)
			}
			if n := len(tg.Requests()); n != 0 {
				t.Errorf("target saw %d requests", n)
			}
		})
	}
	t.Run("literal private address", func(t *testing.T) {
		tg := unsubscribetest.NewTarget(t)
		out, err := standIn(tg).Post(t.Context(), "https://127.0.0.1:1/o")
		if err != nil || out.State != model.Rejected || out.Failure.Code != model.Validation {
			t.Fatalf("outcome %+v err %v", out, err)
		}
	})
}

// The address dialed is the one the guard vetted, whatever the name would
// resolve to a second time.
func TestPostDialsOnlyVettedAddress(t *testing.T) {
	tg := unsubscribetest.NewTarget(t)
	p := standIn(tg)
	var dialed []string
	p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return tg.Dial(ctx, network, addr)
	}
	if _, err := p.Post(t.Context(), tg.URI("/o")); err != nil {
		t.Fatal(err)
	}
	if len(dialed) != 1 || !strings.HasPrefix(dialed[0], "93.184.216.34:") {
		t.Errorf("dialed %q, want the vetted resolver answer", dialed)
	}
}

// The transport uses no proxy, whatever the environment says, and dials
// through the guard.
func TestPostTransport(t *testing.T) {
	tr := Poster{}.transport()
	if tr.Proxy != nil || tr.DialContext == nil || !tr.DisableKeepAlives {
		t.Errorf("transport = %+v", tr)
	}
}

func TestPublicAddr(t *testing.T) {
	for _, a := range []string{"93.184.216.34", "8.8.8.8", "2606:4700:4700::1111"} {
		if !PublicAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s is public", a)
		}
	}
	if PublicAddr(netip.Addr{}) {
		t.Error("the zero address is not public")
	}
}
