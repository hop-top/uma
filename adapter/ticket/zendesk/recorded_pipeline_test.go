package zendesk_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/testing/xrrtest"
)

// Synthetic sandbox: values a real recording would hold, chosen to collide
// with what the ticket ID scrub must leave alone. The ticket is 4; digits
// 4 also sit in the timestamps and in the IDs of the audit (14044), the
// comment (4004), the agent (404) and the requester (44).
const (
	synthSub    = "northwind4"
	synthEmail  = "agent4@northwind4.example"
	synthTicket = "4"

	synthIP       = "203.0.113.77"
	synthClient   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4) AppleWebKit/605.1.15 Safari/605.1.15"
	synthLocation = "Montreal, QC, Canada"
	synthLat      = "45.5019"
	synthLon      = "-73.5674"
)

// zendeskDouble answers the Support API calls of the recorded scenario like
// the sandbox would, recorder details included. It is the provider side of
// a synthetic recording, never contacted over a network.
type zendeskDouble struct {
	mu     sync.Mutex
	audits []map[string]any
	auth   []string
}

func (d *zendeskDouble) RoundTrip(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.auth = append(d.auth, r.Header.Get("Authorization"))
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) < 4 || segs[2] != "tickets" || segs[3] != synthTicket {
		return reply(r, http.StatusNotFound, `{"error":"RecordNotFound"}`), nil
	}
	system := func() map[string]any {
		return map[string]any{
			"client": synthClient, "ip_address": synthIP, "location": synthLocation,
			"latitude": json.Number(synthLat), "longitude": json.Number(synthLon),
		}
	}
	switch {
	case r.Method == http.MethodPut && len(segs) == 4:
		var req struct {
			Ticket struct {
				Comment  map[string]any `json:"comment"`
				Metadata map[string]any `json:"metadata"`
			} `json:"ticket"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			return reply(r, http.StatusBadRequest, `{"error":"bad"}`), nil
		}
		au := map[string]any{
			"id": json.Number("14044"), "ticket_id": json.Number(synthTicket), "author_id": json.Number("404"),
			"created_at": "2026-04-04T04:04:44Z", "via": map[string]any{"channel": "api"},
			"metadata": map[string]any{"custom": req.Ticket.Metadata, "system": system()},
			"events": []any{map[string]any{
				"id": json.Number("4004"), "type": "Comment", "author_id": json.Number("404"),
				"body": req.Ticket.Comment["body"], "public": req.Ticket.Comment["public"],
				"metadata": map[string]any{"system": system()},
			}},
		}
		d.audits = append([]map[string]any{au}, d.audits...)
		return replyJSON(r, http.StatusOK, map[string]any{
			"ticket": map[string]any{
				"id": json.Number(synthTicket), "url": "https://" + synthSub + ".zendesk.com/api/v2/tickets/4.json",
				"subject": "uma sandbox", "status": "open", "requester_id": json.Number("44"),
				"created_at": "2026-04-04T04:04:04Z", "updated_at": "2026-04-04T14:04:04Z",
			},
			"audit": au,
		}), nil
	case r.Method == http.MethodGet && len(segs) == 5 && segs[4] == "audits":
		return replyJSON(r, http.StatusOK, map[string]any{
			"audits": d.audits, "meta": map[string]any{"has_more": false, "after_cursor": nil},
			"links": map[string]any{"next": "https://" + synthSub + ".zendesk.com/api/v2/tickets/4/audits?page%5Bafter%5D=x"},
		}), nil
	}
	return reply(r, http.StatusNotFound, `{"error":"RecordNotFound"}`), nil
}

func reply(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:  http.Header{"Content-Type": {"application/json; charset=utf-8"}},
		Body:    io.NopCloser(strings.NewReader(body)),
		Request: r,
	}
}

func replyJSON(r *http.Request, status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return reply(r, status, string(b))
}

// noNetwork fails the test on any call that reaches it: a replay must be
// answered by cassettes alone.
type noNetwork struct{ t *testing.T }

func (n noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("replay reached the network: %s %s", r.Method, r.URL.Path)
	return nil, io.ErrUnexpectedEOF
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func readCassettes(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no cassettes in %s (%v)", dir, err)
	}
	var all strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // reads a file the test just recorded into its own temp directory.
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
	}
	return all.String()
}

// A synthetic recording keeps no recorder detail and no ticket ID where a
// ticket ID belongs, leaves the timestamps and other IDs that contain the
// same digits alone, and replays, for either authentication variant, with
// a different ticket and subdomain and no network.
func TestSyntheticRecordingScrubsAndReplays(t *testing.T) {
	dir := t.TempDir()
	token := "ztok-" + strings.Repeat("a1", 8)
	rec, err := loadSandbox(true, envOf(map[string]string{
		envSubdomain: synthSub, envEmail: synthEmail, envAPIToken: token, envTicketID: synthTicket,
	}))
	if err != nil {
		t.Fatal(err)
	}
	double := &zendeskDouble{}
	runSandbox(t, xrrtest.NewClient(t, dir, recorderOptions(t, rec,
		xrrtest.WithMode(xrrtest.ModeRecord), xrrtest.WithTransport(double))...), rec)
	if len(double.auth) == 0 || !strings.HasPrefix(double.auth[0], "Basic ") {
		t.Fatalf("recording Authorization = %q, want Basic", double.auth)
	}

	cassette := readCassettes(t, dir)
	for _, real := range []string{synthIP, "Montreal", "Mozilla", "Safari", synthLat, synthLon, synthEmail, token, synthSub} {
		if strings.Contains(cassette, real) {
			t.Errorf("cassettes hold the real value %q", real)
		}
	}
	for _, want := range []string{
		scrubIP, scrubClient, // recorder details replaced
		"2026-04-04T04:04:04Z", "2026-04-04T14:04:04Z", "2026-04-04T04:04:44Z", // timestamps intact
		"14044", "4004", // other IDs intact
		`"ticket_id":` + scrubTicketID, "tickets/" + scrubTicketID + ".json", "tickets/" + scrubTicketID + "/audits",
	} {
		if !strings.Contains(cassette, want) {
			t.Errorf("cassettes lack %q", want)
		}
	}
	for _, left := range []string{`"ticket_id":4,`, `"ticket_id":4}`, "tickets/4/", "tickets/4.json"} {
		if strings.Contains(cassette, left) {
			t.Errorf("cassettes keep the ticket ID as %q", left)
		}
	}
	// Other IDs that contain the ticket's digit stay as the sandbox sent them.
	for _, kept := range []string{`"requester_id":44`, `"author_id":404`} {
		if !strings.Contains(cassette, kept) {
			t.Errorf("cassettes lost %s: the ID scrub reached a field that is not a ticket ID", kept)
		}
	}

	// Replay, either variant, other ticket and subdomain, no recording
	// variables.
	for name, env := range map[string]map[string]string{
		"api token": {},
		"oauth":     {envAuth: zendesk.AuthOAuth},
	} {
		t.Run("replay "+name, func(t *testing.T) {
			s, err := loadSandbox(false, envOf(env))
			if err != nil {
				t.Fatal(err)
			}
			if s.ticket == synthTicket || s.sub == synthSub {
				t.Fatalf("replay sandbox %+v reuses the recorded one", s)
			}
			runSandbox(t, xrrtest.NewClient(t, dir, recorderOptions(t, s,
				xrrtest.WithMode(xrrtest.ModeReplay), xrrtest.WithTransport(noNetwork{t}))...), s)
		})
	}
	// A replay with another ticket ID than the stand-in maps to the same
	// cassettes.
	other := sandboxWith(t, map[string]string{envTicketID: "1"})
	runSandbox(t, xrrtest.NewClient(t, dir, recorderOptions(t, other,
		xrrtest.WithMode(xrrtest.ModeReplay), xrrtest.WithTransport(noNetwork{t}))...), other)
}

func sandboxWith(t *testing.T, env map[string]string) sandbox {
	t.Helper()
	s, err := loadSandbox(false, envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The OAuth variant records the same exchanges as API tokens, with a
// bearer token instead of basic auth, and its cassettes replay under the
// API token variant.
func TestSyntheticRecordingOAuth(t *testing.T) {
	dir := t.TempDir()
	access := "zoauth-" + strings.Repeat("b2", 8)
	rec, err := loadSandbox(true, envOf(map[string]string{
		envAuth: zendesk.AuthOAuth, envSubdomain: synthSub, envAccessToken: access, envTicketID: synthTicket,
	}))
	if err != nil {
		t.Fatal(err)
	}
	double := &zendeskDouble{}
	runSandbox(t, xrrtest.NewClient(t, dir, recorderOptions(t, rec,
		xrrtest.WithMode(xrrtest.ModeRecord), xrrtest.WithTransport(double))...), rec)
	if len(double.auth) == 0 || double.auth[0] != "Bearer "+access {
		t.Fatalf("recording Authorization = %q, want the bearer token", double.auth)
	}
	cassette := readCassettes(t, dir)
	for _, real := range []string{access, synthIP, "Montreal", synthSub, "Bearer", "Basic"} {
		if strings.Contains(cassette, real) {
			t.Errorf("cassettes hold %q", real)
		}
	}
	s := sandboxWith(t, nil) // api_token replay
	runSandbox(t, xrrtest.NewClient(t, dir, recorderOptions(t, s,
		xrrtest.WithMode(xrrtest.ModeReplay), xrrtest.WithTransport(noNetwork{t}))...), s)
}

// Recording refuses what it cannot record safely, and replay needs nothing.
func TestLoadSandbox(t *testing.T) {
	full := map[string]string{envSubdomain: "acme", envEmail: "a@acme.example", envAPIToken: "tok", envTicketID: "7"}
	oauth := map[string]string{envAuth: zendesk.AuthOAuth, envSubdomain: "acme", envAccessToken: "acc", envTicketID: "7"}
	with := func(m map[string]string, k, v string) map[string]string {
		out := map[string]string{}
		for a, b := range m {
			out[a] = b
		}
		if v == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
		return out
	}
	for _, c := range []struct {
		name   string
		record bool
		env    map[string]string
		ok     bool
		auth   string
	}{
		{"record api token", true, full, true, zendesk.AuthAPIToken},
		{"record oauth", true, oauth, true, zendesk.AuthOAuth},
		{"record oauth needs no email or api token", true, with(oauth, envEmail, ""), true, zendesk.AuthOAuth},
		{"record oauth without the access token", true, with(oauth, envAccessToken, ""), false, ""},
		{"record api token without the email", true, with(full, envEmail, ""), false, ""},
		{"record api token without the token", true, with(full, envAPIToken, ""), false, ""},
		{"record api token given an access token only", true, with(with(full, envAPIToken, ""), envAccessToken, "acc"), false, ""},
		{"record without the ticket", true, with(full, envTicketID, ""), false, ""},
		{"record the stand-in ticket", true, with(full, envTicketID, scrubTicketID), false, ""},
		{"unknown auth", true, with(full, envAuth, "basic"), false, ""},
		{"replay needs nothing", false, nil, true, zendesk.AuthAPIToken},
		{"replay oauth needs nothing", false, map[string]string{envAuth: zendesk.AuthOAuth}, true, zendesk.AuthOAuth},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := loadSandbox(c.record, envOf(c.env))
			if (err == nil) != c.ok {
				t.Fatalf("loadSandbox error = %v, want ok=%v", err, c.ok)
			}
			if c.ok && s.auth != c.auth {
				t.Fatalf("auth = %q, want %q", s.auth, c.auth)
			}
		})
	}
	if s := sandboxWith(t, nil); s.ticket != scrubTicketID || s.sub != "sandbox" {
		t.Fatalf("replay defaults = %+v", s)
	}
}
