package twilio_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
)

// listed is one message in a list page, in the documented resource shape.
// A queued or accepted message has no date_sent yet.
type listed struct{ sid, to, from, body, created, status string }

func (l listed) json() map[string]any {
	var dateSent any = l.created
	if l.status == "queued" || l.status == "accepted" {
		dateSent = nil
	}
	return map[string]any{
		"account_sid": accountSID, "api_version": "2010-04-01", "body": l.body, "date_created": l.created,
		"date_sent": dateSent, "date_updated": l.created, "direction": "outbound-api", "error_code": nil,
		"error_message": nil, "from": l.from, "messaging_service_sid": nil, "num_media": "0", "num_segments": "1",
		"price": nil, "price_unit": nil, "sid": l.sid, "status": l.status, "to": l.to,
		"uri": "/2010-04-01/Accounts/" + accountSID + "/Messages/" + l.sid + ".json",
	}
}

// listPages answers the list endpoint with pages of messages in the
// documented list shape
// (https://www.twilio.com/docs/messaging/api/message-resource), linking
// them with next_page_uri.
func listPages(pages ...[]listed) http.HandlerFunc {
	resources := make([][]map[string]any, len(pages))
	for n, page := range pages {
		for _, m := range page {
			resources[n] = append(resources[n], m.json())
		}
	}
	return listResources(resources...)
}

// listResources answers the list endpoint with pages of message
// resources, as listPages.
func listResources(pages ...[]map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := 0
		if p := r.URL.Query().Get("Page"); p != "" {
			_, _ = fmt.Sscan(p, &n)
		}
		msgs := []map[string]any{}
		if n < len(pages) {
			msgs = append(msgs, pages[n]...)
		}
		base := "/2010-04-01/Accounts/" + accountSID + "/Messages.json"
		body := map[string]any{
			"messages": msgs, "page": n, "page_size": 50, "start": 0, "end": 0,
			"first_page_uri": base + "?PageSize=50&Page=0", "previous_page_uri": nil, "uri": r.URL.RequestURI(),
			"next_page_uri": nil,
		}
		if n+1 < len(pages) {
			body["next_page_uri"] = fmt.Sprintf("%s?PageSize=50&Page=%d&PageToken=PA%d", base, n+1, n+1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

var (
	since   = time.Date(2026, 10, 11, 0, 30, 0, 0, time.UTC)
	created = "Sun, 11 Oct 2026 00:30:02 +0000"
	before  = "Sat, 10 Oct 2026 23:59:00 +0000"
	sent    = twilio.SentMessage{To: "+15558675310", From: "+15005550006", Body: "hello", Since: since}
)

func TestFindSentQuery(t *testing.T) {
	c, s := newClient(t, listPages(nil))
	if _, _, err := c.FindSent(t.Context(), accountSID, tokenCred, sent); err != nil {
		t.Fatal(err)
	}
	r := s.reqs[0]
	if r.Method != http.MethodGet || r.URL.Path != "/2010-04-01/Accounts/"+accountSID+"/Messages.json" {
		t.Fatalf("request %s %s", r.Method, r.URL.Path)
	}
	q := r.URL.Query()
	// No DateSent filter: a queued message has no date_sent yet, and the
	// filter would hide exactly the send being looked for.
	want := url.Values{"To": {sent.To}, "From": {sent.From}, "PageSize": {"1000"}}
	if q.Encode() != want.Encode() {
		t.Fatalf("query = %v, want %v", q, want)
	}
}

func TestFindSentMatches(t *testing.T) {
	match := listed{"SM00000000000000000000000000000011", sent.To, sent.From, "hello", created, "sent"}
	for _, c := range []struct {
		name    string
		pages   [][]listed
		wantSID string
		wantErr error
	}{
		{"one match", [][]listed{{match}}, match.sid, nil},
		{"match on a later page", [][]listed{{{"SM00000000000000000000000000000012", sent.To, sent.From, "other", created, "sent"}}, {match}}, match.sid, nil},
		{"none", [][]listed{{}}, "", nil},
		{"other body", [][]listed{{{"SM00000000000000000000000000000013", sent.To, sent.From, "hello!", created, "sent"}}}, "", nil},
		{"other recipient", [][]listed{{{"SM00000000000000000000000000000014", "+15550000000", sent.From, "hello", created, "sent"}}}, "", nil},
		{"created before the attempt", [][]listed{{{"SM00000000000000000000000000000015", sent.To, sent.From, "hello", before, "delivered"}}}, "", nil},
		{"two matches", [][]listed{{match, {"SM00000000000000000000000000000016", sent.To, sent.From, "hello", created, "queued"}}}, "", twilio.ErrAmbiguous},
		{"queued, no date_sent", [][]listed{{{"SM00000000000000000000000000000018", sent.To, sent.From, "hello", created, "queued"}}}, "SM00000000000000000000000000000018", nil},
		{"unreadable date", [][]listed{{{"SM00000000000000000000000000000017", sent.To, sent.From, "hello", "yesterday", "sent"}}}, "", twilio.ErrAmbiguous},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl, _ := newClient(t, listPages(c.pages...))
			m, found, err := cl.FindSent(t.Context(), accountSID, tokenCred, sent)
			if !errors.Is(err, c.wantErr) || found != (c.wantSID != "") || m.SID != c.wantSID {
				t.Fatalf("FindSent = %q, %v, %v; want %q, %v", m.SID, found, err, c.wantSID, c.wantErr)
			}
		})
	}
}

// Without a From (a Messaging Service picks the number) the recipient,
// body and service SID decide.
func TestFindSentByService(t *testing.T) {
	svc := sent
	svc.From, svc.MessagingServiceSID = "", "MG00000000000000000000000000000001"
	m := listed{"SM00000000000000000000000000000021", sent.To, "+15005550007", "hello", created, "sent"}
	cl, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		listPages([]listed{m})(w, r)
	})
	_, found, err := cl.FindSent(t.Context(), accountSID, tokenCred, svc)
	if err != nil || found {
		t.Fatalf("FindSent = %v, %v; want no match: the listed message names no service", found, err)
	}
	if s.reqs[0].URL.Query().Has("From") {
		t.Fatalf("query %v filters by From without one", s.reqs[0].URL.Query())
	}
}

// The listing is newest first: once a page holds a message created before
// the attempt, later pages hold only older ones and are not read.
func TestFindSentStopsAtOlderMessages(t *testing.T) {
	newer := listed{"SM00000000000000000000000000000031", sent.To, sent.From, "other", created, "sent"}
	older := listed{"SM00000000000000000000000000000032", sent.To, sent.From, "other", before, "delivered"}
	stale := listed{"SM00000000000000000000000000000033", sent.To, sent.From, "hello", created, "sent"}
	cl, s := newClient(t, listPages([]listed{newer}, []listed{older}, []listed{stale}))
	m, found, err := cl.FindSent(t.Context(), accountSID, tokenCred, sent)
	if err != nil || found {
		t.Fatalf("FindSent = %q, %v, %v; want none before the older page ends the listing", m.SID, found, err)
	}
	if n := len(s.reqs); n != 2 {
		t.Fatalf("pages read = %d, want 2: the listing stops at the first older message", n)
	}
}

func TestFindSentPageLimit(t *testing.T) {
	pages := make([][]listed, 12)
	cl, _ := newClient(t, listPages(pages...))
	if _, _, err := cl.FindSent(t.Context(), accountSID, tokenCred, sent); !errors.Is(err, twilio.ErrAmbiguous) {
		t.Fatalf("listing cut short = %v, want ambiguous: unseen pages may hold the message", err)
	}
}

func TestReconcileOutcome(t *testing.T) {
	m := twilio.Message{SID: messageSID, Status: "delivered"}
	out, err := twilio.ReconcileOutcome(ch, m, true, nil)
	if err != nil || out.State != model.Delivered || len(out.References) != 1 || out.References[0] != model.ProviderMessageRef(ch, messageSID) {
		t.Fatalf("found = %+v, %v", out, err)
	}
	// Twilio keeps a refused create, and a message it could not deliver,
	// as a failed message. Found, it is the attempt's final outcome: the
	// engine reads failed from a reconciliation as "not found", safe to
	// send again.
	for _, status := range []string{"failed", "undelivered", "canceled"} {
		code := 21266
		out, err = twilio.ReconcileOutcome(ch, twilio.Message{SID: messageSID, Status: status, ErrorCode: &code}, true, nil)
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Retry != model.NoRetry ||
			len(out.References) != 1 || out.References[0] != model.ProviderMessageRef(ch, messageSID) {
			t.Fatalf("found %s = %+v, %v; want rejected for good, with the message", status, out, err)
		}
	}
	out, err = twilio.ReconcileOutcome(ch, twilio.Message{}, false, nil)
	if err != nil || out.State != model.Failed || out.Failure == nil {
		t.Fatalf("not found = %+v, %v; want failed: nothing applied", out, err)
	}
	out, err = twilio.ReconcileOutcome(ch, twilio.Message{}, false, twilio.ErrAmbiguous)
	if err != nil || out.State != model.Unknown || len(out.References) != 0 {
		t.Fatalf("ambiguous = %+v, %v; want outcome_unknown", out, err)
	}
	if _, err = twilio.ReconcileOutcome(ch, twilio.Message{}, false, &twilio.APIError{Status: 503}); model.CodeOf(err) != model.Transient {
		t.Fatalf("listing unavailable = %v, want transient: a read is safe to repeat", err)
	}
}
