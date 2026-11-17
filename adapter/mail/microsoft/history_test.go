package microsoft

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

var historyEpoch = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// holdMail puts a message received at, from peer, into the inbox without a
// delta change: past mail no receiver round lists.
func (e *env) holdMail(id, peer string, at time.Time) {
	e.g.open(e.box.name, gMessage{
		ID: id, InternetMessageID: "<" + id + "@peer.test>", Subject: "subject " + id, ReceivedDateTime: at,
		From:         &gRecipient{EmailAddress: gEmailAddress{Address: peer}},
		ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: testMailbox}}},
		Body:         &gItemBody{ContentType: "text", Content: "body " + id},
	})
}

// readAll reads a window to its end in pages of limit, returning the
// message IDs and the pages' cursors.
func (e *env) readAll(t *testing.T, req adapter.HistoryRequest) (ids []string, nexts []string) {
	t.Helper()
	for range 50 {
		p, err := e.inst.History(t.Context(), req)
		if err != nil {
			t.Fatalf("History(%+v): %v", req, err)
		}
		if len(p.Envelopes) > req.Limit {
			t.Fatalf("page of %d envelopes over limit %d", len(p.Envelopes), req.Limit)
		}
		for _, env := range p.Envelopes {
			ids = append(ids, env.MessageID)
		}
		if p.Next == "" {
			return ids, nexts
		}
		nexts = append(nexts, p.Next)
		req.Cursor = p.Next
	}
	t.Fatal("window not read")
	return nil, nil
}

func TestHistoryDeclared(t *testing.T) {
	d := NewFactory(Options{}).Describe()
	if !slices.Contains(d.Capabilities, model.CapabilityHistory) {
		t.Fatalf("capabilities %v lack history", d.Capabilities)
	}
	e := newEnv(t, Config{})
	st, err := e.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(st, func(c model.CapabilityStatus) bool { return c.Name == model.CapabilityHistory && c.Available }) {
		t.Fatalf("history not available: %+v", st)
	}
}

func TestHistoryListsInboxWithFilterOrderAndPrefer(t *testing.T) {
	e := newEnv(t, Config{})
	since, until := historyEpoch, historyEpoch.Add(48*time.Hour)
	e.holdMail("m1", "a@peer.test", since.Add(time.Hour))
	if _, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: since, Until: until, Limit: 10}); err != nil {
		t.Fatal(err)
	}
	reqs := e.g.requests()
	want := "GET /v1.0/users/" + testMailbox + "/mailFolders/inbox/messages?"
	if len(reqs) != 1 || !strings.HasPrefix(reqs[0], want) {
		t.Fatalf("requests %q, want one list of the inbox", reqs)
	}
	for _, frag := range []string{
		"$filter=receivedDateTime ge 2026-10-01T00:00:00Z and receivedDateTime lt 2026-10-03T00:00:00Z",
		"$orderby=receivedDateTime asc", "$top=10", "internetMessageHeaders",
	} {
		if !strings.Contains(reqs[0], frag) {
			t.Errorf("request %q lacks %q", reqs[0], frag)
		}
	}
	e.g.mu.Lock()
	plain := slices.Clone(e.g.plain)
	e.g.mu.Unlock()
	if len(plain) != 0 {
		t.Fatalf("requests sent without Prefer ImmutableId: %q", plain)
	}
}

func TestHistoryOldestFirstAcrossPages(t *testing.T) {
	e := newEnv(t, Config{})
	// Inserted out of order: the list, not the insertion, orders them.
	for _, id := range []string{"m3", "m1", "m5", "m2", "m4"} {
		at := map[string]int{"m1": 1, "m2": 2, "m3": 3, "m4": 4, "m5": 5}[id]
		e.holdMail(id, "a@peer.test", historyEpoch.Add(time.Duration(at)*time.Hour))
	}
	for _, limit := range []int{2, 1, 3, 10} {
		ids, _ := e.readAll(t, adapter.HistoryRequest{Since: historyEpoch, Limit: limit})
		if !slices.Equal(ids, []string{"m1", "m2", "m3", "m4", "m5"}) {
			t.Fatalf("limit %d: messages %q, want m1..m5 oldest first", limit, ids)
		}
	}
	for _, r := range e.g.requests() {
		if strings.Contains(r, "/delta") {
			t.Fatalf("history ran a delta query: %s", r)
		}
	}
}

func TestHistoryUntilIsExclusive(t *testing.T) {
	e := newEnv(t, Config{})
	until := historyEpoch.Add(10 * time.Hour)
	e.holdMail("before-since", "a@peer.test", historyEpoch.Add(-time.Second))
	e.holdMail("at-since", "a@peer.test", historyEpoch)
	e.holdMail("just-before", "a@peer.test", until.Add(-time.Second))
	e.holdMail("at-until", "a@peer.test", until)
	e.holdMail("after", "a@peer.test", until.Add(time.Hour))
	ids, _ := e.readAll(t, adapter.HistoryRequest{Since: historyEpoch, Until: until, Limit: 10})
	if !slices.Equal(ids, []string{"at-since", "just-before"}) {
		t.Fatalf("messages %q, want Since inclusive and Until exclusive", ids)
	}
	// Open window: no upper bound is sent.
	ids, _ = e.readAll(t, adapter.HistoryRequest{Since: historyEpoch, Limit: 10})
	if len(ids) != 4 {
		t.Fatalf("open window read %q", ids)
	}
	last := e.g.requests()[len(e.g.requests())-1]
	if strings.Contains(last, "receivedDateTime lt") {
		t.Fatalf("open window sent an upper bound: %s", last)
	}
}

func TestHistoryBoundsRoundUpToTheSecond(t *testing.T) {
	e := newEnv(t, Config{})
	e.holdMail("m1", "a@peer.test", historyEpoch.Add(time.Second))
	until := historyEpoch.Add(time.Second + 500*time.Millisecond)
	ids, _ := e.readAll(t, adapter.HistoryRequest{Since: historyEpoch.Add(500 * time.Millisecond), Until: until, Limit: 10})
	if !slices.Equal(ids, []string{"m1"}) {
		t.Fatalf("messages %q", ids)
	}
	if r := e.g.requests()[0]; !strings.Contains(r, "ge 2026-10-01T00:00:01Z and receivedDateTime lt 2026-10-01T00:00:02Z") {
		t.Fatalf("bounds not rounded up to the second: %s", r)
	}
}

func TestHistorySkipsDrafts(t *testing.T) {
	e := newEnv(t, Config{})
	e.holdMail("m1", "a@peer.test", historyEpoch.Add(time.Hour))
	e.g.open(e.box.name, gMessage{ID: "d1", IsDraft: true, ReceivedDateTime: historyEpoch.Add(2 * time.Hour),
		From: &gRecipient{EmailAddress: gEmailAddress{Address: "a@peer.test"}}})
	e.holdMail("m2", "a@peer.test", historyEpoch.Add(3*time.Hour))
	ids, _ := e.readAll(t, adapter.HistoryRequest{Since: historyEpoch, Limit: 10})
	if !slices.Equal(ids, []string{"m1", "m2"}) {
		t.Fatalf("messages %q, draft not skipped", ids)
	}
}

// The envelope is the one the receiver builds: same message ID, destination,
// EventID and payload, by the same mapping.
func TestHistoryEnvelopeEqualsInbound(t *testing.T) {
	e := newEnv(t, Config{Addresses: []string{"support@contoso.test"}})
	e.g.open(e.box.name, gMessage{
		ID: "m1", InternetMessageID: "<m1@peer.test>", ConversationID: "conv-1", Subject: "Re: order",
		ReceivedDateTime: historyEpoch.Add(time.Hour),
		From:             &gRecipient{EmailAddress: gEmailAddress{Address: "Peer@Example.test"}},
		ToRecipients:     []gRecipient{{EmailAddress: gEmailAddress{Address: "Support@contoso.test"}}},
		Body:             &gItemBody{ContentType: "text", Content: "reply"},
		InternetMessageHeaders: []gHeader{
			{Name: "Message-ID", Value: "<m1@peer.test>"}, {Name: "In-Reply-To", Value: "<p1@uma.test>"},
			{Name: "Authentication-Results", Value: "spf=pass"},
		},
	})
	want := fetch(t, e, "m1")
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 10})
	if err != nil || len(p.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v", p, err)
	}
	if !reflect.DeepEqual(p.Envelopes[0], want) {
		t.Fatalf("history envelope\n%+v\nreceiver envelope\n%+v", p.Envelopes[0], want)
	}
	if got := p.Envelopes[0]; got.EventID != "" || got.MessageID != "m1" || got.Destination != "support@contoso.test" {
		t.Fatalf("identity %q %q %q", got.EventID, got.MessageID, got.Destination)
	}
}

func TestHistoryDescribesAttachmentsWithoutStaging(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.name, gMessage{
		ID: "m1", ReceivedDateTime: historyEpoch.Add(time.Hour),
		From: &gRecipient{EmailAddress: gEmailAddress{Address: "a@peer.test"}},
		Body: &gItemBody{ContentType: "text", Content: "see file"},
	}, gAttachment{ID: "att-1", Name: "a.txt", ContentType: "text/plain", Size: 3})
	e.g.files["att-1"] = []byte("abc")
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 10})
	if err != nil || len(p.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v", p, err)
	}
	var found bool
	for _, part := range p.Envelopes[0].Content.Parts {
		if part.Structured != nil && part.Structured.Name == PartAttachment {
			var a attachmentPart
			if err := json.Unmarshal(part.Structured.Data, &a); err != nil || a.ID != "att-1" || a.Staged {
				t.Fatalf("attachment part %s: %v", part.Structured.Data, err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no attachment descriptor in %+v", p.Envelopes[0].Content.Parts)
	}
	if n := e.g.count("GET /v1.0/users/" + testMailbox + "/messages/m1/attachments/att-1/$value"); n != 0 {
		t.Fatalf("attachment bytes fetched %d times", n)
	}
}

func TestHistoryRouteFiltersOnSender(t *testing.T) {
	e := newEnv(t, Config{})
	e.holdMail("a1", "a@peer.test", historyEpoch.Add(time.Hour))
	e.holdMail("b1", "b@peer.test", historyEpoch.Add(2*time.Hour))
	e.holdMail("a2", "a@peer.test", historyEpoch.Add(3*time.Hour))
	r := route("a@peer.test", "ignored")
	ids, _ := e.readAll(t, adapter.HistoryRequest{Route: &r, Since: historyEpoch, Limit: 10})
	if !slices.Equal(ids, []string{"a1", "a2"}) {
		t.Fatalf("route messages %q", ids)
	}
	if req := e.g.requests()[0]; !strings.Contains(req, "receivedDateTime ge 2026-10-01T00:00:00Z and from/emailAddress/address eq 'a@peer.test'") ||
		!strings.Contains(req, "$orderby=receivedDateTime asc") {
		t.Fatalf("request %s", req)
	}
	ids, _ = e.readAll(t, adapter.HistoryRequest{Since: historyEpoch, Limit: 10})
	if len(ids) != 3 {
		t.Fatalf("channel read %q", ids)
	}
	// A quote in an address is doubled.
	q := route("o'neil@peer.test", "")
	if _, err := e.inst.History(t.Context(), adapter.HistoryRequest{Route: &q, Since: historyEpoch, Limit: 1}); err != nil {
		t.Fatal(err)
	}
	if req := e.g.requests(); !strings.Contains(req[len(req)-1], "eq 'o''neil@peer.test'") {
		t.Fatalf("request %s", req[len(req)-1])
	}
}

func TestHistoryRejectsBadRequests(t *testing.T) {
	e := newEnv(t, Config{})
	good := route("a@peer.test", "")
	other := good
	other.Channel.ID = "00000000-0000-7000-8000-0000000000c2"
	foreignNS := good
	foreignNS.Namespace = "other.address"
	foreignKind := good
	foreignKind.Kind = "thread"
	noID := good
	noID.ID = ""
	for name, req := range map[string]adapter.HistoryRequest{
		"no limit":     {Since: historyEpoch},
		"no since":     {Limit: 1},
		"other chan":   {Route: &other, Since: historyEpoch, Limit: 1},
		"other ns":     {Route: &foreignNS, Since: historyEpoch, Limit: 1},
		"other kind":   {Route: &foreignKind, Since: historyEpoch, Limit: 1},
		"empty route":  {Route: &noID, Since: historyEpoch, Limit: 1},
		"bad cursor":   {Since: historyEpoch, Limit: 1, Cursor: "!!"},
		"junk cursor":  {Since: historyEpoch, Limit: 1, Cursor: base64.RawURLEncoding.EncodeToString([]byte("junk"))},
		"empty object": {Since: historyEpoch, Limit: 1, Cursor: base64.RawURLEncoding.EncodeToString([]byte("{}"))},
	} {
		_, err := e.inst.History(t.Context(), req)
		if model.CodeOf(err) != model.Validation {
			t.Errorf("%s: error %v, want validation", name, err)
		}
	}
	if n := len(e.g.requests()); n != 0 {
		t.Fatalf("%d Graph calls before validation", n)
	}
}

func TestHistoryCursorShapeAndScope(t *testing.T) {
	e := newEnv(t, Config{})
	for i := range 3 {
		e.holdMail("m"+string(rune('1'+i)), "a@peer.test", historyEpoch.Add(time.Duration(i+1)*time.Hour))
	}
	req := adapter.HistoryRequest{Since: historyEpoch, Limit: 2}
	p, err := e.inst.History(t.Context(), req)
	if err != nil || p.Next == "" || len(p.Envelopes) != 2 {
		t.Fatalf("History = %+v, %v", p, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Next)
	if err != nil {
		t.Fatalf("cursor is not base64url: %v", err)
	}
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if _, has := c["link"]; has || c["v"] != float64(1) || c["k"] == "" || c["p"] != "$skiptoken" || c["t"] != "2" || c["s"] != float64(2) {
		t.Fatalf("cursor %s: want the paging token only, no link", raw)
	}
	n := len(e.g.requests())
	// Valid for the read it was issued for.
	req.Cursor = p.Next
	if _, err := e.inst.History(t.Context(), req); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Foreign: another window, route or limit-independent scope.
	r := route("a@peer.test", "")
	for name, bad := range map[string]adapter.HistoryRequest{
		"since": {Since: historyEpoch.Add(time.Hour), Limit: 2, Cursor: p.Next},
		"until": {Since: historyEpoch, Until: historyEpoch.Add(time.Hour), Limit: 2, Cursor: p.Next},
		"route": {Route: &r, Since: historyEpoch, Limit: 2, Cursor: p.Next},
	} {
		if _, err := e.inst.History(t.Context(), bad); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: error %v, want validation", name, err)
		}
	}
	if got := len(e.g.requests()); got != n+1 {
		t.Fatalf("foreign cursors made Graph calls: %d requests after the resume, want %d", got, n+1)
	}
	// A cursor cannot steer the request: whatever it names, the list is the
	// same one, or the cursor is refused before any call.
	forge := func(mod func(map[string]any)) string {
		m := map[string]any{}
		for k, v := range c {
			m[k] = v
		}
		mod(m)
		b, _ := json.Marshal(m)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	for name, cursor := range map[string]string{
		"link": forge(func(m map[string]any) {
			m["link"] = e.g.srv.URL + "/v1.0/users/victim@contoso.test/mailFolders/sentitems/messages?$skiptoken=1"
		}),
		"other param": forge(func(m map[string]any) { m["p"] = "$filter" }),
		"path param":  forge(func(m map[string]any) { m["p"] = "path" }),
		"no param":    forge(func(m map[string]any) { delete(m, "p") }),
		"big top":     forge(func(m map[string]any) { m["s"] = 100000 }),
		"no top":      forge(func(m map[string]any) { delete(m, "s") }),
		"huge token":  forge(func(m map[string]any) { m["t"] = strings.Repeat("x", 5000) }),
		"no position": forge(func(m map[string]any) { delete(m, "p"); delete(m, "t") }),
	} {
		bad := req
		bad.Cursor = cursor
		if _, err := e.inst.History(t.Context(), bad); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: error %v, want validation", name, err)
		}
	}
	if got := len(e.g.requests()); got != n+1 {
		t.Fatalf("forged cursors reached Graph: %q", e.g.requests())
	}
	// A token is a value, never more query: it cannot add a parameter.
	var seen map[string][]string
	e.g.mu.Lock()
	e.g.override = func(_ http.ResponseWriter, r *http.Request) bool { seen = r.URL.Query(); return false }
	e.g.mu.Unlock()
	inj := req
	inj.Cursor = forge(func(m map[string]any) { m["t"] = "1&$filter=x&$select=y" })
	_, _ = e.inst.History(t.Context(), inj)
	e.g.mu.Lock()
	got := seen
	e.g.mu.Unlock()
	if len(got["$filter"]) != 1 || strings.Contains(got["$filter"][0], "x") || len(got["$select"]) != 1 || got["$select"][0] == "y" || got["$skiptoken"][0] != "1&$filter=x&$select=y" {
		t.Fatalf("injected token changed the request: %v", got)
	}
}

// A nextLink that is not the next page of the list read is not followed.
func TestHistoryRefusesNextLinkOffTheList(t *testing.T) {
	e := newEnv(t, Config{})
	e.holdMail("m1", "a@peer.test", historyEpoch.Add(time.Hour))
	base := e.g.srv.URL + "/v1.0/users/" + testMailbox + "/mailFolders/inbox/messages"
	good := "$filter=receivedDateTime ge 2026-10-01T00:00:00Z&$orderby=receivedDateTime asc&$select=" + messageSelect
	for name, link := range map[string]string{
		"other origin":  "https://evil.test/v1.0/users/" + testMailbox + "/mailFolders/inbox/messages?$skiptoken=1&" + good,
		"sent items":    strings.Replace(base, "inbox", "sentitems", 1) + "?$skiptoken=1&" + good,
		"other mailbox": strings.Replace(base, testMailbox, "victim@contoso.test", 1) + "?$skiptoken=1&" + good,
		"all messages":  e.g.srv.URL + "/v1.0/users/" + testMailbox + "/messages?$skiptoken=1&" + good,
		"other filter":  base + "?$skiptoken=1&$filter=receivedDateTime ge 2000-01-01T00:00:00Z&$orderby=receivedDateTime asc&$select=" + messageSelect,
		"other select":  base + "?$skiptoken=1&$filter=receivedDateTime ge 2026-10-01T00:00:00Z&$orderby=receivedDateTime asc&$select=id",
		"extra param":   base + "?$skiptoken=1&$expand=attachments&" + good,
		"no select":     base + "?$skiptoken=1&$filter=receivedDateTime ge 2026-10-01T00:00:00Z&$orderby=receivedDateTime asc",
		"no token":      base + "?" + good,
		"two tokens":    base + "?$skiptoken=1&$skip=2&" + good,
	} {
		e.g.mu.Lock()
		e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasSuffix(r.URL.Path, "/mailFolders/inbox/messages") {
				return false
			}
			writeJSON(w, http.StatusOK, gMessageList{Value: []gMessage{}, NextLink: link})
			return true
		}
		e.g.mu.Unlock()
		_, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 5})
		if model.CodeOf(err) != model.Permanent {
			t.Errorf("%s: error %v, want permanent", name, err)
		}
	}
	// The same list, paged with $skip as Graph's list pages, is followed.
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/mailFolders/inbox/messages") {
			return false
		}
		next := ""
		if r.URL.Query().Get("$skip") == "" {
			next = base + "?$top=5&$skip=5&" + good
		}
		writeJSON(w, http.StatusOK, gMessageList{Value: []gMessage{}, NextLink: next})
		return true
	}
	e.g.mu.Unlock()
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 5})
	if err != nil || p.Next == "" {
		t.Fatalf("History = %+v, %v", p, err)
	}
	next, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 5, Cursor: p.Next})
	if err != nil || next.Next != "" {
		t.Fatalf("History = %+v, %v", next, err)
	}
	if r := e.g.requests(); !strings.Contains(r[len(r)-1], "$skip=5") {
		t.Fatalf("last request %s", r[len(r)-1])
	}
}

// A page read under a larger Limit than a later call has left is read again
// past its head, never returned over the Limit.
func TestHistoryResumeUnderSmallerLimit(t *testing.T) {
	e := newEnv(t, Config{})
	for i, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		e.holdMail(id, "a@peer.test", historyEpoch.Add(time.Duration(i+1)*time.Hour))
	}
	p1, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 3})
	if err != nil || !slices.Equal(envelopeIDs(p1.Envelopes), []string{"m1", "m2", "m3"}) || p1.Next == "" {
		t.Fatalf("page 1 = %v next %q, %v", envelopeIDs(p1.Envelopes), p1.Next, err)
	}
	p2, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 1, Cursor: p1.Next})
	if err != nil || !slices.Equal(envelopeIDs(p2.Envelopes), []string{"m4"}) || p2.Next == "" || p2.Next == p1.Next {
		t.Fatalf("page 2 = %v next %q, %v", envelopeIDs(p2.Envelopes), p2.Next, err)
	}
	p3, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 5, Cursor: p2.Next})
	if err != nil || !slices.Equal(envelopeIDs(p3.Envelopes), []string{"m5"}) || p3.Next != "" {
		t.Fatalf("page 3 = %v next %q, %v", envelopeIDs(p3.Envelopes), p3.Next, err)
	}
	// A first page cut short is resumed from its own request.
	e2 := newEnv(t, Config{})
	for i, id := range []string{"m1", "m2"} {
		e2.holdMail(id, "a@peer.test", historyEpoch.Add(time.Duration(i+1)*time.Hour))
	}
	ids, _ := e2.readAll(t, adapter.HistoryRequest{Since: historyEpoch, Limit: 1})
	if !slices.Equal(ids, []string{"m1", "m2"}) {
		t.Fatalf("messages %q", ids)
	}
}

func envelopeIDs(envs []model.Envelope) []string {
	out := make([]string, len(envs))
	for i, env := range envs {
		out[i] = env.MessageID
	}
	return out
}

func TestHistoryGapOnUnreadableItem(t *testing.T) {
	e := newEnv(t, Config{})
	e.holdMail("m1", "a@peer.test", historyEpoch.Add(time.Hour))
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/mailFolders/inbox/messages") {
			return false
		}
		writeJSON(w, http.StatusOK, gMessageList{Value: []gMessage{{ID: "", ReceivedDateTime: historyEpoch.Add(time.Hour)}}})
		return true
	}
	e.g.mu.Unlock()
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 10})
	if err != nil || !p.Gap || len(p.Envelopes) != 0 {
		t.Fatalf("History = %+v, %v; want a gap", p, err)
	}
	// A readable page is no gap.
	e.g.mu.Lock()
	e.g.override = nil
	e.g.mu.Unlock()
	p, err = e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 10})
	if err != nil || p.Gap || len(p.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v; want no gap", p, err)
	}
}

func TestHistoryErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header string
		code   model.ErrorCode
		after  time.Duration
	}{
		{"forbidden mailbox", http.StatusForbidden, "", model.Unavailable, 0},
		{"unknown mailbox", http.StatusNotFound, "", model.Unavailable, 0},
		{"throttled", http.StatusTooManyRequests, "7", model.RateLimited, 7 * time.Second},
		{"busy", http.StatusServiceUnavailable, "3", model.Transient, 3 * time.Second},
		{"server error", http.StatusBadGateway, "", model.Transient, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, Config{})
			e.g.mu.Lock()
			e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/mailFolders/inbox/messages") {
					return false
				}
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				graphError(w, tc.status, "fake")
				return true
			}
			e.g.mu.Unlock()
			start := time.Now()
			_, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 5})
			me := wantErr(t, err, tc.code)
			if me.RetryAfter != tc.after {
				t.Fatalf("RetryAfter = %v, want %v", me.RetryAfter, tc.after)
			}
			if n := e.g.count("GET"); n != 1 {
				t.Fatalf("%d calls, want one: a throttled read is not retried", n)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("History waited")
			}
		})
	}
}

// A link Graph no longer accepts is a cursor error, not an unreachable
// mailbox.
func TestHistoryLostLinkIsValidation(t *testing.T) {
	e := newEnv(t, Config{})
	for i, id := range []string{"m1", "m2", "m3"} {
		e.holdMail(id, "a@peer.test", historyEpoch.Add(time.Duration(i+1)*time.Hour))
	}
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 1})
	if err != nil || p.Next == "" {
		t.Fatalf("History = %+v, %v", p, err)
	}
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("$skiptoken") == "" {
			return false
		}
		graphError(w, http.StatusGone, "fake")
		return true
	}
	e.g.mu.Unlock()
	_, err = e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 1, Cursor: p.Next})
	wantCode(t, err, model.Validation)
}

// An import reads and writes no receiver state.
func TestHistoryLeavesReceiverStateAlone(t *testing.T) {
	p := &memProtocol{}
	e := newEnv(t, Config{}, withProtocol(p))
	e.holdMail("m1", "a@peer.test", historyEpoch.Add(time.Hour))
	if _, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if p.saves != 0 || len(p.ext) != 0 {
		t.Fatalf("history wrote receiver state: %d saves", p.saves)
	}
}

// A history read stages the attachments as the receiver does: the same
// requests (key, destination, metadata, bytes) for the same message, and the
// same envelope.
func TestHistoryStagesAsTheReceiverDoes(t *testing.T) {
	e := newEnv(t, Config{})
	deliverFilesAt(e, "m1", historyEpoch.Add(time.Hour), "invoice.pdf", "notes.pdf")
	hist := &stagingSink{}
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 10, Stager: hist})
	if err != nil || len(p.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v", p, err)
	}
	if n := len(stagedRefs(p.Envelopes[0].Content)); n != 2 {
		t.Fatalf("parts %+v, want both attachments staged", p.Envelopes[0].Content.Parts)
	}
	for _, d := range described(t, p.Envelopes[0].Content) {
		if !d.Staged {
			t.Errorf("%s described as not staged", d.Name)
		}
	}
	live := &stagingSink{}
	in, err := fetchWith(t, e, live, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hist.reqs, live.reqs) || !reflect.DeepEqual(hist.bodies, live.bodies) {
		t.Errorf("history staged %+v %q\nreceiver staged %+v %q\nwant the same", hist.reqs, hist.bodies, live.reqs, live.bodies)
	}
	if !reflect.DeepEqual(p.Envelopes[0], in.Envelope) {
		t.Errorf("history envelope differs from the receiver's:\n%+v\n%+v", p.Envelopes[0], in.Envelope)
	}
}

// The receiver's bounds hold for a history read: at most
// maxStagedAttachments files, and none over maxStagedBytes.
func TestHistoryStagingKeepsTheReceiversBounds(t *testing.T) {
	e := newEnv(t, Config{})
	var names []string
	for i := range maxStagedAttachments + 2 {
		names = append(names, fmt.Sprintf("f%02d.bin", i))
	}
	deliverFilesAt(e, "m1", historyEpoch.Add(time.Hour), names...)
	s := &stagingSink{}
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 10, Stager: s})
	if err != nil || len(p.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v", p, err)
	}
	if n := len(stagedRefs(p.Envelopes[0].Content)); n != maxStagedAttachments || len(s.reqs) != maxStagedAttachments {
		t.Errorf("staged %d, requests %d, want %d", n, len(s.reqs), maxStagedAttachments)
	}
}

// A throttled attachment download returns with its Retry-After and the read
// is not retried or waited on; a refused attachment is described only.
func TestHistoryStagingFailures(t *testing.T) {
	e := newEnv(t, Config{})
	deliverFilesAt(e, "m1", historyEpoch.Add(time.Hour), "invoice.pdf")
	e.g.mu.Lock()
	e.g.valueStatus = map[string]int{"att-invoice.pdf": http.StatusTooManyRequests}
	e.g.mu.Unlock()
	start := time.Now()
	_, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 10, Stager: &stagingSink{}})
	if me := wantErr(t, err, model.RateLimited); me.RetryAfter == 0 && me.Retry == model.NoRetry {
		t.Errorf("error %+v carries no retry advice", me)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("History waited")
	}
	e.g.mu.Lock()
	delete(e.g.valueStatus, "att-invoice.pdf")
	e.g.mu.Unlock()
	refuse := &stagingSink{stageErr: func(int) error { return model.NewError(model.Validation, "media type refused") }}
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 10, Stager: refuse})
	if err != nil || len(p.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v", p, err)
	}
	if len(stagedRefs(p.Envelopes[0].Content)) != 0 || described(t, p.Envelopes[0].Content)[0].Staged {
		t.Errorf("refused attachment staged: %+v", p.Envelopes[0].Content.Parts)
	}
}

// Graph's own nextLink names the folder by key, mailFolders('inbox'), where
// the reader asked for mailFolders/inbox: the same list, so it is followed;
// the key form of another folder is still refused.
func TestHistoryFollowsKeyFormNextLink(t *testing.T) {
	e := newEnv(t, Config{})
	e.holdMail("m1", "a@peer.test", historyEpoch.Add(time.Hour))
	good := "$filter=receivedDateTime ge 2026-10-01T00:00:00Z&$orderby=receivedDateTime asc&$select=" + messageSelect
	keyed := func(folder string) string {
		return e.g.srv.URL + "/v1.0/users/" + testMailbox + "/mailFolders('" + folder + "')/messages?$top=5&$skip=5&" + good
	}
	serve := func(link string) {
		e.g.mu.Lock()
		defer e.g.mu.Unlock()
		e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasSuffix(r.URL.Path, "/mailFolders/inbox/messages") {
				return false
			}
			next := ""
			if r.URL.Query().Get("$skip") == "" {
				next = link
			}
			writeJSON(w, http.StatusOK, gMessageList{Value: []gMessage{}, NextLink: next})
			return true
		}
	}
	serve(keyed("inbox"))
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 5})
	if err != nil || p.Next == "" {
		t.Fatalf("History with a key-form nextLink = %+v, %v; want a cursor", p, err)
	}
	serve(keyed("sentitems"))
	if _, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 5}); model.CodeOf(err) != model.Permanent {
		t.Fatalf("key-form nextLink to sentitems: %v, want permanent", err)
	}
}
