package microsoft

import (
	"encoding/json"
	"fmt"
	"io"
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

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// fakeGraph is an httptest server answering the Microsoft Graph v1.0 mail
// and subscription calls the adapter makes, with the request and response
// shapes Microsoft Learn documents for message, message delta,
// createReply, update (drafts, and categories of any message), send,
// extended-property $filter and subscription. Every mailbox is a separate
// account.
type fakeGraph struct {
	t   testing.TB
	srv *httptest.Server

	mu        sync.Mutex
	mailboxes map[string]*fakeMailbox
	subs      map[string]*gSubscription
	subSeq    int
	log       []string // "METHOD path?query"
	plain     []string // mailbox requests sent without Prefer: IdType="ImmutableId"
	tokens    []string // bearer tokens seen
	reject    map[string]bool
	override  func(w http.ResponseWriter, r *http.Request) bool
	// files are the bytes of file attachments by attachment ID, answered
	// by .../attachments/{id}/$value; valueStatus fails that call with a
	// status for an attachment ID.
	files       map[string][]byte
	valueStatus map[string]int
	// mimes are the MIME forms of messages by message ID, answered by
	// .../messages/{id}/$value
	// (https://learn.microsoft.com/en-us/graph/outlook-get-mime-message).
	mimes map[string][]byte
	// attPage pages the attachment list: that many attachments per page,
	// the position carried in $skiptoken and the next page named in
	// @odata.nextLink. Zero answers the whole list at once. attEndless
	// names a next page after every page, and attNextLink replaces the
	// link of the first page.
	attPage     int
	attEndless  bool
	attNextLink string
}

type fakeMailbox struct {
	name      string
	channel   model.ChannelRef
	messages  map[string]*fakeMessage
	order     []string // message IDs by creation
	changes   []string // inbox delta change log
	seq       int
	responses []conformance.Response
	calls     []conformance.ProviderCall
	events    map[string]string // message ID -> conformance event ID
	// executes maps a message ID to the extension effect the harness saw
	// the adapter execute on it: an update request names only the message.
	executes map[string]fakeExecute
	updates  []gCategories
	// flog is the state change log by folder (see fakegraph_state_test.go),
	// notes the state request the harness saw the adapter make per message,
	// folders the custom folders the mailbox holds.
	flog    map[string][]fchange
	notes   map[string]stateNote
	folders map[string]bool
	// tree is the folder tree and cats the master categories (see
	// fakegraph_folders_test.go).
	tree     []*fakeFolder
	cats     []gCategory
	fseq     int
	pageSize int
}

type fakeExecute struct {
	op       model.OperationRef
	route    model.ExternalRef
	audience model.Audience
	action   model.Extension
}

type fakeMessage struct {
	m      gMessage
	folder string // inbox, drafts, sentitems
	parent string // createReply source
	// bcc are the draft's blind recipients: Graph does not return them on a
	// read the adapter makes.
	bcc   []gRecipient
	props map[string]string
	atts  []gAttachment
	// modified is lastModifiedDateTime.
	modified time.Time
}

// attachmentList answers GET .../attachments: the whole list, or a page of
// it when attPage is set.
func (g *fakeGraph) attachmentList(w http.ResponseWriter, r *http.Request, fm *fakeMessage) {
	g.mu.Lock()
	size, endless, link := g.attPage, g.attEndless, g.attNextLink
	g.mu.Unlock()
	page := struct {
		Value    []gAttachment `json:"value"`
		NextLink string        `json:"@odata.nextLink,omitempty"`
	}{Value: fm.atts}
	if size > 0 {
		pos, _ := strconv.Atoi(r.URL.Query().Get("$skiptoken"))
		pos = min(pos, len(fm.atts))
		end := min(pos+size, len(fm.atts))
		page.Value = fm.atts[pos:end]
		if end < len(fm.atts) || endless {
			q := r.URL.Query()
			q.Set("$skiptoken", strconv.Itoa(end))
			page.NextLink = g.srv.URL + r.URL.Path + "?" + encodeQuery(q)
		}
	}
	if link != "" && r.URL.Query().Get("$skiptoken") == "" {
		page.NextLink = link
	}
	writeJSON(w, http.StatusOK, page)
}

// attachmentValue answers GET .../attachments/{id}/$value: the raw bytes of
// a file attachment, HTTP 405 for a reference attachment, as Microsoft
// Learn documents (Get attachment).
func (g *fakeGraph) attachmentValue(w http.ResponseWriter, fm *fakeMessage, id string) {
	g.mu.Lock()
	status, failing := g.valueStatus[id]
	data, ok := g.files[id]
	g.mu.Unlock()
	switch {
	case failing:
		graphError(w, status, "fake")
	case !slices.ContainsFunc(fm.atts, func(a gAttachment) bool { return a.ID == id }) || !ok:
		graphError(w, http.StatusNotFound, "ErrorItemNotFound")
	case slices.ContainsFunc(fm.atts, func(a gAttachment) bool {
		return a.ID == id && a.ODataType == "#microsoft.graph.referenceAttachment"
	}):
		graphError(w, http.StatusMethodNotAllowed, "ErrorInvalidRequest")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}
}

func newFakeGraph(t testing.TB) *fakeGraph {
	g := &fakeGraph{t: t, mailboxes: map[string]*fakeMailbox{}, subs: map[string]*gSubscription{}, reject: map[string]bool{},
		files: map[string][]byte{}, valueStatus: map[string]int{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGraph) endpoint() string { return g.srv.URL + "/v1.0" }

// mailbox returns the mailbox name, creating it for channel.
func (g *fakeGraph) mailbox(name string, ch model.ChannelRef) *fakeMailbox {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mailboxLocked(name, ch)
}

func (g *fakeGraph) mailboxLocked(name string, ch model.ChannelRef) *fakeMailbox {
	mb, ok := g.mailboxes[name]
	if !ok {
		mb = &fakeMailbox{name: name, channel: ch, messages: map[string]*fakeMessage{}, events: map[string]string{}, executes: map[string]fakeExecute{},
			flog: map[string][]fchange{}, notes: map[string]stateNote{}, folders: map[string]bool{}}
		g.mailboxes[name] = mb
	}
	return mb
}

// deliver puts a message into the inbox and the delta change log; a
// message ID already present is redelivered as a new change.
func (g *fakeGraph) deliver(mailbox string, m gMessage, atts ...gAttachment) {
	g.mu.Lock()
	defer g.mu.Unlock()
	mb := g.mailboxLocked(mailbox, model.ChannelRef{})
	if _, ok := mb.messages[m.ID]; !ok {
		mb.order = append(mb.order, m.ID)
	}
	m.HasAttachments = len(atts) > 0
	mb.messages[m.ID] = &fakeMessage{m: m, folder: "inbox", atts: atts, modified: fakeStamp()}
	mb.changes = append(mb.changes, m.ID)
	mb.flog["inbox"] = append(mb.flog["inbox"], fchange{id: m.ID})
}

// open puts a message from the peer into the inbox without a delta change:
// the message a conversation was opened by, which no receiver round lists.
func (g *fakeGraph) open(mailbox string, m gMessage) {
	g.mu.Lock()
	defer g.mu.Unlock()
	mb := g.mailboxLocked(mailbox, model.ChannelRef{})
	if _, ok := mb.messages[m.ID]; !ok {
		mb.order = append(mb.order, m.ID)
	}
	mb.messages[m.ID] = &fakeMessage{m: m, folder: "inbox", modified: fakeStamp()}
}

func (g *fakeGraph) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.log...)
}

func (g *fakeGraph) count(prefix string) int {
	n := 0
	for _, r := range g.requests() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

var (
	reMailbox = regexp.MustCompile(`^/v1\.0/users/([^/]+)(/.*)$`)
	reValue   = regexp.MustCompile(`ep/value eq '((?:[^']|'')*)'`)
)

func (g *fakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.log = append(g.log, r.Method+" "+r.URL.Path+query(r))
	if !slices.Contains(r.Header.Values("Prefer"), preferImmutableID) && strings.HasPrefix(r.URL.Path, "/v1.0/users/") {
		g.plain = append(g.plain, r.Method+" "+r.URL.Path)
	}
	override := g.override
	g.mu.Unlock()
	if override != nil && override(w, r) {
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.mu.Lock()
	g.tokens = append(g.tokens, tok)
	rejected := g.reject[tok]
	g.mu.Unlock()
	if !ok || tok == "" || rejected {
		graphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1.0/subscriptions") {
		g.serveSubscriptions(w, r)
		return
	}
	sm := reMailbox.FindStringSubmatch(r.URL.Path)
	if sm == nil {
		graphError(w, http.StatusNotFound, "ResourceNotFound")
		return
	}
	name, _ := url.PathUnescape(sm[1])
	g.mu.Lock()
	mb, ok := g.mailboxes[name]
	g.mu.Unlock()
	if !ok {
		graphError(w, http.StatusNotFound, "ErrorInvalidUser")
		return
	}
	g.serveMailbox(w, r, mb, sm[2])
}

func query(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	q, _ := url.QueryUnescape(r.URL.RawQuery)
	return "?" + q
}

func graphError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"fake","innerError":{"request-id":"00000000-0000-0000-0000-000000000000"}}}`, code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; odata.metadata=minimal")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (g *fakeGraph) serveMailbox(w http.ResponseWriter, r *http.Request, mb *fakeMailbox, rest string) {
	switch {
	case rest == "/mailFolders" || strings.HasSuffix(rest, "/childFolders") || rest == "/outlook/masterCategories":
		g.mailboxObjects(w, r, mb, rest)
	case r.Method == "GET" && strings.HasPrefix(rest, "/mailFolders/") && strings.HasSuffix(rest, "/messages/delta"):
		folder, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(rest, "/mailFolders/"), "/messages/delta"))
		g.folderDelta(w, r, mb, folder)
	case r.Method == "GET" && strings.HasPrefix(rest, "/mailFolders/") && !strings.Contains(strings.TrimPrefix(rest, "/mailFolders/"), "/"):
		name, _ := url.PathUnescape(strings.TrimPrefix(rest, "/mailFolders/"))
		g.mailFolder(w, mb, name)
	case r.Method == "GET" && strings.HasPrefix(rest, "/mailFolders/") && strings.HasSuffix(rest, "/messages") && rest != "/mailFolders/sentitems/messages":
		folder, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(rest, "/mailFolders/"), "/messages"))
		g.listInbox(w, r, mb, canonFolder(folder))
	case r.Method == "GET" && (rest == "/messages" || rest == "/mailFolders/sentitems/messages"):
		g.filterByAttempt(w, r, mb, rest != "/messages")
	case r.Method == "POST" && rest == "/messages":
		g.createDraft(w, r, mb, "", false)
	default:
		id, action, _ := strings.Cut(strings.TrimPrefix(rest, "/messages/"), "/")
		id, _ = url.PathUnescape(id)
		g.mu.Lock()
		fm, ok := mb.messages[id]
		g.mu.Unlock()
		if !ok || !strings.HasPrefix(rest, "/messages/") {
			graphError(w, http.StatusNotFound, "ErrorItemNotFound")
			return
		}
		switch {
		case r.Method == "GET" && action == "":
			g.mu.Lock()
			m := fm.view()
			if strings.Contains(r.URL.Query().Get("$expand"), "singleValueExtendedProperties") {
				for id, v := range fm.props {
					m.SingleValueExtendedProperties = append(m.SingleValueExtendedProperties, gExtendedProperty{ID: id, Value: v})
				}
			}
			g.mu.Unlock()
			writeJSON(w, http.StatusOK, m)
		case r.Method == "GET" && action == "$value":
			g.mu.Lock()
			raw, ok := g.mimes[id]
			g.mu.Unlock()
			if !ok {
				graphError(w, http.StatusNotFound, "ErrorItemNotFound")
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(raw)
		case r.Method == "POST" && action == "move":
			g.move(w, r, mb, fm)
		case r.Method == "GET" && action == "attachments":
			g.attachmentList(w, r, fm)
		case r.Method == "GET" && strings.HasPrefix(action, "attachments/") && strings.HasSuffix(action, "/$value"):
			attID, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(action, "attachments/"), "/$value"))
			g.attachmentValue(w, fm, attID)
		case r.Method == "POST" && action == "createReply":
			g.createDraft(w, r, mb, id, false)
		case r.Method == "POST" && action == "createReplyAll":
			g.createDraft(w, r, mb, id, true)
		case r.Method == "PATCH" && action == "":
			g.update(w, r, mb, fm)
		case r.Method == "POST" && action == "send":
			g.send(w, mb, fm)
		case r.Method == "DELETE" && action == "":
			g.mu.Lock()
			delete(mb.messages, id)
			g.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			graphError(w, http.StatusBadRequest, "BadRequest")
		}
	}
}

// delta pages the inbox change log: no token starts at the beginning,
// $skiptoken and $deltatoken carry the position.
func (g *fakeGraph) delta(w http.ResponseWriter, r *http.Request, mb *fakeMailbox) {
	size := 50
	if p := r.Header.Values("Prefer"); len(p) > 0 {
		for _, v := range p {
			if n, ok := strings.CutPrefix(v, "odata.maxpagesize="); ok {
				size, _ = strconv.Atoi(n)
			}
		}
	}
	pos := 0
	q := r.URL.Query()
	for _, k := range []string{"$skiptoken", "$deltatoken"} {
		if v := q.Get(k); v != "" {
			if v == "expired" {
				graphError(w, http.StatusGone, "SyncStateNotFound")
				return
			}
			pos, _ = strconv.Atoi(v)
		}
	}
	g.mu.Lock()
	end := min(pos+size, len(mb.changes))
	page := gDeltaPage{Value: []gDeltaItem{}}
	for _, id := range mb.changes[pos:end] {
		page.Value = append(page.Value, gDeltaItem{ID: id})
	}
	base := g.srv.URL + r.URL.Path
	if end < len(mb.changes) {
		page.NextLink = base + "?$skiptoken=" + strconv.Itoa(end)
	} else {
		page.DeltaLink = base + "?$deltatoken=" + strconv.Itoa(end)
	}
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, page)
}

func (g *fakeGraph) createDraft(w http.ResponseWriter, r *http.Request, mb *fakeMailbox, parent string, all bool) {
	var d gDraft
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		if err := json.Unmarshal(b, &d); err != nil {
			graphError(w, http.StatusBadRequest, "RequestBodyRead")
			return
		}
	}
	g.mu.Lock()
	mb.seq++
	id := fmt.Sprintf("draft-%d", mb.seq)
	fm := &fakeMessage{folder: "drafts", parent: parent, props: map[string]string{}, bcc: d.BccRecipients, m: gMessage{
		ID: id, InternetMessageID: fmt.Sprintf("<%s.%s@fake.outlook.test>", id, strings.ReplaceAll(mb.name, "@", ".")), IsDraft: true,
		Subject: d.Subject, Body: &d.Body, ToRecipients: d.ToRecipients, CcRecipients: d.CcRecipients,
	}}
	if p, ok := mb.messages[parent]; ok {
		fm.m.Subject = "RE: " + p.m.Subject
		fm.m.ConversationID = p.m.ConversationID
		if p.m.From != nil {
			fm.m.ToRecipients = []gRecipient{*p.m.From}
		}
		if all {
			// Stand-in for a reply to all: the sender, then the other To
			// recipients and the Cc recipients, the mailbox's own address
			// among them: the adapter filters it.
			fm.m.ToRecipients = append(fm.m.ToRecipients, p.m.ToRecipients...)
			fm.m.CcRecipients = append([]gRecipient(nil), p.m.CcRecipients...)
		}
	}
	for _, p := range d.SingleValueExtendedProperties {
		fm.props[p.ID] = p.Value
	}
	mb.messages[id] = fm
	mb.order = append(mb.order, id)
	m := fm.m
	g.mu.Unlock()
	writeJSON(w, http.StatusCreated, m)
}

// update answers a message update
// (https://learn.microsoft.com/en-us/graph/api/message-update): a body
// carrying "categories" replaces the message's categories, on a draft or a
// delivered message alike, and records the call the conformance suite
// reads, with the effect the harness noted for the message; any other body
// updates draft fields.
func (g *fakeGraph) update(w http.ResponseWriter, r *http.Request, mb *fakeMailbox, fm *fakeMessage) {
	b, _ := io.ReadAll(r.Body)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		graphError(w, http.StatusBadRequest, "RequestBodyRead")
		return
	}
	if _, isRead := fields["isRead"]; isRead {
		g.patchState(w, r, mb, fm, fields)
		return
	}
	if _, imp := fields["importance"]; imp {
		g.patchState(w, r, mb, fm, fields)
		return
	}
	if _, flag := fields["flag"]; flag {
		g.patchState(w, r, mb, fm, fields)
		return
	}
	raw, ok := fields["categories"]
	if !ok {
		g.updateDraft(w, b, fm)
		return
	}
	var c gCategories
	if err := json.Unmarshal(raw, &c.Categories); err != nil || c.Categories == nil {
		graphError(w, http.StatusBadRequest, "RequestBodyRead")
		return
	}
	g.mu.Lock()
	mb.updates = append(mb.updates, c)
	x := mb.executes[fm.m.ID]
	call := conformance.ProviderCall{Capability: model.CapabilityExtensionExecute, Operation: x.op, Route: x.route, Audience: x.audience}
	if x.action.Name != "" {
		call.Action = &x.action
	}
	fm.m.Categories = c.Categories
	call.Applied, call.References = true, []model.ExternalRef{model.ProviderMessageRef(mb.channel, fm.m.ID)}
	mb.calls = append(mb.calls, call)
	m := fm.m
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, m)
}

func (g *fakeGraph) updateDraft(w http.ResponseWriter, b []byte, fm *fakeMessage) {
	var d gDraft
	if err := json.Unmarshal(b, &d); err != nil {
		graphError(w, http.StatusBadRequest, "RequestBodyRead")
		return
	}
	g.mu.Lock()
	if !fm.m.IsDraft {
		// Stand-in: message-update says body, subject and recipients are
		// "Updatable only if isDraft = true"; the error code is modelled.
		g.mu.Unlock()
		graphError(w, http.StatusBadRequest, "ErrorInvalidRequest")
		return
	}
	if d.Body.ContentType != "" {
		fm.m.Body = &d.Body
	}
	if d.ToRecipients != nil {
		fm.m.ToRecipients = d.ToRecipients
	}
	if d.CcRecipients != nil {
		fm.m.CcRecipients = d.CcRecipients
	}
	if d.BccRecipients != nil {
		fm.bcc = d.BccRecipients
	}
	for _, p := range d.SingleValueExtendedProperties {
		fm.props[p.ID] = p.Value
	}
	m := fm.m
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, m)
}

// send consumes the next scripted response of the mailbox.
func (g *fakeGraph) send(w http.ResponseWriter, mb *fakeMailbox, fm *fakeMessage) {
	g.mu.Lock()
	resp := conformance.Response{Outcome: conformance.OutcomeAccepted}
	if len(mb.responses) > 0 {
		resp, mb.responses = mb.responses[0], mb.responses[1:]
	}
	call := g.callFor(mb, fm, model.CapabilitySend)
	applied := resp.Outcome == conformance.OutcomeAccepted || resp.Outcome == conformance.OutcomeAnswerLost
	if applied {
		fm.folder, fm.m.IsDraft = "sentitems", false
		call.Applied, call.References = true, fakeRefs(mb, fm)
	}
	mb.calls = append(mb.calls, call)
	g.mu.Unlock()
	switch resp.Outcome {
	case conformance.OutcomeAccepted:
		w.WriteHeader(http.StatusAccepted)
	case conformance.OutcomeRejected:
		graphError(w, http.StatusBadRequest, "ErrorInvalidRecipients")
	case conformance.OutcomeTransient:
		graphError(w, http.StatusServiceUnavailable, "ServiceUnavailable")
	case conformance.OutcomeRateLimited:
		w.Header().Set("Retry-After", strconv.Itoa(int(resp.RetryAfter/time.Second)))
		graphError(w, http.StatusTooManyRequests, "TooManyRequests")
	case conformance.OutcomeAnswerLost, conformance.OutcomeRequestLost:
		panic(http.ErrAbortHandler) // the connection drops: no answer
	}
}

// callFor describes a call on fm as the provider saw it.
func (g *fakeGraph) callFor(mb *fakeMailbox, fm *fakeMessage, c model.Capability) conformance.ProviderCall {
	call := conformance.ProviderCall{Capability: c}
	if parts := strings.SplitN(fm.props[AttemptProperty], "/", 3); len(parts) == 3 {
		call.Operation = model.OperationRef{Tenant: model.TenantID(parts[0]), ID: model.ID(parts[1])}
	}
	if len(fm.m.ToRecipients) > 0 {
		call.Route = model.ExternalRef{Channel: mb.channel, Namespace: IdentityNamespace, Kind: refKindAddress,
			ID: fm.m.ToRecipients[0].EmailAddress.Address, ParentID: fm.parent}
	}
	if fm.m.Body != nil {
		call.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: fm.m.Body.Content}}}
	}
	return call
}

func fakeRefs(mb *fakeMailbox, fm *fakeMessage) []model.ExternalRef {
	return []model.ExternalRef{
		model.ProviderMessageRef(mb.channel, fm.m.ID),
		{Channel: mb.channel, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailMessageID, ID: fm.m.InternetMessageID},
	}
}

// filterByAttempt answers the documented extended-property $filter. A
// Sent Items lookup is the reconcile call the provider observes.
func (g *fakeGraph) filterByAttempt(w http.ResponseWriter, r *http.Request, mb *fakeMailbox, sentOnly bool) {
	f := r.URL.Query().Get("$filter")
	sm := reValue.FindStringSubmatch(f)
	if sm == nil || !strings.Contains(f, "ep/id eq '"+AttemptProperty+"'") {
		graphError(w, http.StatusBadRequest, "BadRequest")
		return
	}
	key := strings.ReplaceAll(sm[1], "''", "'")
	g.mu.Lock()
	page := gMessagePage{Value: []gMessage{}}
	var call *conformance.ProviderCall
	for _, id := range mb.order {
		fm, ok := mb.messages[id]
		if !ok || fm.props[AttemptProperty] != key || (sentOnly && fm.folder != "sentitems") {
			continue
		}
		page.Value = append(page.Value, gMessage{ID: fm.m.ID, InternetMessageID: fm.m.InternetMessageID, IsDraft: fm.m.IsDraft})
		if sentOnly {
			c := g.callFor(mb, fm, model.CapabilityReconcile)
			c.Applied, c.References = true, fakeRefs(mb, fm)
			call = &c
		}
	}
	if sentOnly {
		if call == nil {
			parts := strings.SplitN(key, "/", 3)
			c := conformance.ProviderCall{Capability: model.CapabilityReconcile}
			if len(parts) == 3 {
				c.Operation = model.OperationRef{Tenant: model.TenantID(parts[0]), ID: model.ID(parts[1])}
			}
			call = &c
		}
		mb.calls = append(mb.calls, *call)
	}
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, page)
}

func (g *fakeGraph) serveSubscriptions(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1.0/subscriptions"), "/")
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.Method == "POST" && id == "":
		var s gSubscription
		_ = json.NewDecoder(r.Body).Decode(&s)
		for _, ex := range g.subs {
			if strings.EqualFold(strings.TrimPrefix(ex.Resource, "/"), strings.TrimPrefix(s.Resource, "/")) && ex.ChangeType == s.ChangeType {
				graphError(w, http.StatusConflict, "ExtensionError")
				return
			}
		}
		g.subSeq++
		s.ID = fmt.Sprintf("sub-%d", g.subSeq)
		g.subs[s.ID] = &s
		writeJSON(w, http.StatusCreated, s)
	case r.Method == "GET" && id == "":
		page := gSubscriptionPage{Value: []gSubscription{}}
		for _, s := range g.subs {
			c := *s
			c.ClientState = ""
			page.Value = append(page.Value, c)
		}
		writeJSON(w, http.StatusOK, page)
	case r.Method == "PATCH":
		s, ok := g.subs[id]
		if !ok {
			graphError(w, http.StatusNotFound, "ResourceNotFound")
			return
		}
		var p gSubscription
		_ = json.NewDecoder(r.Body).Decode(&p)
		s.ExpirationDateTime = p.ExpirationDateTime
		writeJSON(w, http.StatusOK, s)
	case r.Method == "DELETE":
		if _, ok := g.subs[id]; !ok {
			graphError(w, http.StatusNotFound, "ResourceNotFound")
			return
		}
		delete(g.subs, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		graphError(w, http.StatusBadRequest, "BadRequest")
	}
}

func (g *fakeGraph) subscriptions() []gSubscription {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []gSubscription
	for _, s := range g.subs {
		out = append(out, *s)
	}
	return out
}
