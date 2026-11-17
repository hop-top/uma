package google

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// fakeGmail is an httptest server answering the Gmail API v1 calls the
// adapter makes (users.getProfile, users.watch, users.history.list,
// users.messages.list, users.messages.get with format raw and metadata,
// users.messages.send, users.messages.modify) with the request and
// response shapes Google documents, and Google's certs endpoint with a
// test signing key. Every user is a separate account.
type fakeGmail struct {
	t   testing.TB
	srv *httptest.Server
	key *rsa.PrivateKey

	mu       sync.Mutex
	boxes    map[string]*fakeBox
	log      []string // "METHOD path?query"
	tokens   []string
	reject   map[string]bool
	override func(w http.ResponseWriter, r *http.Request) bool
	certHits int
	now      func() time.Time
}

type fakeBox struct {
	user      string
	channel   model.ChannelRef
	historyID int64
	// minHistory: history records at or before it are gone (404).
	minHistory int64
	messages   map[string]*fakeMsg
	order      []string
	history    []fakeHistory
	watches    []gWatchRequest
	seq        int
	responses  []conformance.Response
	calls      []conformance.ProviderCall
	sends      []gSend
	// attempts maps an outbound Message-ID to the effect the harness saw
	// the adapter serve.
	attempts map[string]fakeAttempt
	// executes maps a message ID to the extension effect the harness saw
	// the adapter execute on it: a modify request names only the message.
	executes map[string]fakeExecute
	modifies []gModify
	// drafts are the Drafts of the mailbox (see fakegmail_drafts_test.go).
	drafts   map[string]*fakeDraft
	draftSeq int
	// draftLost makes that many next draft creations apply and then drop
	// the connection before the answer.
	draftLost int
	events    map[string]string // message ID -> conformance event ID
	// stateCalls maps a message ID to the provider state request the
	// harness saw the adapter make for it: Gmail sees the thread and the
	// label lists, not the operation or the conversation's items.
	stateCalls map[string]conformance.ProviderCall
	// threadOps are the thread requests received, "<verb> <thread>".
	threadOps []string
	// userLabels are the user's own labels (see fakegmail_labels_test.go).
	userLabels []gLabel
	labelSeq   int
}

type fakeAttempt struct {
	op    model.OperationRef
	route model.ExternalRef
}

type fakeExecute struct {
	fakeAttempt
	audience model.Audience
	action   model.Extension
}

// systemLabels are the Gmail system labels every mailbox holds; a modify
// naming another label the mailbox does not hold is refused like Gmail
// refuses an unknown label ID (400 invalidArgument).
var systemLabels = []string{labelInbox, labelSent, labelDraft, "UNREAD", "STARRED", "IMPORTANT", "TRASH", "SPAM", "CATEGORY_PROMOTIONS"}

type fakeMsg struct {
	id, threadID string
	labels       []string
	raw          []byte
	internal     time.Time
	msgID        string
	subject      string
	references   string
}

// fakeHistory is one history record: kind "" (messageAdded), "labelAdded",
// "labelRemoved" or "messageDeleted". labels are the message's labels at
// the time (the added ones for messageAdded), changed the labels a label
// record names.
type fakeHistory struct {
	id      int64
	msg     string
	thread  string
	kind    string
	labels  []string
	changed []string
}

func newFakeGmail(t testing.TB) *fakeGmail {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGmail{t: t, key: key, boxes: map[string]*fakeBox{}, reject: map[string]bool{}, now: time.Now}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGmail) endpoint() string { return g.srv.URL + "/gmail/v1" }
func (g *fakeGmail) certsURL() string { return g.srv.URL + "/oauth2/v3/certs" }

// box returns user's mailbox, creating it for channel.
func (g *fakeGmail) box(user string, ch model.ChannelRef) *fakeBox {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.boxes[user]
	if !ok {
		b = &fakeBox{user: user, channel: ch, historyID: 1000, messages: map[string]*fakeMsg{},
			attempts: map[string]fakeAttempt{}, executes: map[string]fakeExecute{}, events: map[string]string{},
			stateCalls: map[string]conformance.ProviderCall{}}
		g.boxes[user] = b
	}
	return b
}

// deliver puts a raw message into user's inbox as a new history record; a
// message ID already present is redelivered as a new record.
func (g *fakeGmail) deliver(user, id, threadID string, raw []byte, labels ...string) {
	b := g.box(user, model.ChannelRef{})
	if len(labels) == 0 {
		labels = []string{labelInbox, "UNREAD"}
	}
	m, err := mime.Parse(bytes.NewReader(raw), mime.Limits{})
	if err != nil {
		g.t.Fatalf("fake deliver: %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := b.messages[id]; !ok {
		b.order = append(b.order, id)
	}
	b.messages[id] = &fakeMsg{id: id, threadID: threadID, labels: labels, raw: raw, internal: g.now(),
		msgID: m.Header.MessageID, subject: m.Header.Subject, references: m.Header.References}
	b.historyID++
	b.history = append(b.history, fakeHistory{id: b.historyID, msg: id, thread: threadID, labels: labels})
}

// hold puts a raw message into user's inbox as one received at at, with no
// history record: mail that arrived before the receiver started. A message
// already held only changes its date.
func (g *fakeGmail) hold(user, id, threadID string, raw []byte, at time.Time, labels ...string) {
	b := g.box(user, model.ChannelRef{})
	if len(labels) == 0 {
		labels = []string{labelInbox, "UNREAD"}
	}
	m, err := mime.Parse(bytes.NewReader(raw), mime.Limits{})
	if err != nil {
		g.t.Fatalf("fake hold: %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if old, ok := b.messages[id]; ok {
		old.internal = at
		return
	}
	b.order = append(b.order, id)
	b.messages[id] = &fakeMsg{id: id, threadID: threadID, labels: labels, raw: raw, internal: at,
		msgID: m.Header.MessageID, subject: m.Header.Subject, references: m.Header.References}
}

// open puts a raw message from the peer into the mailbox outside the inbox
// and without a history record: the message a conversation was opened by,
// which no receiver round lists but modify reaches.
func (g *fakeGmail) open(user, id, threadID string, raw []byte) {
	b := g.box(user, model.ChannelRef{})
	m, err := mime.Parse(bytes.NewReader(raw), mime.Limits{})
	if err != nil {
		g.t.Fatalf("fake open: %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := b.messages[id]; !ok {
		b.order = append(b.order, id)
	}
	b.messages[id] = &fakeMsg{id: id, threadID: threadID, labels: []string{"UNREAD"}, raw: raw, internal: g.now(),
		msgID: m.Header.MessageID, subject: m.Header.Subject, references: m.Header.References}
}

func (g *fakeGmail) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.log...)
}

func (g *fakeGmail) count(prefix string) int {
	n := 0
	for _, r := range g.requests() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

var reUser = regexp.MustCompile(`^/gmail/v1/users/([^/]+)(/.*)$`)

func (g *fakeGmail) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.log = append(g.log, r.Method+" "+r.URL.Path+query(r))
	override := g.override
	g.mu.Unlock()
	if override != nil && override(w, r) {
		return
	}
	if r.URL.Path == "/oauth2/v3/certs" {
		g.certs(w)
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.mu.Lock()
	g.tokens = append(g.tokens, tok)
	rejected := g.reject[tok]
	g.mu.Unlock()
	if !ok || tok == "" || rejected {
		googleError(w, http.StatusUnauthorized, "authError", "UNAUTHENTICATED")
		return
	}
	sm := reUser.FindStringSubmatch(r.URL.Path)
	if sm == nil {
		googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
		return
	}
	user, _ := url.PathUnescape(sm[1])
	g.mu.Lock()
	b, ok := g.boxes[user]
	g.mu.Unlock()
	if !ok {
		googleError(w, http.StatusBadRequest, "failedPrecondition", "FAILED_PRECONDITION")
		return
	}
	g.serveUser(w, r, b, sm[2])
}

func query(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	q, _ := url.QueryUnescape(r.URL.RawQuery)
	return "?" + q
}

// googleError writes the documented Google API error body.
func googleError(w http.ResponseWriter, status int, reason, statusName string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"fake","errors":[{"message":"fake","domain":"global","reason":%q}],"status":%q}}`,
		status, reason, statusName)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (g *fakeGmail) serveUser(w http.ResponseWriter, r *http.Request, b *fakeBox, rest string) {
	switch {
	case r.Method == "GET" && rest == "/profile":
		g.mu.Lock()
		p := map[string]any{"emailAddress": b.user, "messagesTotal": len(b.messages), "threadsTotal": len(b.messages),
			"historyId": strconv.FormatInt(b.historyID, 10)}
		g.mu.Unlock()
		writeJSON(w, http.StatusOK, p)
	case r.Method == "POST" && rest == "/watch":
		var req gWatchRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.TopicName == "" {
			googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
			return
		}
		g.mu.Lock()
		b.watches = append(b.watches, req)
		resp := map[string]string{"historyId": strconv.FormatInt(b.historyID, 10),
			"expiration": strconv.FormatInt(g.now().Add(7*24*time.Hour).UnixMilli(), 10)}
		g.mu.Unlock()
		writeJSON(w, http.StatusOK, resp)
	case r.Method == "GET" && rest == "/history":
		g.history(w, r, b)
	case r.Method == "GET" && rest == "/messages":
		g.list(w, r, b)
	case rest == "/labels":
		g.labelsAPI(w, r, b)
	case r.Method == "POST" && rest == "/messages/send":
		g.send(w, r, b)
	case r.Method == "POST" && strings.HasPrefix(rest, "/messages/") && strings.HasSuffix(rest, "/modify"):
		id, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(rest, "/messages/"), "/modify"))
		g.modify(w, r, b, id)
	case rest == "/drafts" || strings.HasPrefix(rest, "/drafts/"):
		g.draftsAPI(w, r, b, rest)
	case strings.HasPrefix(rest, "/threads/"):
		g.threads(w, r, b, strings.TrimPrefix(rest, "/threads/"))
	case r.Method == "GET" && strings.HasPrefix(rest, "/messages/"):
		id, _ := url.PathUnescape(strings.TrimPrefix(rest, "/messages/"))
		g.get(w, r, b, id)
	default:
		googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
	}
}

// history answers users.history.list: records after startHistoryId of the
// requested historyTypes (messageAdded, labelAdded, labelRemoved,
// messageDeleted) and the labelId filter; a start at or before the oldest
// kept record is 404.
func (g *fakeGmail) history(w http.ResponseWriter, r *http.Request, b *fakeBox) {
	q := r.URL.Query()
	start, err := strconv.ParseInt(q.Get("startHistoryId"), 10, 64)
	if err != nil {
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
		return
	}
	types := q["historyTypes"]
	for _, t := range types {
		if !slices.Contains([]string{"messageAdded", "labelAdded", "labelRemoved", "messageDeleted"}, t) {
			googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
			return
		}
	}
	if len(types) == 0 {
		types = []string{"messageAdded", "labelAdded", "labelRemoved", "messageDeleted"}
	}
	kinds := map[string]string{"": "messageAdded", "labelAdded": "labelAdded", "labelRemoved": "labelRemoved", "messageDeleted": "messageDeleted"}
	size, _ := strconv.Atoi(q.Get("maxResults"))
	if size <= 0 {
		size = 100
	}
	pos, _ := strconv.Atoi(q.Get("pageToken"))
	g.mu.Lock()
	defer g.mu.Unlock()
	if start <= b.minHistory {
		googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
		return
	}
	var recs []fakeHistory
	for _, h := range b.history {
		if h.id > start && slices.Contains(types, kinds[h.kind]) && (q.Get("labelId") == "" || slices.Contains(h.labels, q.Get("labelId"))) {
			recs = append(recs, h)
		}
	}
	page := gHistoryList{HistoryID: int64s(b.historyID)}
	end := min(pos+size, len(recs))
	for _, h := range recs[min(pos, end):end] {
		m := gMessage{ID: h.msg, ThreadID: h.thread, LabelIDs: h.labels}
		rec := gHistory{ID: int64s(h.id)}
		switch h.kind {
		case "":
			rec.MessagesAdded = []gMessageAdded{{Message: m}}
		case "labelAdded":
			rec.LabelsAdded = []gLabelChange{{Message: m, LabelIDs: h.changed}}
		case "labelRemoved":
			rec.LabelsRemoved = []gLabelChange{{Message: m, LabelIDs: h.changed}}
		case "messageDeleted":
			rec.MessagesDeleted = []gMessageAdded{{Message: m}}
		}
		page.History = append(page.History, rec)
	}
	if end < len(recs) {
		page.NextPageToken = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, page)
}

var (
	reAfter  = regexp.MustCompile(`after:(\d+)`)
	reBefore = regexp.MustCompile(`before:(\d+)`)
	reFrom   = regexp.MustCompile(`from:(\S+)`)
)

// list answers users.messages.list with labelIds and the q operators the
// adapter uses (after:<epoch seconds>, rfc822msgid:<id>) and the recorded
// scenario's before:<epoch seconds>, newest first. An
// rfc822msgid: search of SENT is the reconcile call the provider sees.
func (g *fakeGmail) list(w http.ResponseWriter, r *http.Request, b *fakeBox) {
	q := r.URL.Query()
	label, search := q.Get("labelIds"), q.Get("q")
	size, _ := strconv.Atoi(q.Get("maxResults"))
	if size <= 0 {
		size = 100
	}
	pos, _ := strconv.Atoi(q.Get("pageToken"))
	g.mu.Lock()
	defer g.mu.Unlock()
	var after, before time.Time
	if m := reAfter.FindStringSubmatch(search); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		after = time.Unix(n, 0)
	}
	if m := reBefore.FindStringSubmatch(search); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		before = time.Unix(n, 0)
	}
	msgid, isReconcile := strings.CutPrefix(search, "rfc822msgid:")
	var from string
	if m := reFrom.FindStringSubmatch(search); m != nil {
		from = strings.ToLower(m[1])
	}
	var hits []gMessage
	for j := len(b.order) - 1; j >= 0; j-- {
		m := b.messages[b.order[j]]
		switch {
		case label != "" && !slices.Contains(m.labels, label),
			!after.IsZero() && !m.internal.After(after),
			!before.IsZero() && !m.internal.Before(before),
			from != "" && !strings.Contains(strings.ToLower(headerValue(m.raw, "From")), from),
			isReconcile && m.msgID != "<"+msgid+">":
			continue
		}
		hits = append(hits, gMessage{ID: m.id, ThreadID: m.threadID})
	}
	if isReconcile {
		a := b.attempts["<"+msgid+">"]
		call := conformance.ProviderCall{Capability: model.CapabilityReconcile, Operation: a.op, Route: a.route}
		if len(hits) > 0 {
			call.Applied, call.References = true, g.refs(b, b.messages[hits[0].ID])
		}
		b.calls = append(b.calls, call)
	}
	page := gMessageList{Messages: []gMessage{}}
	end := min(pos+size, len(hits))
	page.Messages = append(page.Messages, hits[min(pos, end):end]...)
	if end < len(hits) {
		page.NextPageToken = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": page.Messages, "nextPageToken": page.NextPageToken, "resultSizeEstimate": len(hits)})
}

// get answers users.messages.get with format=raw (padded base64url, as
// Google's encoder writes it) or format=metadata with metadataHeaders.
func (g *fakeGmail) get(w http.ResponseWriter, r *http.Request, b *fakeBox, id string) {
	g.mu.Lock()
	m, ok := b.messages[id]
	g.mu.Unlock()
	if !ok {
		googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
		return
	}
	out := gMessage{ID: m.id, ThreadID: m.threadID, LabelIDs: m.labels, InternalDate: int64s(m.internal.UnixMilli()), HistoryID: 1}
	switch q := r.URL.Query(); q.Get("format") {
	case "raw":
		out.Raw = base64.URLEncoding.EncodeToString(m.raw)
	case "minimal":
		// Gmail dates a minimal message too.
	case "metadata":
		out.SizeEstimate = int64s(len(m.raw))
		out.Payload = &gPart{}
		for _, name := range q["metadataHeaders"] {
			if v := headerValue(m.raw, name); v != "" {
				out.Payload.Headers = append(out.Payload.Headers, gHeader{Name: name, Value: v})
			}
		}
	default:
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// send answers users.messages.send, consuming the next scripted response
// of the mailbox. An applied send stores the message under SENT, keeping
// its Message-ID.
func (g *fakeGmail) send(w http.ResponseWriter, r *http.Request, b *fakeBox) {
	var req gSend
	body, _ := io.ReadAll(r.Body)
	if json.Unmarshal(body, &req) != nil {
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
		return
	}
	raw, err := base64.URLEncoding.DecodeString(req.Raw)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(req.Raw)
	}
	var parsed *mime.Message
	if err == nil {
		parsed, err = mime.Parse(bytes.NewReader(raw), mime.Limits{})
	}
	if err != nil || len(parsed.Header.To) == 0 {
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
		return
	}
	g.mu.Lock()
	b.sends = append(b.sends, req)
	resp := conformance.Response{Outcome: conformance.OutcomeAccepted}
	if len(b.responses) > 0 {
		resp, b.responses = b.responses[0], b.responses[1:]
	}
	a := b.attempts[parsed.Header.MessageID]
	call := conformance.ProviderCall{Capability: model.CapabilitySend, Operation: a.op, Route: a.route,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: parsed.Text}}}}
	if call.Route.ID == "" {
		call.Route = model.ExternalRef{Channel: b.channel, Namespace: IdentityNamespace, Kind: refKindAddress, ID: parsed.Header.To[0].Address}
	}
	var sent *fakeMsg
	if resp.Outcome == conformance.OutcomeAccepted || resp.Outcome == conformance.OutcomeAnswerLost {
		b.seq++
		thread := req.ThreadID
		if thread == "" {
			thread = fmt.Sprintf("thread-s%d", b.seq)
		}
		sent = &fakeMsg{id: fmt.Sprintf("sent-%d", b.seq), threadID: thread, labels: []string{labelSent}, raw: raw,
			internal: g.now(), msgID: parsed.Header.MessageID, subject: parsed.Header.Subject, references: parsed.Header.References}
		b.messages[sent.id] = sent
		b.order = append(b.order, sent.id)
		b.historyID++
		call.Applied, call.References = true, g.refs(b, sent)
	}
	b.calls = append(b.calls, call)
	g.mu.Unlock()
	switch resp.Outcome {
	case conformance.OutcomeAccepted:
		writeJSON(w, http.StatusOK, gMessage{ID: sent.id, ThreadID: sent.threadID, LabelIDs: sent.labels})
	case conformance.OutcomeRejected:
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
	case conformance.OutcomeRateLimited:
		w.Header().Set("Retry-After", strconv.Itoa(int(resp.RetryAfter/time.Second)))
		googleError(w, http.StatusTooManyRequests, "rateLimitExceeded", "RESOURCE_EXHAUSTED")
	case conformance.OutcomeTransient:
		// Rendered by the dial-failing transport; reaching the server
		// means it was not in use.
		googleError(w, http.StatusServiceUnavailable, "backendError", "UNAVAILABLE")
	case conformance.OutcomeAnswerLost, conformance.OutcomeRequestLost:
		panic(http.ErrAbortHandler) // the connection drops: no answer
	}
}

// modify answers users.messages.modify
// (https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages/modify):
// addLabelIds and removeLabelIds, at most 100 each, applied to the message
// and answered with the updated Message; an unknown message is 404 and an
// unknown label 400. It records the call the conformance suite reads, with
// the effect the harness noted for the message.
func (g *fakeGmail) modify(w http.ResponseWriter, r *http.Request, b *fakeBox, id string) {
	var req gModify
	if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.AddLabelIDs) > 100 || len(req.RemoveLabelIDs) > 100 {
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	b.modifies = append(b.modifies, req)
	m, ok := b.messages[id]
	if !ok {
		googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
		return
	}
	x := b.executes[id]
	call := conformance.ProviderCall{Capability: model.CapabilityExtensionExecute, Operation: x.op, Route: x.route, Audience: x.audience}
	if x.action.Name != "" {
		call.Action = &x.action
	}
	for _, l := range slices.Concat(req.AddLabelIDs, req.RemoveLabelIDs) {
		if !slices.Contains(systemLabels, l) && !g.boxHasLabel(b, l) {
			b.calls = append(b.calls, call)
			googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
			return
		}
	}
	g.relabel(b, m, req.AddLabelIDs, req.RemoveLabelIDs)
	call.Applied, call.References = true, []model.ExternalRef{model.ProviderMessageRef(b.channel, m.id)}
	b.calls = append(b.calls, call)
	writeJSON(w, http.StatusOK, gMessage{ID: m.id, ThreadID: m.threadID, LabelIDs: m.labels})
}

// relabel applies a label change to m and adds its history records, one
// per direction and only for labels that changed, as Gmail does; the caller
// holds g.mu.
func (g *fakeGmail) relabel(b *fakeBox, m *fakeMsg, add, remove []string) {
	var added, removed []string
	for _, l := range add {
		if !slices.Contains(m.labels, l) {
			m.labels = append(m.labels, l)
			added = append(added, l)
		}
	}
	for _, l := range remove {
		if slices.Contains(m.labels, l) {
			m.labels = slices.DeleteFunc(m.labels, func(x string) bool { return x == l })
			removed = append(removed, l)
		}
	}
	for _, d := range []struct {
		kind   string
		labels []string
	}{{"labelAdded", added}, {"labelRemoved", removed}} {
		if len(d.labels) == 0 {
			continue
		}
		b.historyID++
		b.history = append(b.history, fakeHistory{id: b.historyID, msg: m.id, thread: m.threadID, kind: d.kind, labels: slices.Clone(m.labels), changed: slices.Clone(d.labels)})
	}
}

// boxHasLabel reports whether any message of b carries label l: a user
// label exists in the fake once a message was delivered with it.
func (g *fakeGmail) boxHasLabel(b *fakeBox, l string) bool {
	for _, m := range b.messages {
		if slices.Contains(m.labels, l) {
			return true
		}
	}
	return false
}

func (g *fakeGmail) refs(b *fakeBox, m *fakeMsg) []model.ExternalRef {
	return []model.ExternalRef{
		model.ProviderMessageRef(b.channel, m.id),
		{Channel: b.channel, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailMessageID, ID: m.msgID},
	}
}

// nextIsTransient consumes the mailbox's next scripted response when it
// is OutcomeTransient, for a send request.
func (g *fakeGmail) nextIsTransient(r *http.Request) bool {
	if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/messages/send") {
		return false
	}
	sm := reUser.FindStringSubmatch(r.URL.Path)
	if sm == nil {
		return false
	}
	user, _ := url.PathUnescape(sm[1])
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.boxes[user]
	if !ok || len(b.responses) == 0 || b.responses[0].Outcome != conformance.OutcomeTransient {
		return false
	}
	b.responses = b.responses[1:]
	b.calls = append(b.calls, conformance.ProviderCall{Capability: model.CapabilitySend})
	return true
}

// client returns an HTTP client for the fake whose connection fails before
// the request is written whenever a send's scripted answer is transient:
// a real refused TCP connect, observed through the client trace.
func (g *fakeGmail) client() *http.Client {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		g.t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()
	refusing := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, closed)
	}}
	base := g.srv.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if g.nextIsTransient(r) {
			return refusing.RoundTrip(r)
		}
		return base.RoundTrip(r)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// certs serves the signing key as a JWK set.
func (g *fakeGmail) certs(w http.ResponseWriter) {
	g.mu.Lock()
	g.certHits++
	g.mu.Unlock()
	pub := g.key.PublicKey
	w.Header().Set("Cache-Control", "public, max-age=3600, must-revalidate, no-transform")
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test-key",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// rawMessage renders an inbound RFC 5322 message.
func rawMessage(from, to, msgID, subject, text string, extra ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: Sun, 11 Oct 2026 12:00:00 +0000\r\nMessage-ID: %s\r\n", from, to, subject, msgID)
	for _, h := range extra {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + text + "\r\n")
	return []byte(b.String())
}

// headerValue is the unfolded value of the first header field name in raw,
// as users.messages.get answers it for metadataHeaders.
func headerValue(raw []byte, name string) string {
	head, _, _ := bytes.Cut(raw, []byte("\r\n\r\n"))
	var cur string
	var fields []string
	for _, l := range strings.Split(string(head), "\r\n") {
		if l != "" && (l[0] == ' ' || l[0] == '\t') {
			cur += " " + strings.TrimSpace(l)
			continue
		}
		if cur != "" {
			fields = append(fields, cur)
		}
		cur = l
	}
	if cur != "" {
		fields = append(fields, cur)
	}
	for _, f := range fields {
		if k, v, ok := strings.Cut(f, ":"); ok && strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
