// Package unsubscribe is the provider-neutral part of the unsubscribe
// effect of the mail adapters: it parses the action, decides from a
// message's headers which method, if any, may be used, and performs the RFC
// 8058 one-click POST. The adapters own what is provider specific: reading
// the message and sending the mailto message through their own send path.
//
// The rules, in the order they are applied (every refusal is final and
// happens before any request to the unsubscribe target):
//
//   - the message carries exactly one List-Unsubscribe field (RFC 2369);
//   - one-click (RFC 8058): an https URI on the default port naming a public
//     host with no credentials, the message carrying a List-Unsubscribe-Post
//     field whose value is exactly "List-Unsubscribe=One-Click", and a valid
//     DKIM signature covering both fields (see CheckDKIM);
//   - mailto (RFC 2369, RFC 6068): one address whose organizational domain
//     (public suffix plus one label) equals the sender's, naming no further
//     recipient;
//   - every other URI is never followed.
package unsubscribe

import (
	"bytes"
	"encoding/json"
	"net/mail"
	"net/netip"
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/publicsuffix"

	"hop.top/uma/model"
)

// Action methods. An empty method is MethodAuto.
const (
	MethodAuto     = "auto"
	MethodOneClick = "one_click"
	MethodMailto   = "mailto"
)

// Limits on what a header may make the adapter do.
const (
	maxURILength     = 2048
	maxSubjectLength = 200
	maxBodyLength    = 1000
	// OneClickBody is the form body of the one-click POST (RFC 8058,
	// section 3.1).
	OneClickBody = "List-Unsubscribe=One-Click"
	// DefaultSubject and DefaultBody fill a mailto message whose URI names
	// none.
	DefaultSubject = "unsubscribe"
	DefaultBody    = "unsubscribe"
)

// Action is the data of the unsubscribe effect.
type Action struct {
	// Method is MethodAuto (one-click when eligible, else mailto),
	// MethodOneClick or MethodMailto.
	Method string
}

// CheckAction validates the action's data without I/O. The data is optional;
// when set it is a JSON object whose "method" is one of the Method
// constants. Fields it does not know are ignored.
func CheckAction(action model.Extension) (Action, *model.ValidationIssue) {
	refuse := func(field, msg string) (Action, *model.ValidationIssue) {
		return Action{}, &model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
	}
	a := Action{Method: MethodAuto}
	data := bytes.TrimSpace(action.Data)
	if len(data) == 0 {
		return a, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return refuse("data", "action data must be a JSON object")
	}
	raw, set := fields["method"]
	if !set || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return a, nil
	}
	var m string
	if err := json.Unmarshal(raw, &m); err != nil {
		return refuse("data.method", "method must be a string")
	}
	switch m {
	case "", MethodAuto:
	case MethodOneClick, MethodMailto:
		a.Method = m
	default:
		return refuse("data.method", "method must be auto, one_click or mailto")
	}
	return a, nil
}

// Headers is what Decide needs of a message: its raw header fields and the
// address that sent it.
type Headers struct {
	// Unsubscribe and Post are the raw values of every List-Unsubscribe and
	// List-Unsubscribe-Post field of the message.
	Unsubscribe []string
	Post        []string
	// From is the message's From addresses.
	From []string
}

// Mailto is a validated mailto unsubscribe target.
type Mailto struct {
	Address string
	Subject string
	Body    string
}

// Plan is the method Decide chose.
type Plan struct {
	Method string
	// URL is the https URI of a one-click plan.
	URL string
	// Mailto is the target of a mailto plan.
	Mailto Mailto
}

// Refusal is a final refusal: Message is safe to show.
type Refusal struct {
	Message string
	// absent marks a refusal that only says the method has no URI to use,
	// which says less than another method's refusal of a URI it found.
	absent bool
}

func (r *Refusal) Error() string { return r.Message }

func refuse(msg string) *Refusal { return &Refusal{Message: msg} }

// Decide picks the method for a message from its headers. It does no I/O;
// the one-click plan still needs CheckDKIM before it is carried out.
func Decide(h Headers, a Action, o Options) (Plan, *Refusal) {
	switch len(h.Unsubscribe) {
	case 0:
		return Plan{}, refuse("the message has no List-Unsubscribe header")
	case 1:
	default:
		return Plan{}, refuse("the message has more than one List-Unsubscribe header")
	}
	uris := parseURIs(h.Unsubscribe[0])
	if len(uris) == 0 {
		return Plan{}, refuse("the List-Unsubscribe header names no URI")
	}
	var first *Refusal
	note := func(r *Refusal) {
		if first == nil || (first.absent && !r.absent) {
			first = r
		}
	}
	if a.Method != MethodMailto {
		p, r := oneClick(h, uris, o)
		if r == nil {
			return p, nil
		}
		note(r)
	}
	if a.Method != MethodOneClick {
		p, r := mailto(h, uris)
		if r == nil {
			return p, nil
		}
		note(r)
	}
	return Plan{}, first
}

// oneClick finds the https URI of a message eligible for RFC 8058.
func oneClick(h Headers, uris []string, o Options) (Plan, *Refusal) {
	var https, http bool
	var target string
	for _, u := range uris {
		switch scheme(u) {
		case "https":
			if !https {
				target = u
			}
			https = true
		case "http":
			http = true
		}
	}
	if !https {
		if http {
			return Plan{}, &Refusal{Message: "the List-Unsubscribe header names an http URI, not https; it is never followed", absent: true}
		}
		return Plan{}, &Refusal{Message: "the List-Unsubscribe header names no https URI; other schemes are never followed", absent: true}
	}
	switch len(h.Post) {
	case 0:
		return Plan{}, refuse("the message has no List-Unsubscribe-Post header, so no one-click request may be made")
	case 1:
	default:
		return Plan{}, refuse("the message has more than one List-Unsubscribe-Post header")
	}
	if strings.TrimSpace(h.Post[0]) != OneClickBody {
		return Plan{}, refuse("List-Unsubscribe-Post is not List-Unsubscribe=One-Click")
	}
	if r := checkHTTPS(target, o); r != nil {
		return Plan{}, r
	}
	return Plan{Method: MethodOneClick, URL: target}, nil
}

// checkHTTPS applies the target rules to an https URI.
func checkHTTPS(raw string, o Options) *Refusal {
	public := o.Public
	if public == nil {
		public = PublicHost
	}
	if len(raw) > maxURILength {
		return refuse("the https URI is too long")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "":
		return refuse("the https URI is not valid")
	case u.User != nil:
		return refuse("the https URI carries credentials")
	case !o.AnyPort && u.Port() != "" && u.Port() != "443":
		return refuse("the https URI is not on the default port")
	case !public(u.Hostname()):
		return refuse("the https URI does not name a public host")
	}
	return nil
}

// mailto finds the mailto URI of a message that may be answered by mail.
func mailto(h Headers, uris []string) (Plan, *Refusal) {
	var raw string
	for _, u := range uris {
		if scheme(u) == "mailto" {
			raw = u
			break
		}
	}
	if raw == "" {
		return Plan{}, &Refusal{Message: "the List-Unsubscribe header names no mailto URI; other schemes are never followed", absent: true}
	}
	m, r := parseMailto(raw)
	if r != nil {
		return Plan{}, r
	}
	from, r := senderDomain(h.From)
	if r != nil {
		return Plan{}, r
	}
	target, r := OrgDomain(m.Address[strings.LastIndexByte(m.Address, '@')+1:])
	if r != nil || target != from {
		return Plan{}, refuse("the mailto target is not in the sender's organization")
	}
	return Plan{Method: MethodMailto, Mailto: m}, nil
}

// senderDomain is the organizational domain of the message's one sender.
func senderDomain(from []string) (string, *Refusal) {
	if len(from) != 1 {
		return "", refuse("the message has no single sender to compare the mailto target with")
	}
	a, err := mail.ParseAddress(from[0])
	if err != nil {
		return "", refuse("the message's sender address is not valid")
	}
	d, r := OrgDomain(a.Address[strings.LastIndexByte(a.Address, '@')+1:])
	if r != nil {
		return "", refuse("the message's sender is not in a registrable domain")
	}
	return d, nil
}

// OrgDomain is the organizational domain of host: its public suffix plus
// one label (golang.org/x/net/publicsuffix, which includes the privately
// operated suffixes, so tenants of a hosting domain are not one
// organization). It is lowercased. A host that is a public suffix itself, an
// address or empty has none.
func OrgDomain(host string) (string, *Refusal) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if _, err := netip.ParseAddr(host); err == nil || host == "" {
		return "", refuse("not a registrable domain")
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return "", refuse("not a registrable domain")
	}
	return d, nil
}

// parseMailto validates a mailto URI (RFC 6068): one address, and only the
// subject and body header fields, which are never allowed to add a
// recipient or to break a header line.
func parseMailto(raw string) (Mailto, *Refusal) {
	if len(raw) > maxURILength {
		return Mailto{}, refuse("the mailto URI is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "mailto") || u.Opaque == "" {
		return Mailto{}, refuse("the mailto URI is not valid")
	}
	addr, err := url.PathUnescape(u.Opaque)
	if err != nil || strings.ContainsAny(addr, ", ;") {
		return Mailto{}, refuse("the mailto URI must name exactly one address")
	}
	addr, err = model.NormalizeEmail(addr)
	if err != nil {
		return Mailto{}, refuse("the mailto address is not valid")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return Mailto{}, refuse("the mailto URI is not valid")
	}
	m := Mailto{Address: addr, Subject: DefaultSubject, Body: DefaultBody}
	for k, vs := range q {
		switch strings.ToLower(k) {
		case "to", "cc", "bcc":
			return Mailto{}, refuse("the mailto URI names more recipients")
		case "subject":
			if s := vs[0]; s != "" {
				m.Subject = s
			}
		case "body":
			if b := vs[0]; b != "" {
				m.Body = b
			}
		}
	}
	if len(m.Subject) > maxSubjectLength || strings.ContainsAny(m.Subject, "\r\n") || hasControl(m.Subject) {
		return Mailto{}, refuse("the mailto subject is not acceptable")
	}
	if len(m.Body) > maxBodyLength || hasControl(strings.NewReplacer("\r", "", "\n", "").Replace(m.Body)) {
		return Mailto{}, refuse("the mailto body is not acceptable")
	}
	return m, nil
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\t' })
}

// parseURIs returns the angle-bracketed URIs of a List-Unsubscribe value in
// order (RFC 2369, section 2), skipping comments in parentheses and the
// separators.
func parseURIs(v string) []string {
	var out []string
	depth := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case c == '<' && depth == 0:
			end := strings.IndexByte(v[i+1:], '>')
			if end < 0 {
				return out
			}
			if u := strings.TrimSpace(v[i+1 : i+1+end]); u != "" {
				out = append(out, u)
			}
			i += end + 1
		}
	}
	return out
}

// scheme is the lowercased URI scheme of u, empty when it has none.
func scheme(u string) string {
	i := strings.IndexByte(u, ':')
	if i <= 0 {
		return ""
	}
	return strings.ToLower(u[:i])
}

// reserved lists address ranges that are not on the public internet and
// that the unicast, non-private, non-loopback tests do not exclude: the
// "this network", shared (CGNAT), IETF protocol, documentation, benchmarking
// and reserved IPv4 blocks (which include the broadcast address), and the
// IPv4-compatible, discard, NAT64, Teredo, documentation and 6to4 IPv6
// blocks. IPv4-mapped IPv6 addresses are judged as the IPv4 address they
// carry.
var reserved = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"::/96", "100::/64", "64:ff9b::/96", "64:ff9b:1::/48", "2001::/32", "2001:db8::/32", "2002::/16",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// PublicAddr reports whether ip is globally routable: not loopback, private,
// link-local (the cloud metadata address 169.254.169.254 included),
// unspecified, multicast, shared, documentation or reserved, whatever form
// it is written in.
func PublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// PublicHost reports whether host, as written in a URL, names a public
// place: a globally routable address, or a name with a dot and an
// alphabetic last label (so no single-label, numeric or hexadecimal form a
// resolver might read as an address) outside the local and internal
// suffixes. It judges the text of the URL only, as the refusal made before
// any network call; Poster re-checks every address a name resolves to at
// connect time.
func PublicHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || strings.Contains(host, "%") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return PublicAddr(ip)
	}
	dot := strings.LastIndexByte(host, '.')
	if dot < 0 || !strings.ContainsAny(host[dot+1:], "abcdefghijklmnopqrstuvwxyz") {
		return false
	}
	for _, s := range []string{".localhost", ".local", ".internal", ".localdomain", ".home.arpa"} {
		if strings.HasSuffix(host, s) {
			return false
		}
	}
	return true
}
