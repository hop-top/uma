package unsubscribe

import (
	"strings"
	"testing"

	"hop.top/uma/model"
)

func hdr(unsub, post, from string) Headers {
	h := Headers{From: []string{from}}
	if unsub != "" {
		h.Unsubscribe = []string{unsub}
	}
	if post != "" {
		h.Post = []string{post}
	}
	return h
}

func TestCheckAction(t *testing.T) {
	for _, c := range []struct {
		name, data, method, field string
	}{
		{"empty", ``, MethodAuto, ""},
		{"empty object", `{}`, MethodAuto, ""},
		{"auto", `{"method":"auto"}`, MethodAuto, ""},
		{"one click", `{"method":"one_click","x":1}`, MethodOneClick, ""},
		{"mailto", `{"method":"mailto"}`, MethodMailto, ""},
		{"null", `{"method":null}`, MethodAuto, ""},
		{"unknown", `{"method":"get"}`, "", "data.method"},
		{"not a string", `{"method":1}`, "", "data.method"},
		{"not an object", `[1]`, "", "data"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, issue := CheckAction(model.Extension{Data: []byte(c.data)})
			switch {
			case c.field == "" && issue != nil:
				t.Fatalf("issue %+v", issue)
			case c.field != "" && (issue == nil || issue.Field != c.field || issue.Code != string(model.Validation)):
				t.Fatalf("issue %+v, want field %s", issue, c.field)
			case c.field == "" && a.Method != c.method:
				t.Fatalf("method %q, want %q", a.Method, c.method)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	const post = "List-Unsubscribe=One-Click"
	const from = "News <news@mail.example.com>"
	for _, c := range []struct {
		name   string
		h      Headers
		method string
		want   string // plan method, or "" for a refusal
		refuse string // substring of the refusal
	}{
		{"one click", hdr("<https://u.example.net/o?id=1>", post, from), MethodAuto, MethodOneClick, ""},
		{"one click preferred over mailto", hdr("<mailto:u@example.com>, <https://u.example.net/o>", post, from), MethodAuto, MethodOneClick, ""},
		{"comment skipped", hdr("(unsubscribe here) <https://u.example.net/o>", post, from), MethodAuto, MethodOneClick, ""},
		{"mailto, same domain", hdr("<mailto:unsub@mail.example.com?subject=bye>", "", from), MethodAuto, MethodMailto, ""},
		{"mailto, sibling subdomain of the organization", hdr("<mailto:unsub@lists.example.com>", "", from), MethodAuto, MethodMailto, ""},
		{"mailto when one-click is not eligible", hdr("<https://u.example.net/o>, <mailto:u@example.com>", "", from), MethodAuto, MethodMailto, ""},
		{"mailto forced", hdr("<mailto:u@example.com>, <https://u.example.net/o>", post, from), MethodMailto, MethodMailto, ""},
		{"no header", Headers{From: []string{from}}, MethodAuto, "", "no List-Unsubscribe header"},
		{"two headers", Headers{From: []string{from}, Unsubscribe: []string{"<https://a.example.net/x>", "<https://b.example.net/x>"}, Post: []string{post}}, MethodAuto, "", "more than one List-Unsubscribe"},
		{"no URI", hdr("unsubscribe me", post, from), MethodAuto, "", "no URI"},
		{"http, not https", hdr("<http://u.example.net/o>", post, from), MethodAuto, "", "http URI, not https"},
		{"other schemes only", hdr("<ftp://u.example.net/o>, <javascript:alert(1)>, <tel:+1>", post, from), MethodAuto, "", "other schemes are never followed"},
		{"missing Post", hdr("<https://u.example.net/o>", "", from), MethodAuto, "", "no List-Unsubscribe-Post"},
		{"missing Post, one-click forced", hdr("<https://u.example.net/o>, <mailto:u@example.com>", "", from), MethodOneClick, "", "no List-Unsubscribe-Post"},
		{"Post with another value", hdr("<https://u.example.net/o>", "List-Unsubscribe=Click", from), MethodAuto, "", "not List-Unsubscribe=One-Click"},
		{"two Post", Headers{From: []string{from}, Unsubscribe: []string{"<https://u.example.net/o>"}, Post: []string{post, post}}, MethodAuto, "", "more than one List-Unsubscribe-Post"},
		{"https with credentials", hdr("<https://a:b@u.example.net/o>", post, from), MethodAuto, "", "credentials"},
		{"https on another port", hdr("<https://u.example.net:8443/o>", post, from), MethodAuto, "", "default port"},
		{"https to a private address", hdr("<https://10.0.0.5/o>", post, from), MethodAuto, "", "public host"},
		{"https to localhost", hdr("<https://localhost/o>", post, from), MethodAuto, "", "public host"},
		{"https to a single label", hdr("<https://intranet/o>", post, from), MethodAuto, "", "public host"},
		{"mailto, other organization", hdr("<mailto:unsub@evil.example.org>", "", from), MethodAuto, "", "not in the sender's organization"},
		{"mailto, suffix trick", hdr("<mailto:unsub@notexample.com>", "", from), MethodAuto, "", "not in the sender's organization"},
		{"mailto, private suffix tenants are not one organization", hdr("<mailto:u@a.github.io>", "", "x@b.github.io"), MethodAuto, "", "not in the sender's organization"},
		{"mailto, no sender", Headers{Unsubscribe: []string{"<mailto:u@example.com>"}}, MethodAuto, "", "no single sender"},
		{"mailto, two recipients", hdr("<mailto:a@example.com,b@example.org>", "", from), MethodAuto, "", "exactly one address"},
		{"mailto, cc", hdr("<mailto:a@example.com?cc=b@example.org>", "", from), MethodAuto, "", "more recipients"},
		{"mailto, to", hdr("<mailto:a@example.com?to=b@example.org>", "", from), MethodAuto, "", "more recipients"},
		{"mailto, header injection", hdr("<mailto:a@example.com?subject=x%0d%0aBcc:%20b@example.org>", "", from), MethodAuto, "", "subject is not acceptable"},
		{"mailto, no address", hdr("<mailto:?subject=x>", "", from), MethodAuto, "", "not valid"},
		{"https forced, only mailto", hdr("<mailto:u@example.com>", post, from), MethodOneClick, "", "names no https URI"},
		{"mailto forced, only https", hdr("<https://u.example.net/o>", post, from), MethodMailto, "", "no mailto URI"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, _ := CheckAction(model.Extension{Data: []byte(`{"method":"` + c.method + `"}`)})
			plan, ref := Decide(c.h, a, Options{})
			if c.want == "" {
				if ref == nil || !strings.Contains(ref.Message, c.refuse) {
					t.Fatalf("plan %+v refusal %v, want refusal containing %q", plan, ref, c.refuse)
				}
				return
			}
			if ref != nil || plan.Method != c.want {
				t.Fatalf("plan %+v refusal %v, want %s", plan, ref, c.want)
			}
		})
	}
}

func TestMailtoFields(t *testing.T) {
	p, ref := Decide(hdr("<mailto:Unsub@Mail.Example.COM?subject=Remove%20me&body=please%0Aremove>", "", "a@example.com"), Action{Method: MethodMailto}, Options{})
	if ref != nil {
		t.Fatal(ref)
	}
	if p.Mailto.Address != "Unsub@mail.example.com" || p.Mailto.Subject != "Remove me" || p.Mailto.Body != "please\nremove" {
		t.Errorf("mailto = %+v", p.Mailto)
	}
	p, _ = Decide(hdr("<mailto:u@example.com>", "", "a@example.com"), Action{Method: MethodMailto}, Options{})
	if p.Mailto.Subject != DefaultSubject || p.Mailto.Body != DefaultBody {
		t.Errorf("defaults = %+v", p.Mailto)
	}
}

func TestOrgDomain(t *testing.T) {
	for in, want := range map[string]string{
		"mail.example.com": "example.com", "EXAMPLE.com.": "example.com", "a.b.example.co.uk": "example.co.uk", "x.github.io": "x.github.io",
	} {
		if got, ref := OrgDomain(in); ref != nil || got != want {
			t.Errorf("OrgDomain(%q) = %q, %v; want %q", in, got, ref, want)
		}
	}
	for _, in := range []string{"", "com", "co.uk", "10.0.0.1", "github.io"} {
		if got, ref := OrgDomain(in); ref == nil {
			t.Errorf("OrgDomain(%q) = %q, want none", in, got)
		}
	}
}
