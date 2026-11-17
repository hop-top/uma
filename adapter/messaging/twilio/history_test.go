package twilio_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
)

const (
	channelNumber = "+15005550006"
	peerNumber    = "+15558675310"
)

// windowStart is a history window's start, mid-second as a caller's clock
// may give it.
var windowStart = time.Date(2026, 10, 11, 9, 30, 0, 400_000_000, time.UTC)

// received is an inbound message resource, in the documented resource
// shape (https://www.twilio.com/docs/messaging/api/message-resource):
// direction inbound, status received, date_sent when Twilio received it.
func received(n int, from string, at time.Time) map[string]any {
	sid := fmt.Sprintf("SM%032x", n)
	return map[string]any{
		"account_sid": accountSID, "api_version": "2010-04-01", "body": fmt.Sprintf("message %d", n),
		"date_created": at.Format(time.RFC1123Z), "date_sent": at.Format(time.RFC1123Z),
		"date_updated": at.Format(time.RFC1123Z), "direction": "inbound", "error_code": nil, "error_message": nil,
		"from": from, "messaging_service_sid": nil, "num_media": "0", "num_segments": "1", "price": nil,
		"price_unit": "USD", "sid": sid, "status": "received", "to": channelNumber,
		"uri": "/2010-04-01/Accounts/" + accountSID + "/Messages/" + sid + ".json",
	}
}

func sids(ms []twilio.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.SID
	}
	return out
}

func sidOf(n int) string { return fmt.Sprintf("SM%032x", n) }

// readWindow reads the whole window in pages of limit, following each
// page's cursor, and returns the messages in order.
func readWindow(t *testing.T, c *twilio.Client, q twilio.InboundQuery) []twilio.Message {
	t.Helper()
	var out []twilio.Message
	for range 50 {
		p, err := c.ListInbound(t.Context(), accountSID, tokenCred, q)
		if err != nil {
			t.Fatalf("ListInbound(after %+v): %v", q.After, err)
		}
		if len(p.Messages) > q.Limit {
			t.Fatalf("page of limit %d holds %d messages", q.Limit, len(p.Messages))
		}
		out = append(out, p.Messages...)
		if p.Next == "" {
			return out
		}
		if q.After, err = twilio.ParseCursor(p.Next); err != nil {
			t.Fatalf("issued cursor %q refused: %v", p.Next, err)
		}
	}
	t.Fatal("window never ends")
	return nil
}

// The listing filters by the channel's number, the sender when one is
// given, and the UTC day holding the window's start, in Twilio's
// documented date-only granularity, at the largest page size.
func TestListInboundQuery(t *testing.T) {
	c, s := newClient(t, listResources(nil))
	q := twilio.InboundQuery{To: channelNumber, From: peerNumber, Since: windowStart, Limit: 10}
	if _, err := c.ListInbound(t.Context(), accountSID, tokenCred, q); err != nil {
		t.Fatal(err)
	}
	r := s.reqs[0]
	if r.Method != http.MethodGet || r.URL.Path != "/2010-04-01/Accounts/"+accountSID+"/Messages.json" {
		t.Fatalf("request %s %s", r.Method, r.URL.Path)
	}
	want := url.Values{"To": {channelNumber}, "From": {peerNumber}, "DateSent>": {"2026-10-11T00:00:00Z"}, "PageSize": {"1000"}}
	if r.URL.Query().Encode() != want.Encode() {
		t.Fatalf("query = %v, want %v", r.URL.Query(), want)
	}
	q.From = ""
	if _, err := c.ListInbound(t.Context(), accountSID, tokenCred, q); err != nil {
		t.Fatal(err)
	}
	if s.reqs[1].URL.Query().Has("From") {
		t.Fatalf("channel window query %v filters by sender", s.reqs[1].URL.Query())
	}
	// A cursor moves the lower bound to its day.
	after := twilio.Position{Sent: windowStart.Add(48 * time.Hour).Truncate(time.Second), SID: sidOf(1)}
	q.After = after
	if _, err := c.ListInbound(t.Context(), accountSID, tokenCred, q); err != nil {
		t.Fatal(err)
	}
	if got := s.reqs[2].URL.Query().Get("DateSent>"); got != "2026-10-13T00:00:00Z" {
		t.Fatalf("resumed lower bound = %q, want the cursor's day", got)
	}
}

// Twilio lists newest first, across pages. The reader returns the window
// oldest first, in bounded pages, each message once, whatever the page
// limit; the day filter's earlier messages stay out.
func TestListInboundOldestFirstAcrossPages(t *testing.T) {
	at := func(d time.Duration) time.Time { return windowStart.Truncate(time.Second).Add(d) }
	pages := [][]map[string]any{
		{received(6, peerNumber, at(5*time.Hour)), received(5, peerNumber, at(4*time.Hour))},
		{received(4, peerNumber, at(3*time.Hour)), received(3, peerNumber, at(2*time.Hour))},
		// Messages 1 and 2 share a second, in which Twilio's order is
		// unspecified: they order by SID.
		{received(1, peerNumber, at(time.Hour)), received(2, peerNumber, at(time.Hour))},
		{received(0, peerNumber, at(-time.Minute))},
	}
	want := []string{sidOf(1), sidOf(2), sidOf(3), sidOf(4), sidOf(5), sidOf(6)}
	for _, limit := range []int{1, 2, 4, 10} {
		c, _ := newClient(t, listResources(pages...))
		got := readWindow(t, c, twilio.InboundQuery{To: channelNumber, Since: windowStart, Limit: limit})
		if !slices.Equal(sids(got), want) {
			t.Fatalf("limit %d: window = %q, want %q", limit, sids(got), want)
		}
	}
}

// A message that arrives during a read shifts Twilio's later pages, so
// one message may be listed on two pages: the window holds it once.
func TestListInboundShiftedPages(t *testing.T) {
	at := func(d time.Duration) time.Time { return windowStart.Truncate(time.Second).Add(d) }
	c, _ := newClient(t, listResources(
		[]map[string]any{received(3, peerNumber, at(3*time.Hour)), received(2, peerNumber, at(2*time.Hour))},
		[]map[string]any{received(2, peerNumber, at(2*time.Hour)), received(1, peerNumber, at(time.Hour))},
	))
	p, err := c.ListInbound(t.Context(), accountSID, tokenCred, twilio.InboundQuery{To: channelNumber, Since: windowStart, Limit: 10})
	if err != nil || !slices.Equal(sids(p.Messages), []string{sidOf(1), sidOf(2), sidOf(3)}) {
		t.Fatalf("window = %q, %v; want each message once", sids(p.Messages), err)
	}
}

// A message in the second holding the window's start is in, since Twilio
// dates have one-second resolution; one in the second before is out.
func TestListInboundWindowStart(t *testing.T) {
	start := windowStart.Truncate(time.Second)
	c, _ := newClient(t, listResources([]map[string]any{
		received(2, peerNumber, start), received(1, peerNumber, start.Add(-time.Second)),
	}))
	p, err := c.ListInbound(t.Context(), accountSID, tokenCred, twilio.InboundQuery{To: channelNumber, Since: windowStart, Limit: 10})
	if err != nil || !slices.Equal(sids(p.Messages), []string{sidOf(2)}) || p.Next != "" || p.Gap {
		t.Fatalf("window = %q next %q gap %v, %v; want only the message at the start", sids(p.Messages), p.Next, p.Gap, err)
	}
}

// Until ends the window exclusively: a message received at Until is out,
// one just before it is in, across pages, and the messages after it do not
// count as older ones that end the read.
func TestListInboundUntil(t *testing.T) {
	start := windowStart.Truncate(time.Second)
	until := start.Add(3 * time.Second)
	c, _ := newClient(t, listResources([]map[string]any{
		received(5, peerNumber, start.Add(4*time.Second)), received(4, peerNumber, until),
		received(3, peerNumber, until.Add(-time.Second)), received(2, peerNumber, start.Add(time.Second)),
		received(1, peerNumber, start),
	}))
	got := readWindow(t, c, twilio.InboundQuery{To: channelNumber, Since: windowStart, Until: until, Limit: 2})
	if !slices.Equal(sids(got), []string{sidOf(1), sidOf(2), sidOf(3)}) {
		t.Fatalf("window = %q, want the messages before Until, oldest first", sids(got))
	}
}

// Only inbound messages to the channel's number, from the sender asked
// for, are history: outbound messages, the adapter's own sends among them,
// never come back as received, and a listing that ignored a filter adds
// nothing. A message between two numbers of one account is listed twice,
// as the sender's outbound message and the recipient's inbound one: only
// the inbound one is history.
func TestListInboundKeepsInboundOnly(t *testing.T) {
	at := windowStart.Add(time.Hour)
	own := received(1, channelNumber, at)
	own["direction"], own["to"], own["status"] = "outbound-api", peerNumber, "delivered"
	reply := received(2, peerNumber, at)
	reply["direction"], reply["status"] = "outbound-api", "delivered"
	other := received(3, "+15550000001", at)
	elsewhere := received(4, peerNumber, at)
	elsewhere["to"] = "+15550000002"
	in := received(5, peerNumber, at)
	c, _ := newClient(t, listResources([]map[string]any{own, reply, other, elsewhere, in}))
	p, err := c.ListInbound(t.Context(), accountSID, tokenCred, twilio.InboundQuery{To: channelNumber, From: peerNumber, Since: windowStart, Limit: 10})
	if err != nil || !slices.Equal(sids(p.Messages), []string{sidOf(5)}) {
		t.Fatalf("window = %q, %v; want only the inbound message from the sender", sids(p.Messages), err)
	}
}

// Newest first, a page holding a message before the window's start ends
// the listing: later pages are older still.
func TestListInboundStopsAtOlderMessages(t *testing.T) {
	c, s := newClient(t, listResources(
		[]map[string]any{received(2, peerNumber, windowStart.Add(time.Hour)), received(1, peerNumber, windowStart.Add(-time.Hour))},
		[]map[string]any{received(0, peerNumber, windowStart.Add(-2*time.Hour))},
	))
	p, err := c.ListInbound(t.Context(), accountSID, tokenCred, twilio.InboundQuery{To: channelNumber, Since: windowStart, Limit: 10})
	if err != nil || !slices.Equal(sids(p.Messages), []string{sidOf(2)}) {
		t.Fatalf("window = %q, %v", sids(p.Messages), err)
	}
	if n := s.count(); n != 1 {
		t.Fatalf("pages read = %d, want 1", n)
	}
}

// A window larger than the pages the reader takes in one call returns its
// newest part, reported as a gap: the older messages are out of reach.
func TestListInboundPageLimitIsGap(t *testing.T) {
	pages := make([][]map[string]any, 12)
	for n := range pages {
		pages[n] = []map[string]any{received(100-n, peerNumber, windowStart.Add(time.Duration(100-n)*time.Minute))}
	}
	c, s := newClient(t, listResources(pages...))
	p, err := c.ListInbound(t.Context(), accountSID, tokenCred, twilio.InboundQuery{To: channelNumber, Since: windowStart, Limit: 1})
	if err != nil || !p.Gap || !slices.Equal(sids(p.Messages), []string{sidOf(91)}) || p.Next == "" {
		t.Fatalf("window = %q next %q gap %v, %v; want the oldest message read, a gap and more", sids(p.Messages), p.Next, p.Gap, err)
	}
	if n := s.count(); n != 10 {
		t.Fatalf("pages read = %d, want 10", n)
	}
}

// An inbound message without a readable date cannot be placed in the
// window: it is left out and reported as a gap.
func TestListInboundUnreadableDateIsGap(t *testing.T) {
	bad := received(1, peerNumber, windowStart)
	bad["date_sent"] = nil
	c, _ := newClient(t, listResources([]map[string]any{received(2, peerNumber, windowStart.Add(time.Hour)), bad}))
	p, err := c.ListInbound(t.Context(), accountSID, tokenCred, twilio.InboundQuery{To: channelNumber, Since: windowStart, Limit: 10})
	if err != nil || !p.Gap || !slices.Equal(sids(p.Messages), []string{sidOf(2)}) {
		t.Fatalf("window = %q gap %v, %v; want the dated message and a gap", sids(p.Messages), p.Gap, err)
	}
}

// A next_page_uri off the account's message list is never followed.
func TestListInboundForeignNextPage(t *testing.T) {
	c, s := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"messages":[],"next_page_uri":"https://evil.example/2010-04-01/Accounts/%s/Messages.json?Page=1"}`, accountSID)
	})
	_, err := c.ListInbound(t.Context(), accountSID, tokenCred, twilio.InboundQuery{To: channelNumber, Since: windowStart, Limit: 10})
	if model.CodeOf(err) != model.Permanent || s.count() != 1 {
		t.Fatalf("foreign next page = %v after %d requests, want permanent after 1", err, s.count())
	}
}

// Listing failures map as reconcile's do: a 429 is rate_limited with
// Twilio's Retry-After, refused credentials unauthorized.
func TestListInboundFailures(t *testing.T) {
	for _, c := range []struct {
		name string
		h    http.HandlerFunc
		code model.ErrorCode
	}{
		{"rate limited", reply(http.StatusTooManyRequests, `{"code":20429,"status":429}`, "Retry-After", "7"), model.RateLimited},
		{"refused credentials", reply(http.StatusUnauthorized, `{"code":20003,"status":401}`), model.Unauthorized},
		{"server error", reply(http.StatusServiceUnavailable, `{"status":503}`), model.Transient},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl, _ := newClient(t, c.h)
			_, err := cl.ListInbound(t.Context(), accountSID, tokenCred, twilio.InboundQuery{To: channelNumber, Since: windowStart, Limit: 10})
			if model.CodeOf(err) != c.code {
				t.Fatalf("error = %v, want %s", err, c.code)
			}
			var me *model.Error
			if c.code == model.RateLimited && (!errors.As(err, &me) || me.RetryAfter != 7*time.Second) {
				t.Fatalf("rate limit %v carries no Retry-After of 7s", err)
			}
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	p := twilio.Position{Sent: time.Date(2026, 10, 11, 9, 30, 1, 0, time.UTC), SID: "MM0123456789abcdef0123456789abcdef"}
	got, err := twilio.ParseCursor(p.Cursor())
	if err != nil || !got.Sent.Equal(p.Sent) || got.SID != p.SID {
		t.Fatalf("ParseCursor(%q) = %+v, %v; want %+v", p.Cursor(), got, err, p)
	}
	if got, err := twilio.ParseCursor(""); err != nil || !got.IsZero() {
		t.Fatalf("empty cursor = %+v, %v; want the window's start", got, err)
	}
	for _, bad := range []string{
		"conformance-unknown-cursor", "h1.x.SM00000000000000000000000000000001", "h1.-5.SM00000000000000000000000000000001",
		"h1.1760000000.AC00000000000000000000000000000001", "h1.1760000000.SM0", "h2.1760000000.SM00000000000000000000000000000001",
		"/2010-04-01/Accounts/AC00000000000000000000000000000001/Messages.json?Page=1",
	} {
		if _, err := twilio.ParseCursor(bad); model.CodeOf(err) != model.Validation {
			t.Errorf("ParseCursor(%q) = %v, want validation", bad, err)
		}
	}
}
