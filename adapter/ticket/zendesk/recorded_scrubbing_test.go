package zendesk_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"hop.top/uma/testing/xrrtest"
)

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	v, ok := decodeJSON([]byte(s))
	if !ok {
		t.Fatalf("not JSON: %s", s)
	}
	return v
}

// The ticket ID is replaced where it is a ticket ID and nowhere else:
// numbers and strings that merely hold its digits, an exactly equal ID of
// another object and timestamps stay as the sandbox sent them.
func TestTicketIDScrubIsStructural(t *testing.T) {
	in := `{
	  "ticket": {"id": 4, "url": "https://sandbox.zendesk.com/api/v2/tickets/4.json", "requester_id": 44, "problem_id": 4,
	    "followup_ids": [4, 14, 444], "created_at": "2026-04-04T04:04:04Z", "subject": "Ticket 14 and 44"},
	  "audits": [
	    {"id": 4, "ticket_id": 4, "created_at": "2024-04-14T04:04:04Z", "author_id": 404,
	     "events": [{"id": 4, "type": "Comment", "body": "see https://sandbox.zendesk.com/agent/tickets/4 and /tickets/14 and /tickets/44/audits"}],
	     "via": {"channel": "rule", "source": {"from": {"ticket_id": "4", "id": 4}, "rel": "follow_up"}}},
	    {"id": 14044, "ticket_id": 14, "events": []}],
	  "tickets": [{"id": 4}, {"id": 40}],
	  "next_page": "https://sandbox.zendesk.com/api/v2/tickets/4/audits?ticket_id=4&page=4&x=ticket_id=44",
	  "count": 4
	}`
	want := `{
	  "ticket": {"id": 424242, "url": "https://sandbox.zendesk.com/api/v2/tickets/424242.json", "requester_id": 44, "problem_id": 424242,
	    "followup_ids": [424242, 14, 444], "created_at": "2026-04-04T04:04:04Z", "subject": "Ticket 14 and 44"},
	  "audits": [
	    {"id": 4, "ticket_id": 424242, "created_at": "2024-04-14T04:04:04Z", "author_id": 404,
	     "events": [{"id": 4, "type": "Comment", "body": "see https://sandbox.zendesk.com/agent/tickets/424242 and /tickets/14 and /tickets/44/audits"}],
	     "via": {"channel": "rule", "source": {"from": {"ticket_id": "424242", "id": 4}, "rel": "follow_up"}}},
	    {"id": 14044, "ticket_id": 14, "events": []}],
	  "tickets": [{"id": 424242}, {"id": 40}],
	  "next_page": "https://sandbox.zendesk.com/api/v2/tickets/424242/audits?ticket_id=424242&page=4&x=ticket_id=44",
	  "count": 4
	}`
	got := zendeskScrub{ticket: "4"}.body("application/json", []byte(in))
	if !reflect.DeepEqual(mustDecode(t, string(got)), mustDecode(t, want)) {
		t.Fatalf("scrubbed body:\n%s\nwant (as data):\n%s", got, want)
	}
	// Scrubbing what was scrubbed changes nothing.
	if again := (zendeskScrub{ticket: "4"}).body("application/json", got); !bytes.Equal(again, got) {
		t.Fatalf("a second scrub changed the body:\n%s", again)
	}
	// Bodies that are not JSON, and a sandbox with no ticket, pass.
	if out := (zendeskScrub{ticket: "4"}).body("text/plain", []byte("tickets/4")); string(out) != "tickets/4" {
		t.Fatalf("text body = %q", out)
	}
	if out := (zendeskScrub{}).body("application/json", []byte(in)); !bytes.Equal(out, []byte(in)) {
		t.Fatal("scrub without a ticket changed the body")
	}
}

// The recorded request URL carries the stand-in as the ticket's path
// segment, whatever else holds the digits.
func TestTicketIDScrubURL(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://s.zendesk.com/api/v2/tickets/4", "https://s.zendesk.com/api/v2/tickets/424242"},
		{"https://s.zendesk.com/api/v2/tickets/4/audits?sort_order=desc&page%5Bsize%5D=100", "https://s.zendesk.com/api/v2/tickets/424242/audits?sort_order=desc&page%5Bsize%5D=100"},
		{"https://s.zendesk.com/api/v2/tickets/44/audits", "https://s.zendesk.com/api/v2/tickets/44/audits"},
		{"https://s.zendesk.com/api/v2/users/4/tickets", "https://s.zendesk.com/api/v2/users/4/tickets"},
		{"https://s.zendesk.com/api/v2/tickets/14", "https://s.zendesk.com/api/v2/tickets/14"},
		{"https://s.zendesk.com/api/v2/search?ticket_id=4", "https://s.zendesk.com/api/v2/search?ticket_id=424242"},
	} {
		u, err := url.Parse(c.in)
		if err != nil {
			t.Fatal(err)
		}
		r := &xrrtest.Request{Method: http.MethodGet, URL: u}
		zendeskScrub{ticket: "4"}.normalize(r)
		if got := r.URL.String(); got != c.want {
			t.Errorf("%s -> %s, want %s", c.in, got, c.want)
		}
	}
}

// Each audit's metadata.system, and a comment's, loses the recorder's
// client, IP address and location (the field names are those of the
// Support API reference); custom metadata and everything else stays.
func TestAuditSystemScrub(t *testing.T) {
	in := `{"audit": {"id": 1, "metadata": {
	    "custom": {"uma_operation": "00000ad0-0000-7000-8000-0000000000f1"},
	    "system": {"client": "Mozilla/5.0 (X11; Linux x86_64)", "ip_address": "203.0.113.77", "location": "Lyon, 69, France",
	               "latitude": 45.75, "longitude": 4.85, "message_id": "<abc@mail.example>"}},
	  "events": [{"id": 2, "type": "Comment", "metadata": {"system": {"ip_address": "198.51.100.9", "location": "Oslo, Norway"}, "via": {"channel": "api"}}}]}}`
	want := `{"audit": {"id": 1, "metadata": {
	    "custom": {"uma_operation": "00000ad0-0000-7000-8000-0000000000f1"},
	    "system": {"client": "uma-recorded-client", "ip_address": "192.0.2.1", "location": "",
	               "latitude": 0, "longitude": 0, "message_id": "<redacted:system.message_id>"}},
	  "events": [{"id": 2, "type": "Comment", "metadata": {"system": {"ip_address": "192.0.2.1", "location": ""}, "via": {"channel": "api"}}}]}}`
	got := zendeskScrub{}.body("application/json; charset=utf-8", []byte(in))
	if !reflect.DeepEqual(mustDecode(t, string(got)), mustDecode(t, want)) {
		t.Fatalf("scrubbed audit:\n%s\nwant (as data):\n%s", got, want)
	}
	for _, real := range []string{"203.0.113.77", "198.51.100.9", "Lyon", "Oslo", "Mozilla", "45.75", "4.85", "abc@mail"} {
		if bytes.Contains(got, []byte(real)) {
			t.Errorf("scrubbed audit still holds %q", real)
		}
	}
	if r := residue("application/json", got, "", nil); len(r) != 0 {
		t.Fatalf("residue after the scrub: %v", r)
	}
}

// The residue check reports a real value that survives, by name and place
// and never by value, and passes a scrubbed body.
func TestResidue(t *testing.T) {
	const j = "application/json"
	secret := []residueLiteral{{"api token", "ztok-1234567890"}}
	for _, c := range []struct {
		name string
		body string
		want []string // substrings of the findings; none means clean
	}{
		{"scrubbed", `{"ticket":{"id":424242},"audit":{"ticket_id":424242,"metadata":{"system":{"client":"uma-recorded-client","ip_address":"192.0.2.1","location":"","latitude":0,"longitude":0}}}}`, nil},
		{"timestamps holding the digits", `{"created_at":"2026-04-14T14:04:04Z","id":140,"s":"a14b","u":"ab:140:c"}`, nil},
		{"ticket number left", `{"audit":{"ticket_id":14}}`, []string{"ticket ID at .audit.ticket_id"}},
		{"ticket string left", `{"audit":{"ticket_id":"14"}}`, []string{"ticket ID at .audit.ticket_id"}},
		{"ticket in a link", `{"url":"https://x.zendesk.com/api/v2/tickets/14/audits"}`, []string{"ticket ID at .url"}},
		{"ticket in a subject", `{"subject":"Ticket 14 broke"}`, []string{"ticket ID at .subject"}},
		{"another ID equal to the ticket", `{"audit":{"id":14}}`, []string{"ticket ID at .audit.id"}},
		{"ip in system", `{"audit":{"metadata":{"system":{"ip_address":"203.0.113.77"}}}}`, []string{"recorder detail .audit.metadata.system.ip_address"}},
		{"unknown system field", `{"audit":{"metadata":{"system":{"message_id":"<a@b>"}}}}`, []string{"recorder detail .audit.metadata.system.message_id"}},
		{"location outside system", `{"user":{"location":"Lyon"}}`, []string{"recorder detail .user.location"}},
		{"latitude outside system", `{"events":[{"latitude":45.7}]}`, []string{"recorder detail .events[0].latitude"}},
		{"literal secret", `{"note":"key ztok-1234567890 here"}`, []string{"literal api token"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := residue(j, []byte(c.body), "14", secret)
			if len(c.want) == 0 && len(got) != 0 {
				t.Fatalf("residue = %v, want none", got)
			}
			for _, w := range c.want {
				if !strings.Contains(strings.Join(got, "\n"), w) {
					t.Errorf("residue = %v, want %q", got, w)
				}
			}
			for _, f := range got {
				for _, value := range []string{"203.0.113.77", "ztok-1234567890", "Lyon", "<a@b>"} {
					if strings.Contains(f, value) {
						t.Errorf("finding %q prints a real value", f)
					}
				}
			}
		})
	}
	// A sandbox without a ticket, or the stand-in itself, checks no ticket.
	if got := residue(j, []byte(`{"ticket_id":14}`), "", nil); len(got) != 0 {
		t.Fatalf("no ticket: residue = %v", got)
	}
	if got := residue(j, []byte(`{"ticket_id":424242}`), scrubTicketID, nil); len(got) != 0 {
		t.Fatalf("stand-in ticket: residue = %v", got)
	}
}

// spyTB collects what a recording reports instead of failing the test.
type spyTB struct {
	testing.TB
	errs []string
}

func (s *spyTB) Errorf(format string, args ...any) {
	b, _ := json.Marshal(append([]any{format}, args...))
	s.errs = append(s.errs, string(b))
}

// A recording whose response holds a recorder detail the scrub does not
// reach fails: the residue check runs on every body about to be saved.
func TestRecordingFailsOnResidue(t *testing.T) {
	leak := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return reply(r, http.StatusOK, `{"audits":[{"ticket_id":4,"metadata":{"system":{"ip_address":"203.0.113.77"}},"via":{"source":{"from":{"ip_address":"203.0.113.78"}}}}]}`), nil
	})
	s := sandboxWith(t, nil)
	s.ticket = "4"
	spy := &spyTB{TB: t}
	client := xrrtest.NewClient(spy, t.TempDir(), recorderOptions(spy, s,
		xrrtest.WithMode(xrrtest.ModeRecord), xrrtest.WithTransport(leak))...)
	resp, err := client.Get("https://sandbox.zendesk.com/api/v2/tickets/4/audits")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	all := strings.Join(spy.errs, "\n")
	if !strings.Contains(all, "recorder detail .audits[0].via.source.from.ip_address") {
		t.Fatalf("recording errors = %q, want the unscrubbed ip_address named", all)
	}
	if strings.Contains(all, "ticket ID") || strings.Contains(all, "metadata.system") {
		t.Fatalf("recording errors = %q; the ticket and the system fields were scrubbed", all)
	}
	if strings.Contains(all, "203.0.113") {
		t.Fatalf("recording errors print a real value: %q", all)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
