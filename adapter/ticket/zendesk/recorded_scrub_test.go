package zendesk_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"hop.top/uma/testing/xrrtest"
)

// What a recording keeps of the sandbox and what it replaces.
//
// The ticket ID is replaced where it is a ticket ID, never as a string: a
// short ID such as 4 also occurs inside timestamps and other IDs, and a
// plain replace corrupts those. The places, per the Support API reference:
//
//   - URL path segment after "tickets" and the ticket_id query parameter;
//   - JSON fields ticket_id, problem_id, followup_ids and ticket_ids, and
//     the id of a "ticket" object or a "tickets" element;
//   - those same two URL shapes inside any JSON string (a ticket's url, a
//     pagination link, an agent link).
//
// Every ID found there becomes scrubTicketID, in the type it had (number or
// string). Replay maps the sandbox's ticket to it the same way, so the
// recorded form of a request does not depend on the ticket.
//
// Each audit's metadata.system (and the same object on a comment) records
// the client, IP address and location of the machine that made the change
// ("client", "ip_address", "location", "latitude", "longitude"); each is
// replaced by a fixed neutral value, and any other key by a marker.
const (
	scrubTicketID = "424242"
	scrubIP       = "192.0.2.1" // RFC 5737 documentation address
	scrubClient   = "uma-recorded-client"
)

// neutralSystem is what a metadata.system field becomes.
var neutralSystem = map[string]any{
	"client":     scrubClient,
	"ip_address": scrubIP,
	"location":   "",
	"latitude":   json.Number("0"),
	"longitude":  json.Number("0"),
}

func neutralFor(key string) any {
	if v, ok := neutralSystem[key]; ok {
		return v
	}
	return "<redacted:system." + key + ">"
}

// zendeskScrub rewrites one sandbox's recorded exchanges. ticket is the
// sandbox ticket ID the test was given; empty or scrubTicketID needs no
// replacement.
type zendeskScrub struct{ ticket string }

func (s zendeskScrub) active() bool { return s.ticket != "" && s.ticket != scrubTicketID }

// normalize rewrites the ticket ID in the recorded URL, never in the
// request sent.
func (s zendeskScrub) normalize(r *xrrtest.Request) {
	if !s.active() {
		return
	}
	segs := strings.Split(r.URL.Path, "/")
	for i := 1; i < len(segs); i++ {
		if segs[i] == s.ticket && segs[i-1] == "tickets" {
			segs[i] = scrubTicketID
		}
	}
	r.URL.Path, r.URL.RawPath = strings.Join(segs, "/"), ""
	q := r.URL.Query()
	if q.Get("ticket_id") == s.ticket {
		q.Set("ticket_id", scrubTicketID)
		r.URL.RawQuery = q.Encode()
	}
}

// body scrubs a JSON request or response body; other bodies pass.
func (s zendeskScrub) body(contentType string, b []byte) []byte {
	if !strings.Contains(strings.ToLower(contentType), "json") {
		return b
	}
	doc, ok := decodeJSON(b)
	if !ok {
		return b
	}
	out, changed := s.walk(doc, "")
	if !changed {
		return b
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return b
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func decodeJSON(b []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return nil, false
	}
	return v, true
}

// isTicketIDField reports whether key, in an object that sits under the
// field self, holds a ticket ID (or a list of them).
func isTicketIDField(self, key string) bool {
	switch key {
	case "ticket_id", "problem_id", "followup_ids", "ticket_ids":
		return true
	case "id":
		return self == "ticket" || self == "tickets"
	}
	return false
}

// walk rewrites v, which sits under the field self (array elements share
// their array's field).
func (s zendeskScrub) walk(v any, self string) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		changed := false
		for k, c := range t {
			var nv any
			var ch bool
			switch {
			case self == "metadata" && k == "system":
				nv, ch = scrubSystem(c)
			case s.active() && isTicketIDField(self, k):
				nv, ch = s.replaceID(c)
			default:
				nv, ch = s.walk(c, k)
			}
			if ch {
				t[k], changed = nv, true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, c := range t {
			if nv, ch := s.walk(c, self); ch {
				t[i], changed = nv, true
			}
		}
		return t, changed
	case string:
		if !s.active() {
			return v, false
		}
		out := s.urlPattern().ReplaceAllString(t, "${1}"+scrubTicketID)
		return out, out != t
	}
	return v, false
}

// urlPattern matches the ticket ID as a path segment after "tickets" or as
// the ticket_id parameter, bounded so a longer number never matches.
func (s zendeskScrub) urlPattern() *regexp.Regexp {
	return regexp.MustCompile(`(/tickets/|[?&]ticket_id=)` + regexp.QuoteMeta(s.ticket) + `\b`)
}

// replaceID replaces the ticket in a scalar or a list of scalars.
func (s zendeskScrub) replaceID(v any) (any, bool) {
	switch t := v.(type) {
	case json.Number:
		if t.String() == s.ticket {
			return json.Number(scrubTicketID), true
		}
	case string:
		if t == s.ticket {
			return scrubTicketID, true
		}
	case []any:
		changed := false
		for i, c := range t {
			if nv, ch := s.replaceID(c); ch {
				t[i], changed = nv, true
			}
		}
		return t, changed
	}
	return v, false
}

// scrubSystem replaces every field of a metadata.system object.
func scrubSystem(v any) (any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return v, false
	}
	changed := false
	for k, c := range m {
		if want := neutralFor(k); !equalJSON(c, want) {
			m[k], changed = want, true
		}
	}
	return m, changed
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// residueLiteral is a value that must not appear in a recording, found
// anywhere in a body. Findings name it, never print it.
type residueLiteral struct{ name, value string }

// residue returns what a scrubbed body still holds that a recording must
// not: it names the field or literal, never the value. An empty result is
// clean. It does not reuse the scrubber's rules: it is deliberately
// stricter, so a gap in the scrubber shows here. For the ticket it flags
// any scalar equal to the ID, or any string (other than a timestamp) that
// holds it as a whole token, wherever it sits; an unrelated ID that happens
// to equal the ticket's is flagged too, and that is by design: pick
// another ticket or review the body.
func residue(contentType string, b []byte, ticket string, literals []residueLiteral) []string {
	var out []string
	for _, l := range literals {
		if l.value != "" && bytes.Contains(b, []byte(l.value)) {
			out = append(out, "literal "+l.name)
		}
	}
	if !strings.Contains(strings.ToLower(contentType), "json") {
		return out
	}
	doc, ok := decodeJSON(b)
	if !ok {
		return out
	}
	checkTicket := ticket != "" && ticket != scrubTicketID
	var walk func(v any, path string, inSystem bool)
	walk = func(v any, path string, inSystem bool) {
		switch t := v.(type) {
		case map[string]any:
			for _, k := range sortedKeys(t) {
				p := path + "." + k
				switch {
				case k == "system" && strings.HasSuffix(path, ".metadata"):
					walk(t[k], p, true)
				case inSystem:
					if !equalJSON(t[k], neutralFor(k)) {
						out = append(out, "recorder detail "+p)
					}
				default:
					if _, ok := neutralSystem[k]; ok && !equalJSON(t[k], neutralFor(k)) {
						out = append(out, "recorder detail "+p)
					}
					walk(t[k], p, false)
				}
			}
		case []any:
			for i, c := range t {
				walk(c, fmt.Sprintf("%s[%d]", path, i), inSystem)
			}
		case json.Number:
			if checkTicket && t.String() == ticket {
				out = append(out, "ticket ID at "+path)
			}
		case string:
			if checkTicket && holdsID(t, ticket) {
				out = append(out, "ticket ID at "+path)
			}
		}
	}
	walk(doc, "", false)
	return out
}

// holdsID reports whether s is the ID or holds it as a whole token: not
// part of a longer number, hex ID or word. Timestamps are not searched:
// their numbers are dates and times.
func holdsID(s, id string) bool {
	if s == id {
		return true
	}
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return false
	}
	return regexp.MustCompile(`(^|[^0-9A-Za-z])` + regexp.QuoteMeta(id) + `($|[^0-9A-Za-z])`).MatchString(s)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
