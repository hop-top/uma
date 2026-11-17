package google

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

var historySince = testNow.Add(-10 * 24 * time.Hour)

// hold puts a message from peer in the inbox as received at at.
func (e *env) hold(id, peer string, at time.Time, labels ...string) {
	e.g.hold(e.box.user, id, "thread-"+id, rawMessage(peer, testMailbox, "<"+id+"@remote.test>", "hello", "text "+id), at, labels...)
}

// readHistory reads the window page by page and returns the IDs in order,
// the pages read and whether any page reported a gap.
func (e *env) readHistory(t *testing.T, req adapter.HistoryRequest) (ids []string, pages int, gap bool) {
	t.Helper()
	if req.Since.IsZero() {
		req.Since = historySince
	}
	for range 200 {
		p, err := e.inst.History(t.Context(), req)
		if err != nil {
			t.Fatalf("History(cursor %q): %v", req.Cursor, err)
		}
		if len(p.Envelopes) > req.Limit {
			t.Fatalf("page holds %d envelopes, limit %d", len(p.Envelopes), req.Limit)
		}
		pages++
		gap = gap || p.Gap
		for _, env := range p.Envelopes {
			ids = append(ids, env.MessageID)
		}
		if p.Next == "" {
			return ids, pages, gap
		}
		if p.Next == req.Cursor {
			t.Fatalf("cursor %q did not advance", p.Next)
		}
		req.Cursor = p.Next
	}
	t.Fatal("window not read")
	return nil, 0, false
}

func (e *env) cursorOf(t *testing.T, c string) historyCursor {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		t.Fatalf("cursor %q is not base64url: %v", c, err)
	}
	var cur historyCursor
	if err := json.Unmarshal(b, &cur); err != nil {
		t.Fatalf("cursor %s is not JSON: %v", b, err)
	}
	return cur
}

func TestHistoryDeclaredAndReader(t *testing.T) {
	f := NewFactory(Options{})
	if !slices.Contains(f.Describe().Capabilities, model.CapabilityHistory) {
		t.Fatalf("capabilities %v lack history", f.Describe().Capabilities)
	}
	e := newEnv(t, Config{})
	var _ adapter.HistoryReader = e.inst
	st, _ := e.inst.Capabilities(t.Context())
	if !slices.ContainsFunc(st, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityHistory && s.Available }) {
		t.Fatalf("history not available in %v", st)
	}
}

// Oldest first by (internalDate, ID), across days, whatever order the
// provider lists in and the order the mail was added.
func TestHistoryOldestFirstByInternalDateThenID(t *testing.T) {
	e := newEnv(t, Config{})
	day := testNow.Add(-5 * 24 * time.Hour)
	e.hold("m-c", testPeer, day.Add(3*time.Hour))
	e.hold("m-a", testPeer, day.Add(time.Hour))
	e.hold("m-z", testPeer, day.Add(2*time.Hour))
	e.hold("m-b", testPeer, day.Add(2*time.Hour)) // same instant as m-z: ID breaks the tie
	e.hold("m-late", testPeer, day.Add(48*time.Hour))
	e.hold("m-early", testPeer, day.Add(-30*time.Hour))
	got, _, gap := e.readHistory(t, adapter.HistoryRequest{Limit: 100})
	want := []string{"m-early", "m-a", "m-b", "m-z", "m-c", "m-late"}
	if !slices.Equal(got, want) || gap {
		t.Fatalf("history = %q gap %v, want %q", got, gap, want)
	}
	for _, limit := range []int{1, 2, 3} {
		if got, _, _ := e.readHistory(t, adapter.HistoryRequest{Limit: limit}); !slices.Equal(got, want) {
			t.Fatalf("limit %d: history = %q, want %q", limit, got, want)
		}
	}
}

// The envelope is the receiver's: same function, same message ID,
// destination and event ID.
func TestHistoryEnvelopeEqualsInbound(t *testing.T) {
	e := newEnv(t, Config{Addresses: []string{"desk@example.test", "other@example.test"}})
	e.g.hold(e.box.user, "m-1", "thread-1", rawMessage(testPeer, "other@example.test", "<m-1@remote.test>", "subject", "body"), testNow.Add(-time.Hour))
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 10})
	if err != nil || len(p.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v", p, err)
	}
	in, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), &testSink{}, "m-1")
	if err != nil || skip {
		t.Fatalf("fetchIngress: skip %v, %v", skip, err)
	}
	if !reflect.DeepEqual(p.Envelopes[0], in.Envelope) {
		t.Fatalf("history envelope %+v differs from inbound %+v", p.Envelopes[0], in.Envelope)
	}
	got := p.Envelopes[0]
	if got.MessageID != "m-1" || got.Destination != "other@example.test" || got.EventID != "" || got.Kind != model.EnvelopeMessage {
		t.Fatalf("envelope id %q destination %q event %q kind %q", got.MessageID, got.Destination, got.EventID, got.Kind)
	}
}

// Since is in, Until is out, judged on internalDate.
func TestHistoryWindowIsHalfOpen(t *testing.T) {
	e := newEnv(t, Config{})
	since, until := testNow.Add(-48*time.Hour), testNow.Add(-24*time.Hour)
	e.hold("before-since", testPeer, since.Add(-time.Millisecond))
	e.hold("at-since", testPeer, since)
	e.hold("inside", testPeer, since.Add(12*time.Hour))
	e.hold("before-until", testPeer, until.Add(-time.Millisecond))
	e.hold("at-until", testPeer, until)
	e.hold("after-until", testPeer, until.Add(time.Hour))
	got, _, _ := e.readHistory(t, adapter.HistoryRequest{Since: since, Until: until, Limit: 100})
	if want := []string{"at-since", "inside", "before-until"}; !slices.Equal(got, want) {
		t.Fatalf("window [since, until) = %q, want %q", got, want)
	}
	// Open above: now is the end, so the future is out.
	e.hold("future", testPeer, testNow.Add(time.Hour))
	got, _, _ = e.readHistory(t, adapter.HistoryRequest{Since: since, Limit: 100})
	if slices.Contains(got, "future") || !slices.Contains(got, "after-until") {
		t.Fatalf("open window = %q", got)
	}
}

// Until is judged to the millisecond, inside the last second listed.
func TestHistoryUntilIsExclusiveWithinASecond(t *testing.T) {
	e := newEnv(t, Config{})
	until := testNow.Add(-time.Hour).Add(500 * time.Millisecond)
	e.hold("just-before", testPeer, until.Add(-time.Millisecond))
	e.hold("at", testPeer, until)
	e.hold("just-after", testPeer, until.Add(time.Millisecond))
	got, _, _ := e.readHistory(t, adapter.HistoryRequest{Until: until, Limit: 10})
	if want := []string{"just-before"}; !slices.Equal(got, want) {
		t.Fatalf("history = %q, want %q", got, want)
	}
}

// Only the receive scope: INBOX, neither DRAFT nor SENT.
func TestHistoryReceivedMailOnly(t *testing.T) {
	e := newEnv(t, Config{})
	at := testNow.Add(-time.Hour)
	e.hold("inbox", testPeer, at)
	e.hold("archived", testPeer, at, "UNREAD")
	e.hold("spam", testPeer, at, "SPAM")
	e.hold("draft", testPeer, at, labelInbox, labelDraft)
	e.hold("sent", testPeer, at, labelSent)
	e.hold("sent-to-self", testPeer, at, labelInbox, labelSent)
	e.hold("trash", testPeer, at, "TRASH")
	got, _, gap := e.readHistory(t, adapter.HistoryRequest{Limit: 100})
	if want := []string{"inbox"}; !slices.Equal(got, want) || gap {
		t.Fatalf("history = %q gap %v, want %q", got, gap, want)
	}
	for _, r := range e.g.requests() {
		if strings.Contains(r, "/messages?") && !strings.Contains(r, "labelIds=INBOX") {
			t.Fatalf("list without the INBOX label: %s", r)
		}
	}
}

// The cursor is {v, k, lo, hi, rest} base64url JSON; rest carries what the
// call did not deliver, and a resume lists nothing again.
func TestHistoryCursorShapeAndRestResume(t *testing.T) {
	e := newEnv(t, Config{})
	day := testNow.Add(-3 * 24 * time.Hour)
	for n, id := range []string{"a", "b", "c", "d"} {
		e.hold(id, testPeer, day.Add(time.Duration(n)*time.Minute))
	}
	e.hold("later", testPeer, day.Add(24*time.Hour))
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 1})
	if err != nil || !slices.Equal(envIDs(p), []string{"a"}) || p.Next == "" {
		t.Fatalf("first page = %q next %q, %v", envIDs(p), p.Next, err)
	}
	cur := e.cursorOf(t, p.Next)
	if cur.Version != 1 || cur.Scope == "" || !slices.Equal(cur.Rest, []string{"b", "c", "d"}) {
		t.Fatalf("cursor = %+v, want v1, a scope and rest [b c d]", cur)
	}
	if want := (day.Truncate(time.Second).Unix()/historySliceSeconds + 1) * historySliceSeconds; cur.Lo <= day.Unix() || cur.Lo > want+historySliceSeconds || cur.Hi != testNow.Unix() {
		t.Fatalf("cursor lo %d hi %d: lo is the end of the slice listed, hi is now %d", cur.Lo, cur.Hi, testNow.Unix())
	}
	lists := e.g.count("GET /gmail/v1/users/" + testMailbox + "/messages?")
	p, err = e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 2, Cursor: p.Next})
	if err != nil || !slices.Equal(envIDs(p), []string{"b", "c"}) {
		t.Fatalf("second page = %q, %v", envIDs(p), err)
	}
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/messages?"); n != lists {
		t.Fatalf("a resume with rest listed again: %d lists, was %d", n, lists)
	}
	if cur = e.cursorOf(t, p.Next); !slices.Equal(cur.Rest, []string{"d"}) {
		t.Fatalf("cursor rest = %v, want [d]", cur.Rest)
	}
}

func envIDs(p adapter.HistoryPage) []string {
	var out []string
	for _, e := range p.Envelopes {
		out = append(out, e.MessageID)
	}
	return out
}

// A cursor of another read, or none, is Validation before any call.
func TestHistoryForeignCursorIsValidation(t *testing.T) {
	e := newEnv(t, Config{})
	e.hold("a", testPeer, testNow.Add(-24*time.Hour))
	e.hold("b", testPeer, testNow.Add(-23*time.Hour))
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 1})
	if err != nil || p.Next == "" {
		t.Fatalf("History = %+v, %v", p, err)
	}
	calls := len(e.g.requests())
	rt := route(testPeer, "")
	for name, req := range map[string]adapter.HistoryRequest{
		"other since": {Since: historySince.Add(time.Hour), Cursor: p.Next, Limit: 1},
		"other until": {Since: historySince, Until: testNow, Cursor: p.Next, Limit: 1},
		"other route": {Since: historySince, Route: &rt, Cursor: p.Next, Limit: 1},
		"garbage":     {Since: historySince, Cursor: "conformance-unknown-cursor", Limit: 1},
		"not json":    {Since: historySince, Cursor: base64.RawURLEncoding.EncodeToString([]byte("nope")), Limit: 1},
		"version":     {Since: historySince, Cursor: base64.RawURLEncoding.EncodeToString([]byte(`{"v":2}`)), Limit: 1},
	} {
		if _, err := e.inst.History(t.Context(), req); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
	if n := len(e.g.requests()); n != calls {
		t.Fatalf("a refused cursor made %d provider calls", n-calls)
	}
}

func TestHistoryRequestValidation(t *testing.T) {
	e := newEnv(t, Config{})
	other := route(testPeer, "")
	other.Channel = model.ChannelRef{Tenant: "t1", ID: "00000000-0000-7000-8000-0000000000c9"}
	wrongNS := route(testPeer, "")
	wrongNS.Namespace = "other.address"
	notAddr := route("Peer <peer@remote.test>", "")
	for name, req := range map[string]adapter.HistoryRequest{
		"no limit":      {Since: historySince},
		"no since":      {Limit: 1},
		"other channel": {Since: historySince, Limit: 1, Route: &other},
		"other ns":      {Since: historySince, Limit: 1, Route: &wrongNS},
		"not address":   {Since: historySince, Limit: 1, Route: &notAddr},
	} {
		if _, err := e.inst.History(t.Context(), req); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
	if n := len(e.g.requests()); n != 0 {
		t.Fatalf("refused requests made %d provider calls", n)
	}
}

// A listing that does not end on its first page is halved, until it does;
// every message comes once, in order.
func TestHistoryHalvesASliceThatDoesNotFit(t *testing.T) {
	e := newEnv(t, Config{})
	e.inst.f.opts.listMax = 3
	day := testNow.Add(-4 * 24 * time.Hour).Truncate(24 * time.Hour)
	var want []string
	for n := range 8 {
		id := string(rune('a' + n))
		e.hold(id, testPeer, day.Add(time.Duration(n)*3*time.Hour))
		want = append(want, id)
	}
	e.hold("other-day", testPeer, day.Add(30*time.Hour))
	want = append(want, "other-day")
	got, _, gap := e.readHistory(t, adapter.HistoryRequest{Limit: 100})
	if !slices.Equal(got, want) || gap {
		t.Fatalf("history = %q gap %v, want %q", got, gap, want)
	}
	// A narrower window than a day was listed, and none held a token drained.
	var narrow bool
	for _, r := range e.g.requests() {
		a, b, ok := listBounds(r)
		narrow = narrow || (ok && b-a < historySliceSeconds+2)
	}
	if !narrow {
		t.Fatalf("no slice was halved: %q", e.g.requests())
	}
	for _, r := range e.g.requests() {
		if strings.Contains(r, "pageToken=") {
			// Only a slice a second wide may drain pages; none here.
			t.Fatalf("pages drained instead of halving: %s", r)
		}
	}
}

// listBounds reads the after:/before: seconds of a list request.
func listBounds(r string) (a, b int64, ok bool) {
	if !strings.HasPrefix(r, "GET ") || !strings.Contains(r, "/messages?") {
		return 0, 0, false
	}
	am, bm := reAfter.FindStringSubmatch(r), reBefore.FindStringSubmatch(r)
	if am == nil || bm == nil {
		return 0, 0, false
	}
	a, b = atoi64(am[1]), atoi64(bm[1])
	return a, b, true
}

func atoi64(s string) int64 {
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}

// The q carries after: and before: in epoch seconds around the slice.
func TestHistoryListQuery(t *testing.T) {
	e := newEnv(t, Config{})
	since, until := testNow.Add(-2*time.Hour), testNow.Add(-time.Hour)
	e.hold("a", testPeer, since.Add(time.Minute))
	e.readHistory(t, adapter.HistoryRequest{Since: since, Until: until, Limit: 10})
	var found bool
	for _, r := range e.g.requests() {
		if a, b, ok := listBounds(r); ok {
			found = true
			if a != since.Unix()-1 || b != until.Unix()+1 {
				t.Fatalf("list %s: after %d before %d, want the window %d..%d widened by a second", r, a, b, since.Unix(), until.Unix())
			}
			if !strings.Contains(r, "labelIds=INBOX") || !strings.Contains(r, "maxResults=500") {
				t.Fatalf("list %s lacks labelIds=INBOX or maxResults=500", r)
			}
		}
	}
	if !found {
		t.Fatalf("no list request: %q", e.g.requests())
	}
}

// Days are walked one list at a time, and a long empty stretch is bounded
// per call: the cursor resumes it.
func TestHistoryWalksDaysAndBoundsEmptyStretches(t *testing.T) {
	e := newEnv(t, Config{})
	e.hold("old", testPeer, testNow.Add(-50*24*time.Hour))
	e.hold("new", testPeer, testNow.Add(-time.Hour))
	since := testNow.Add(-120 * 24 * time.Hour)
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: since, Limit: 10})
	if err != nil || len(p.Envelopes) != 0 || p.Next == "" {
		t.Fatalf("first page %q next %q, %v: the first 31 days are empty", envIDs(p), p.Next, err)
	}
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/messages?"); n > maxHistorySlices {
		t.Fatalf("one call listed %d slices, bound %d", n, maxHistorySlices)
	}
	got, pages, _ := e.readHistory(t, adapter.HistoryRequest{Since: since, Limit: 10})
	if want := []string{"old", "new"}; !slices.Equal(got, want) || pages < 2 {
		t.Fatalf("history = %q in %d pages, want %q in several", got, pages, want)
	}
}

// Gap for a message listed that can never be read; the others come.
func TestHistoryGapForUnreadableMessage(t *testing.T) {
	e := newEnv(t, Config{})
	at := testNow.Add(-time.Hour)
	e.hold("ok-1", testPeer, at)
	e.hold("bad", testPeer, at.Add(time.Minute))
	e.hold("ok-2", testPeer, at.Add(2*time.Minute))
	answerFor(e.g, testMailbox, "bad", "raw", 200, undecodableRaw("bad"), always)
	got, _, gap := e.readHistory(t, adapter.HistoryRequest{Limit: 100})
	if want := []string{"ok-1", "ok-2"}; !slices.Equal(got, want) || !gap {
		t.Fatalf("history = %q gap %v, want %q with a gap", got, gap, want)
	}
	// One at a time too: the gap is on the page that met the message.
	var gaps int
	req := adapter.HistoryRequest{Since: historySince, Limit: 1}
	for range 10 {
		p, err := e.inst.History(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if p.Gap {
			gaps++
		}
		if p.Next == "" {
			break
		}
		req.Cursor = p.Next
	}
	if gaps != 1 {
		t.Fatalf("%d pages reported a gap, want 1", gaps)
	}
}

// A message listed and gone when read is absent, not a gap.
func TestHistoryVanishedMessageIsAbsent(t *testing.T) {
	e := newEnv(t, Config{})
	at := testNow.Add(-time.Hour)
	e.hold("ok", testPeer, at)
	e.hold("gone", testPeer, at.Add(time.Minute))
	answerWhen(e.g, func(r *http.Request) bool {
		return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/messages/gone")
	}, 404, `{"error":{"code":404,"errors":[{"reason":"notFound"}]}}`, always)
	got, _, gap := e.readHistory(t, adapter.HistoryRequest{Limit: 100})
	if want := []string{"ok"}; !slices.Equal(got, want) || gap {
		t.Fatalf("history = %q gap %v, want %q without a gap", got, gap, want)
	}
}

// A token under gmail.metadata cannot use q: Unsupported, not Unauthorized.
func TestHistoryMetadataScopeIsUnsupported(t *testing.T) {
	e := newEnv(t, Config{})
	answerWhen(e.g, func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/messages") },
		403, `{"error":{"code":403,"message":"Metadata scope does not support 'q' parameter","errors":[{"reason":"forbidden"}],"status":"PERMISSION_DENIED"}}`, always)
	_, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 10})
	_ = wantCode(t, err, model.Unsupported)
	// An ordinary 403 stays a missing scope.
	answerWhen(e.g, func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/messages") },
		403, `{"error":{"code":403,"message":"Insufficient Permission","errors":[{"reason":"insufficientPermissions"}]}}`, always)
	_, err = e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 10})
	_ = wantCode(t, err, model.Unauthorized)
}

// Throttling is returned with its wait; the reader neither sleeps nor
// retries.
func TestHistoryThrottlingIsReturnedNotWaited(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		code   model.ErrorCode
	}{
		"429":        {429, `{"error":{"code":429,"errors":[{"reason":"rateLimitExceeded"}]}}`, model.RateLimited},
		"403 reason": {403, `{"error":{"code":403,"errors":[{"reason":"userRateLimitExceeded"}]}}`, model.RateLimited},
		"503":        {503, `{"error":{"code":503,"errors":[{"reason":"backendError"}]}}`, model.Transient},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, Config{})
			e.hold("a", testPeer, testNow.Add(-time.Hour))
			e.g.mu.Lock()
			e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/messages") {
					return false
				}
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
				return true
			}
			e.g.mu.Unlock()
			start := time.Now()
			_, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 10})
			me := wantCode(t, err, tc.code)
			if me.RetryAfter != 7*time.Second || me.Retry != model.RetrySameOperation {
				t.Fatalf("retry %v after %v, want same operation after 7s", me.Retry, me.RetryAfter)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("History took %v: it must not wait", d)
			}
			if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/messages?"); n != 1 {
				t.Fatalf("%d list calls, want 1: no retry", n)
			}
		})
	}
}

// A throttled get mid-slice returns the error and leaves the cursor where
// it was, so the host retries the same page.
func TestHistoryThrottledFetchKeepsTheCursor(t *testing.T) {
	e := newEnv(t, Config{})
	at := testNow.Add(-time.Hour)
	e.hold("a", testPeer, at)
	e.hold("b", testPeer, at.Add(time.Minute))
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 1})
	if err != nil || !slices.Equal(envIDs(p), []string{"a"}) {
		t.Fatalf("first page %q, %v", envIDs(p), err)
	}
	served := answerFor(e.g, testMailbox, "b", "raw", 429, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`, always)
	_, err = e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 1, Cursor: p.Next})
	_ = wantCode(t, err, model.RateLimited)
	if served.Load() != 1 {
		t.Fatalf("throttled fetch served %d times", served.Load())
	}
	e.g.mu.Lock()
	e.g.override = nil
	e.g.mu.Unlock()
	p, err = e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 1, Cursor: p.Next})
	if err != nil || !slices.Equal(envIDs(p), []string{"b"}) || p.Next != "" {
		t.Fatalf("retry page %q next %q, %v", envIDs(p), p.Next, err)
	}
}

// A route reads the sender's mail: from: in the query, and only that
// address in the result.
func TestHistoryRouteReadsOneSender(t *testing.T) {
	e := newEnv(t, Config{})
	at := testNow.Add(-time.Hour)
	e.hold("p1", testPeer, at)
	e.hold("q1", "someone@remote.test", at.Add(time.Minute))
	e.hold("p2", testPeer, at.Add(2*time.Minute))
	e.hold("lookalike", "x"+testPeer, at.Add(3*time.Minute))
	rt := route(testPeer, "opened")
	got, _, _ := e.readHistory(t, adapter.HistoryRequest{Route: &rt, Limit: 100})
	if want := []string{"p1", "p2"}; !slices.Equal(got, want) {
		t.Fatalf("route history = %q, want %q", got, want)
	}
	if !slices.ContainsFunc(e.g.requests(), func(r string) bool { return strings.Contains(r, "from:"+testPeer) }) {
		t.Fatalf("no from: in the queries: %q", e.g.requests())
	}
	all, _, _ := e.readHistory(t, adapter.HistoryRequest{Limit: 100})
	if len(all) != 4 {
		t.Fatalf("channel history = %q, want 4", all)
	}
}

// A read changes nothing and touches no receiver state.
func TestHistoryIsReadOnly(t *testing.T) {
	proto := &memProtocol{}
	e := newEnv(t, Config{}, withProtocol(proto))
	e.hold("a", testPeer, testNow.Add(-time.Hour))
	e.readHistory(t, adapter.HistoryRequest{Limit: 10})
	if proto.saves != 0 || len(proto.ext) != 0 {
		t.Fatalf("history wrote receiver state: %d saves", proto.saves)
	}
	for _, r := range e.g.requests() {
		if !strings.HasPrefix(r, "GET ") {
			t.Fatalf("history sent %s", r)
		}
		if strings.Contains(r, "/history") || strings.Contains(r, "/profile") {
			t.Fatalf("history read the receiver's position: %s", r)
		}
	}
}

// Attachments are described and never staged.
func TestHistoryDescribesAttachments(t *testing.T) {
	e := newEnv(t, Config{})
	raw := []byte("From: " + testPeer + "\r\nTo: " + testMailbox + "\r\nSubject: s\r\nMessage-ID: <att@remote.test>\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n--B\r\n" +
		"Content-Type: text/plain; name=\"a.txt\"\r\nContent-Disposition: attachment; filename=\"a.txt\"\r\n\r\nhello\r\n--B--\r\n")
	e.g.hold(e.box.user, "att", "thread-att", raw, testNow.Add(-time.Hour))
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 10})
	if err != nil || len(p.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v", p, err)
	}
	var described int
	for _, part := range p.Envelopes[0].Content.Parts {
		switch {
		case part.Kind == model.PartAttachment:
			t.Fatalf("history staged an attachment: %+v", part)
		case part.Structured != nil && part.Structured.Name == PartAttachment:
			described++
			var a attachmentPart
			if json.Unmarshal(part.Structured.Data, &a) != nil || a.Staged || a.Filename != "a.txt" {
				t.Fatalf("attachment descriptor %s", part.Structured.Data)
			}
		}
	}
	if described != 1 {
		t.Fatalf("%d attachment descriptors, want 1", described)
	}
}

// forged is a cursor of this read with the walk's state replaced: the scope
// is known to the caller, so it guards nothing.
func (e *env) forged(t *testing.T, valid string, edit func(*historyCursor)) string {
	t.Helper()
	cur := e.cursorOf(t, valid)
	edit(&cur)
	return cur.encode()
}

// A cursor naming messages outside the read's scope delivers none of them.
func TestHistoryForgedRestDeliversNothingOutOfScope(t *testing.T) {
	e := newEnv(t, Config{})
	at := testNow.Add(-24 * time.Hour)
	e.hold("a", testPeer, at)
	e.hold("b", testPeer, at.Add(time.Minute))
	e.hold("in-scope", testPeer, at.Add(2*time.Minute))
	e.hold("sent", testPeer, at, labelSent)
	e.hold("sent-inbox", testPeer, at, labelInbox, labelSent)
	e.hold("draft", testPeer, at, labelInbox, labelDraft)
	e.hold("archived", testPeer, at, "UNREAD")
	e.hold("spam", testPeer, at, labelInbox, labelSpam)
	e.hold("trash", testPeer, at, labelInbox, labelTrash)
	e.hold("too-old", testPeer, historySince.Add(-time.Hour))
	e.hold("too-new", testPeer, testNow.Add(-time.Hour))
	e.hold("other-sender", "someone@remote.test", at)
	since, until := historySince, testNow.Add(-2*time.Hour)
	rt := route(testPeer, "")
	for name, rtp := range map[string]*model.ExternalRef{"channel": nil, "conversation": &rt} {
		req := adapter.HistoryRequest{Since: since, Until: until, Route: rtp, Limit: 1}
		p, err := e.inst.History(t.Context(), req)
		if err != nil || p.Next == "" {
			t.Fatalf("%s: first page %+v, %v", name, p, err)
		}
		req.Limit = 100
		req.Cursor = e.forged(t, p.Next, func(c *historyCursor) {
			c.Rest = []string{"sent", "sent-inbox", "draft", "archived", "spam", "trash", "too-old", "too-new", "other-sender", "in-scope", "missing"}
		})
		p, err = e.inst.History(t.Context(), req)
		if err != nil {
			t.Fatalf("%s: forged page: %v", name, err)
		}
		want := []string{"other-sender", "in-scope"}
		if rtp != nil {
			want = []string{"in-scope"}
		}
		if got := envIDs(p); !slices.Equal(got, want) {
			t.Fatalf("%s: forged cursor delivered %q, want only %q", name, got, want)
		}
	}
}

// Bounds outside the request's window are Validation before any call.
func TestHistoryForgedBoundsAreValidation(t *testing.T) {
	e := newEnv(t, Config{})
	e.hold("a", testPeer, testNow.Add(-24*time.Hour))
	e.hold("b", testPeer, testNow.Add(-23*time.Hour))
	until := testNow.Add(-time.Hour)
	req := adapter.HistoryRequest{Since: historySince, Until: until, Limit: 1}
	p, err := e.inst.History(t.Context(), req)
	if err != nil || p.Next == "" {
		t.Fatalf("first page %+v, %v", p, err)
	}
	calls := len(e.g.requests())
	for name, edit := range map[string]func(*historyCursor){
		"lo before since": func(c *historyCursor) { c.Lo = historySince.Unix() - 86400 },
		"hi after until":  func(c *historyCursor) { c.Hi = until.Unix() + 86400 },
		"lo negative":     func(c *historyCursor) { c.Lo = -5 },
	} {
		req.Cursor = e.forged(t, p.Next, edit)
		if _, err := e.inst.History(t.Context(), req); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
	if n := len(e.g.requests()); n != calls {
		t.Fatalf("a forged cursor made %d provider calls", n-calls)
	}
	// Without Until the end is now: a later hi is forged too.
	open := adapter.HistoryRequest{Since: historySince, Limit: 1}
	p, err = e.inst.History(t.Context(), open)
	if err != nil || p.Next == "" {
		t.Fatalf("open first page %+v, %v", p, err)
	}
	open.Cursor = e.forged(t, p.Next, func(c *historyCursor) { c.Hi = testNow.Unix() + 86400 })
	if _, err := e.inst.History(t.Context(), open); model.CodeOf(err) != model.Validation {
		t.Errorf("open hi after now: err = %v, want validation", err)
	}
}
