package imap_test

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
)

var histBase = time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)

// histInstance opens an instance on srv for history reads.
func histInstance(t *testing.T, srv *mailServer, tweak func(*imap.Config), deps adapter.Dependencies) *imap.Instance {
	t.Helper()
	cfg := config(srv, nil)
	if tweak != nil {
		tweak(&cfg)
	}
	deps.Credentials = passwordCreds()
	return open(t, newFactory(t), channel(channelA, cfg), deps)
}

func history(t *testing.T, inst *imap.Instance, req adapter.HistoryRequest) adapter.HistoryPage {
	t.Helper()
	if req.Limit == 0 {
		req.Limit = 100
	}
	p, err := inst.History(t.Context(), req)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	return p
}

func at(h, m int) time.Time {
	return histBase.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

func TestHistoryDeclaresCapability(t *testing.T) {
	d := imap.NewFactory(imap.Options{}).Describe()
	if !slices.Contains(d.Capabilities, model.CapabilityHistory) {
		t.Fatalf("capabilities %v lack history", d.Capabilities)
	}
	var _ adapter.HistoryReader = (*imap.Instance)(nil)
}

// The window is half-open on INTERNALDATE, judged to the second, though
// SEARCH SINCE/BEFORE only compare dates: mail on the boundary days outside
// the window is fetched and refused by the refinement.
func TestHistoryWindowBounds(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	for id, when := range map[string]time.Time{
		"<before-day@example.org>": at(-30, 0), "<early@example.org>": at(11, 30), "<start@example.org>": at(12, 0),
		"<inside@example.org>": at(13, 59), "<end@example.org>": at(14, 0), "<late@example.org>": at(14, 30),
		"<after-day@example.org>": at(40, 0),
	} {
		srv.deliver("INBOX", mail(id, id), when)
	}
	inst := histInstance(t, srv, nil, adapter.Dependencies{})
	p := history(t, inst, adapter.HistoryRequest{Since: at(12, 0), Until: at(14, 0)})
	got := messageIDs(p.Envelopes)
	slices.Sort(got)
	if want := []string{"<inside@example.org>", "<start@example.org>"}; !slices.Equal(got, want) || p.Next != "" || p.Gap {
		t.Fatalf("window = %v next %q gap %v, want %v: Since inclusive, Until exclusive", got, p.Next, p.Gap, want)
	}
	// Open upper bound.
	p = history(t, inst, adapter.HistoryRequest{Since: at(12, 0)})
	if len(p.Envelopes) != 5 {
		t.Fatalf("open window = %v, want 5 messages from Since on", messageIDs(p.Envelopes))
	}
}

// Delivery is in UID order (arrival), not date order, across pages; Next
// resumes and the last page ends the window.
func TestHistoryPagesInUIDOrder(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	ids := []string{"<p1@example.org>", "<p2@example.org>", "<p3@example.org>", "<p4@example.org>", "<p5@example.org>"}
	for n, id := range ids {
		srv.deliver("INBOX", mail(id, id), at(20-n, 0)) // dates descend as UIDs ascend
	}
	inst := histInstance(t, srv, nil, adapter.Dependencies{})
	req := adapter.HistoryRequest{Since: at(0, 0), Limit: 2}
	var got []string
	var sizes []int
	for range 10 {
		p := history(t, inst, req)
		got = append(got, messageIDs(p.Envelopes)...)
		sizes = append(sizes, len(p.Envelopes))
		if p.Next == "" {
			break
		}
		if p.Next == req.Cursor {
			t.Fatal("cursor did not advance")
		}
		req.Cursor = p.Next
	}
	if !slices.Equal(got, ids) || !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Fatalf("pages %v of %v, want 2,2,1 in UID order %v", sizes, got, ids)
	}
}

func TestHistoryCursorFormat(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	for n := range 3 {
		srv.deliver("INBOX", mail("<c"+string(rune('1'+n))+"@example.org>", "x"), at(10+n, 0))
	}
	inst := histInstance(t, srv, nil, adapter.Dependencies{})
	p := history(t, inst, adapter.HistoryRequest{Since: at(0, 0), Limit: 2})
	b, err := base64.RawURLEncoding.DecodeString(p.Next)
	if err != nil {
		t.Fatalf("cursor %q is not base64url: %v", p.Next, err)
	}
	var c struct {
		V           int    `json:"v"`
		K           string `json:"k"`
		Mailbox     string `json:"mailbox"`
		UIDValidity uint32 `json:"uid_validity"`
		LastUID     uint32 `json:"last_uid"`
		LastDate    int64  `json:"last_date"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.V != 1 || c.K == "" || c.Mailbox != "INBOX" || c.UIDValidity != srv.uidValidity("INBOX") || c.LastUID != 2 || c.LastDate != at(11, 0).UnixMicro() {
		t.Fatalf("cursor %s", b)
	}
}

func TestHistoryForeignCursorIsValidation(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	for n := range 3 {
		srv.deliver("INBOX", mail("<f"+string(rune('1'+n))+"@example.org>", "x"), at(10+n, 0))
	}
	inst := histInstance(t, srv, nil, adapter.Dependencies{})
	p := history(t, inst, adapter.HistoryRequest{Since: at(0, 0), Limit: 1})
	r := route(channelA, peer)
	for name, req := range map[string]adapter.HistoryRequest{
		"other since": {Since: at(1, 0), Cursor: p.Next},
		"other until": {Since: at(0, 0), Until: at(23, 0), Cursor: p.Next},
		"other route": {Since: at(0, 0), Route: &r, Cursor: p.Next},
		"garbage":     {Since: at(0, 0), Cursor: "not-a-cursor"},
		"not json":    {Since: at(0, 0), Cursor: base64.RawURLEncoding.EncodeToString([]byte("zzz"))},
	} {
		req.Limit = 10
		if _, err := inst.History(t.Context(), req); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err %v, want validation", name, err)
		}
	}
	// A cursor for another mailbox is foreign too.
	other := histInstance(t, srv, func(c *imap.Config) { c.Mailbox = "Projects" }, adapter.Dependencies{})
	if _, err := other.History(t.Context(), adapter.HistoryRequest{Since: at(0, 0), Cursor: p.Next, Limit: 10}); model.CodeOf(err) != model.Validation {
		t.Errorf("other mailbox: err %v, want validation", err)
	}
}

// UIDVALIDITY changed between two calls: Gap, and the window restarts at the
// cursor's last_date under the new value; the cursor then carries it.
func TestHistoryUIDValidityChangeGaps(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	for n := range 4 {
		srv.deliver("INBOX", mail("<v"+string(rune('1'+n))+"@example.org>", "x"), at(10+n, 0))
	}
	inst := histInstance(t, srv, nil, adapter.Dependencies{})
	p := history(t, inst, adapter.HistoryRequest{Since: at(0, 0), Limit: 2})
	if p.Gap || len(p.Envelopes) != 2 {
		t.Fatalf("first page gap %v, %d envelopes", p.Gap, len(p.Envelopes))
	}
	old := srv.uidValidity("INBOX")
	srv.recreate("INBOX")
	if srv.uidValidity("INBOX") == old {
		t.Skip("server reuses UIDVALIDITY")
	}
	for n := range 4 { // the same mail again, new UIDs
		srv.deliver("INBOX", mail("<v"+string(rune('1'+n))+"@example.org>", "x"), at(10+n, 0))
	}
	req := adapter.HistoryRequest{Since: at(0, 0), Limit: 10, Cursor: p.Next}
	p2 := history(t, inst, req)
	if !p2.Gap {
		t.Error("no gap across a UIDVALIDITY change")
	}
	// last_date is the second message's date, 11:00: restart there.
	if got, want := messageIDs(p2.Envelopes), []string{"<v2@example.org>", "<v3@example.org>", "<v4@example.org>"}; !slices.Equal(got, want) {
		t.Fatalf("restarted window %v, want %v", got, want)
	}
	if p2.Next != "" {
		t.Errorf("next %q, want the window done", p2.Next)
	}
}

func TestHistoryUndeliverableIsGapNeverStored(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", mail("<ok1@example.org>", "x"), at(10, 0))
	srv.deliver("INBOX", []byte(crlf("To: "+mailUser+"\nMessage-ID: <nofrom@example.org>\n\nno sender\n")), at(11, 0))
	srv.deliver("INBOX", mail("<ok2@example.org>", "x"), at(12, 0))
	inst := histInstance(t, srv, nil, adapter.Dependencies{})
	p := history(t, inst, adapter.HistoryRequest{Since: at(0, 0)})
	if got := messageIDs(p.Envelopes); !slices.Equal(got, []string{"<ok1@example.org>", "<ok2@example.org>"}) || !p.Gap {
		t.Fatalf("envelopes %v gap %v, want the two deliverable and a gap", got, p.Gap)
	}
}

func TestHistoryInaccessibleMailboxIsUnavailable(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	inst := histInstance(t, srv, func(c *imap.Config) { c.Mailbox = "Missing" }, adapter.Dependencies{})
	_, err := inst.History(t.Context(), adapter.HistoryRequest{Since: at(0, 0), Limit: 10})
	if model.CodeOf(err) != model.Unavailable || !strings.Contains(err.Error(), "mailbox cannot be examined") {
		t.Fatalf("err %v, want unavailable: mailbox cannot be examined", err)
	}
}

func TestHistoryRouteFiltersBySender(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", mailFrom(peer, "<a1@example.org>", "x"), at(10, 0))
	srv.deliver("INBOX", mailFrom("bob@example.org", "<b1@example.org>", "x"), at(11, 0))
	// SEARCH FROM matches substrings: this sender is not alice.
	srv.deliver("INBOX", mailFrom("m"+peer, "<m1@example.org>", "x"), at(12, 0))
	srv.deliver("INBOX", mailFrom(peer, "<a2@example.org>", "x"), at(13, 0))
	inst := histInstance(t, srv, nil, adapter.Dependencies{})
	r := route(channelA, peer)
	p := history(t, inst, adapter.HistoryRequest{Since: at(0, 0), Route: &r})
	if got := messageIDs(p.Envelopes); !slices.Equal(got, []string{"<a1@example.org>", "<a2@example.org>"}) {
		t.Fatalf("conversation window %v, want alice's two", got)
	}
	bad := model.ExternalRef{Channel: channelA, Namespace: "other", Kind: imap.RouteKind, ID: peer}
	if _, err := inst.History(t.Context(), adapter.HistoryRequest{Since: at(0, 0), Route: &bad, Limit: 5}); model.CodeOf(err) != model.Validation {
		t.Errorf("foreign route: %v, want validation", err)
	}
}

// Only Config.Mailbox is read: nothing in Sent is imported and Sent is never
// selected.
func TestHistoryNeverReadsSent(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.create("Sent")
	srv.deliver("INBOX", mail("<in@example.org>", "x"), at(10, 0))
	srv.deliver("Sent", mail("<out@example.org>", "x"), at(11, 0))
	inst := histInstance(t, srv, func(c *imap.Config) { c.Sent = imap.Sent{Mailbox: "Sent"} }, adapter.Dependencies{})
	p := history(t, inst, adapter.HistoryRequest{Since: at(0, 0)})
	if got := messageIDs(p.Envelopes); !slices.Equal(got, []string{"<in@example.org>"}) {
		t.Fatalf("history %v, want the inbox mail only", got)
	}
	for _, l := range srv.clientLines() {
		if strings.Contains(l, "Sent") {
			t.Errorf("client line %q touches Sent", l)
		}
	}
}

// A history read is the receiver's own envelope: same message ID, destination,
// event ID and payload; and it never touches the receiver's state or flags.
func TestHistoryEnvelopeEqualsInbound(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", mail("<same@example.org>", "body"), time.Now().Add(-5*time.Minute))
	ps := &protocolState{}
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds(), Protocol: ps})
	s := newSink()
	r := run(t, inst, s)
	live := s.await(t, 1)
	w.idle(t)
	if err := r.stop(); err != nil && model.CodeOf(err) != "" {
		t.Logf("receiver stopped: %v", err)
	}
	before := map[string]model.Extension{}
	ps.mu.Lock()
	for k, v := range ps.data {
		before[k] = v
	}
	ps.mu.Unlock()
	p := history(t, inst, adapter.HistoryRequest{Since: time.Now().Add(-time.Hour)})
	if len(p.Envelopes) != 1 || !reflect.DeepEqual(p.Envelopes[0], live[0]) {
		t.Fatalf("history %+v\nlive    %+v\nwant the same envelope", p.Envelopes, live)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	// The read records what it imported under its own key, for the receiver
	// to follow; the receiver's cursor and ledger are untouched.
	after := map[string]model.Extension{}
	for k, v := range ps.data {
		if k != "mailstate.import" {
			after[k] = v
		}
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("receiver state changed by a history read: %v -> %v", before, after)
	}
	if _, ok := ps.data["mailstate.import"]; !ok {
		t.Error("the read recorded nothing for the receiver to follow")
	}
	if n := srv.seen("INBOX"); n != 0 {
		t.Errorf("%d messages marked \\Seen, want 0", n)
	}
}

func importHistory(r *engineRig, since time.Time) (uma.ImportReport, error) {
	ch := channelA
	return r.engine.Runtime().Import(r.ctx, runtimeCall, uma.ImportRequest{Requester: "operator", Channel: &ch, Since: since})
}

func TestEngineImportThenReceiveStoresOnce(t *testing.T) {
	r := newEngineRig(t, passwordHost(), nil)
	r.srv.deliver("INBOX", mail("<ir1@example.org>", "once"), time.Now().Add(-10*time.Minute))
	rep, err := importHistory(r, time.Now().Add(-time.Hour))
	if err != nil || rep.Imported != 1 || !rep.Complete {
		t.Fatalf("import %+v, %v", rep, err)
	}
	r.receive()
	r.waiter.drive(t, func() bool { return r.srv.commands("IDLE") >= 1 || len(r.waiter.requested()) > 0 })
	r.waiter.idle(t)
	if msgs := r.messages(); len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1: the live delivery is the import's duplicate", len(msgs))
	}
}

func TestEngineReceiveThenImportStoresOnce(t *testing.T) {
	r := newEngineRig(t, passwordHost(), nil)
	r.srv.deliver("INBOX", mail("<ri1@example.org>", "once"), time.Now().Add(-10*time.Minute))
	r.receive()
	r.awaitMessages(1)
	rep, err := importHistory(r, time.Now().Add(-time.Hour))
	if err != nil || rep.Imported != 0 {
		t.Fatalf("import %+v, %v, want nothing imported", rep, err)
	}
	if msgs := r.messages(); len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
}

// A mail without a Message-ID is imported: the event ID identifies it.
func TestEngineImportsMailWithoutMessageID(t *testing.T) {
	r := newEngineRig(t, passwordHost(), nil)
	r.srv.deliver("INBOX", []byte(crlf("From: "+peer+"\nTo: "+mailUser+"\nSubject: hi\n\nno id here\n")), time.Now().Add(-10*time.Minute))
	rep, err := importHistory(r, time.Now().Add(-time.Hour))
	if err != nil || rep.Imported != 1 || rep.Skipped != 0 {
		t.Fatalf("import %+v, %v, want one imported", rep, err)
	}
	if msgs := r.messages(); len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
}

// forge re-encodes the cursor of a real page with edit applied, keeping its
// scope hash: what a caller who computes the hash can build.
func forge(t *testing.T, cursor string, edit func(m map[string]any)) string {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	b, _ = json.Marshal(m)
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestHistoryForgedCursorMailboxIsValidationAndNeverSelected(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.create("Sent")
	for n := range 3 {
		srv.deliver("INBOX", mail("<g"+string(rune('1'+n))+"@example.org>", "x"), at(10+n, 0))
	}
	srv.deliver("Sent", mail("<secret@example.org>", "x"), at(11, 0))
	inst := histInstance(t, srv, func(c *imap.Config) { c.Sent = imap.Sent{Mailbox: "Sent"} }, adapter.Dependencies{})
	p := history(t, inst, adapter.HistoryRequest{Since: at(0, 0), Limit: 1})
	bad := forge(t, p.Next, func(m map[string]any) { m["mailbox"] = "Sent" })
	_, err := inst.History(t.Context(), adapter.HistoryRequest{Since: at(0, 0), Limit: 10, Cursor: bad})
	if model.CodeOf(err) != model.Validation {
		t.Fatalf("err %v, want validation", err)
	}
	for _, l := range srv.clientLines() {
		if strings.Contains(l, "Sent") {
			t.Errorf("client line %q touches Sent", l)
		}
	}
}

func TestHistoryForgedCursorDateOutsideWindowIsValidation(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	for n := range 3 {
		srv.deliver("INBOX", mail("<d"+string(rune('1'+n))+"@example.org>", "x"), at(10+n, 0))
	}
	inst := histInstance(t, srv, nil, adapter.Dependencies{})
	req := adapter.HistoryRequest{Since: at(9, 0), Until: at(20, 0), Limit: 1}
	p := history(t, inst, req)
	for name, when := range map[string]time.Time{"before since": at(1, 0), "at until": at(20, 0), "after until": at(30, 0)} {
		req.Cursor = forge(t, p.Next, func(m map[string]any) { m["last_date"] = when.UnixMicro() })
		if _, err := inst.History(t.Context(), req); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err %v, want validation", name, err)
		}
	}
}
