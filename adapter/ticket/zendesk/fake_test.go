package zendesk_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// fakeZendesk is a Zendesk Support API test server answering in the shapes
// Zendesk documents for the endpoints the adapter uses:
//
//	GET  /{sub}/api/v2/incremental/ticket_events?start_time=&include=comment_events
//	GET  /{sub}/api/v2/tickets/{id}
//	PUT  /{sub}/api/v2/tickets/{id}
//	GET  /{sub}/api/v2/tickets/{id}/audits[?filter_events[]=Comment]
//	GET  /{sub}/api/v2/tickets/{id}/audits/{audit_id}
//
// Each subdomain is one account. Ticket updates take scripted outcomes in
// call order; a transient outcome fails the dial in the client transport,
// before any byte reaches the server.
type fakeZendesk struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	accounts map[string]*fakeAccount
	nextID   int
	// base is the export time of an account's first queued event.
	base int64
	// brand is the brand of tickets an update creates.
	brand string
}

type fakeAccount struct {
	sub     string
	channel model.ChannelRef
	events  []*fakeEvent
	tickets map[string]*fakeTicket
	audits  map[string][]fakeAudit
	gone    map[string]bool
	script  []conformance.Response
	calls   []conformance.ProviderCall
	// writes counts every ticket update request that reached the server.
	writes int
	// starts are the start_time values of the export requests, in order.
	starts []int64
	// auth is the Authorization header of the last request.
	auth string
	// fail answers the next ticket updates with a status, after applying
	// them when applied is set.
	fail []failure
}

type failure struct {
	status  int
	applied bool
}

type fakeEvent struct {
	eventID  string
	time     int64
	ticketID string
	comment  map[string]any
	servedAt time.Time
	acked    bool
}

type fakeTicket struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Status      string `json:"status"`
	RequesterID string `json:"requester_id"`
	AssigneeID  any    `json:"assignee_id"`
	BrandID     string `json:"brand_id"`
	UpdatedAt   string `json:"updated_at"`
}

type fakeAudit struct {
	ID        string           `json:"id"`
	TicketID  string           `json:"ticket_id"`
	CreatedAt string           `json:"created_at"`
	Metadata  map[string]any   `json:"metadata"`
	Events    []map[string]any `json:"events"`
}

// eventBase is the export time of the first queued event: within the
// receiver's default lookback of the conformance clock.
const eventBase = 1_700_000_000 - 1800

// quiet is how long a served event that no later poll passed counts as
// still in flight: the receiver polls again at once after accepting a
// page, so silence that long means it stopped.
const quiet = time.Second

func newFakeZendesk(t *testing.T) *fakeZendesk {
	f := &fakeZendesk{t: t, accounts: map[string]*fakeAccount{}, nextID: 9000, base: eventBase, brand: "support"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// factory returns an adapter factory talking to f.
func (f *fakeZendesk) factory() *zendesk.Factory {
	return zendesk.NewFactory(zendesk.WithBaseURL(f.srv.URL+"/{subdomain}"), zendesk.WithHTTPClient(f.client()))
}

func (f *fakeZendesk) client() *http.Client {
	return &http.Client{Transport: dialGate{f: f, next: http.DefaultTransport}}
}

// dialGate fails a ticket update scripted transient as a dial failure.
type dialGate struct {
	f    *fakeZendesk
	next http.RoundTripper
}

func (g dialGate) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut {
		sub := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)[0]
		g.f.mu.Lock()
		a := g.f.accounts[sub]
		transient := a != nil && len(a.script) > 0 && a.script[0].Outcome == conformance.OutcomeTransient
		if transient {
			a.script = a.script[1:]
			body, _ := io.ReadAll(r.Body)
			a.calls = append(a.calls, g.f.callOf(a, ticketID(r.URL.Path), body, false, nil))
		}
		g.f.mu.Unlock()
		if transient {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: io.ErrUnexpectedEOF}
		}
	}
	return g.next.RoundTrip(r)
}

// account returns the account of subdomain sub, bound to channel ch.
func (f *fakeZendesk) account(sub string, ch model.ChannelRef) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accountLocked(sub, ch)
}

func (f *fakeZendesk) accountLocked(sub string, ch model.ChannelRef) *fakeAccount {
	a, ok := f.accounts[sub]
	if !ok {
		a = &fakeAccount{sub: sub, channel: ch, tickets: map[string]*fakeTicket{}, audits: map[string][]fakeAudit{}, gone: map[string]bool{}}
		f.accounts[sub] = a
	}
	if a.channel == (model.ChannelRef{}) {
		a.channel = ch
	}
	return a
}

func (f *fakeZendesk) id() string {
	f.nextID++
	return strconv.Itoa(f.nextID)
}

// addComment queues a comment for the incremental export and records its
// audit. public nil leaves visibility unstated.
func (f *fakeZendesk) addComment(a *fakeAccount, eventID, ticketID, brand, commentID, authorID, body string, public *bool, viaAPI bool, custom map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := a.tickets[ticketID]; !ok {
		a.tickets[ticketID] = &fakeTicket{ID: ticketID, Subject: "Ticket " + ticketID, Status: "open", RequesterID: authorID, BrandID: brand, UpdatedAt: "2023-11-14T21:00:00Z"}
	}
	auditID := f.id()
	at := f.base + int64(len(a.events))
	channel := "web"
	if viaAPI {
		channel = "api"
	}
	c := map[string]any{
		"id": commentID, "type": "Comment", "event_type": "Comment", "author_id": authorID, "body": body,
		"audit_id": auditID, "created_at": time.Unix(at, 0).UTC().Format(time.RFC3339), "via": map[string]any{"channel": channel},
	}
	if public != nil {
		c["public"] = *public
	}
	a.events = append(a.events, &fakeEvent{eventID: eventID, time: at, ticketID: ticketID, comment: c})
	a.audits[ticketID] = append(a.audits[ticketID], fakeAudit{
		ID: auditID, TicketID: ticketID, CreatedAt: time.Unix(at, 0).UTC().Format(time.RFC3339),
		Metadata: map[string]any{"custom": custom, "system": map[string]any{}},
		Events:   []map[string]any{{"id": commentID, "type": "Comment", "body": body, "public": public != nil && *public}},
	})
}

func (f *fakeZendesk) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accountLocked(parts[0], model.ChannelRef{})
	a.auth = r.Header.Get("Authorization")
	path := "/" + parts[1]
	seg := strings.Split(strings.TrimPrefix(path, "/api/v2/"), "/")
	switch {
	case r.Method == http.MethodGet && path == "/api/v2/incremental/ticket_events":
		f.export(w, r, a)
	case len(seg) == 2 && seg[0] == "tickets" && r.Method == http.MethodGet:
		t, ok := a.tickets[seg[1]]
		if !ok || a.gone[seg[1]] {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "RecordNotFound", "description": "Not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ticket": t})
	case len(seg) == 2 && seg[0] == "tickets" && r.Method == http.MethodPut:
		f.update(w, r, a, seg[1])
	case len(seg) == 3 && seg[0] == "tickets" && seg[2] == "audits" && r.Method == http.MethodGet:
		if a.gone[seg[1]] {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "RecordNotFound"})
			return
		}
		if !r.URL.Query().Has("filter_events[]") {
			a.calls = append(a.calls, conformance.ProviderCall{Capability: model.CapabilityReconcile, Route: route(a.channel, seg[1])})
		}
		list := slices.Clone(a.audits[seg[1]])
		if r.URL.Query().Get("sort_order") == "desc" {
			slices.Reverse(list)
		}
		writeJSON(w, http.StatusOK, map[string]any{"audits": list, "meta": map[string]any{"has_more": false, "after_cursor": nil}})
	case len(seg) == 4 && seg[0] == "tickets" && seg[2] == "audits" && r.Method == http.MethodGet:
		for _, au := range a.audits[seg[1]] {
			if au.ID == seg[3] {
				writeJSON(w, http.StatusOK, map[string]any{"audit": au})
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "RecordNotFound"})
	default:
		http.NotFound(w, r)
	}
}

// export answers the time-based incremental ticket event export: events at
// or after start_time, with end_time the latest returned. A served event a
// later request starts after has been read past: acknowledged.
func (f *fakeZendesk) export(w http.ResponseWriter, r *http.Request, a *fakeAccount) {
	start, err := strconv.ParseInt(r.URL.Query().Get("start_time"), 10, 64)
	if err != nil || r.URL.Query().Get("include") != "comment_events" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "InvalidPaginationParameter"})
		return
	}
	a.starts = append(a.starts, start)
	now := time.Now()
	end := start
	out := []map[string]any{}
	for _, ev := range a.events {
		if ev.time < start {
			if !ev.servedAt.IsZero() {
				ev.acked = true
			}
			continue
		}
		ev.servedAt = now
		end = max(end, ev.time)
		out = append(out, map[string]any{
			"id": f.id(), "ticket_id": ev.ticketID, "timestamp": ev.time, "event_type": "Audit",
			"child_events": []any{ev.comment},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket_events": out, "count": len(out), "end_time": end, "end_of_stream": true})
}

// update applies a ticket update under the account's script.
func (f *fakeZendesk) update(w http.ResponseWriter, r *http.Request, a *fakeAccount, id string) {
	a.writes++
	body, _ := io.ReadAll(r.Body)
	outcome := conformance.Response{Outcome: conformance.OutcomeAccepted}
	if len(a.script) > 0 {
		outcome, a.script = a.script[0], a.script[1:]
	}
	if a.gone[id] {
		a.calls = append(a.calls, f.callOf(a, id, body, false, nil))
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "RecordNotFound"})
		return
	}
	if len(a.fail) > 0 {
		fl := a.fail[0]
		a.fail = a.fail[1:]
		var au *fakeAudit
		if fl.applied {
			x, _ := f.apply(a, id, body)
			au = &x
		}
		a.calls = append(a.calls, f.callOf(a, id, body, fl.applied, au))
		writeJSON(w, fl.status, map[string]any{"error": "ServerError"})
		return
	}
	switch outcome.Outcome {
	case conformance.OutcomeRejected:
		a.calls = append(a.calls, f.callOf(a, id, body, false, nil))
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "RecordInvalid", "description": "Record validation errors"})
		return
	case conformance.OutcomeRateLimited:
		a.calls = append(a.calls, f.callOf(a, id, body, false, nil))
		w.Header().Set("Retry-After", strconv.Itoa(int(outcome.RetryAfter/time.Second)))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "TooManyRequests"})
		return
	case conformance.OutcomeRequestLost:
		a.calls = append(a.calls, f.callOf(a, id, body, false, nil))
		writeJSON(w, http.StatusGatewayTimeout, nil)
		return
	}
	au, ok := f.apply(a, id, body)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "InvalidJSON"})
		return
	}
	a.calls = append(a.calls, f.callOf(a, id, body, true, &au))
	if outcome.Outcome == conformance.OutcomeAnswerLost {
		writeJSON(w, http.StatusBadGateway, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": au, "ticket": a.tickets[id]})
}

// apply records the update's audit: its comment and custom metadata.
func (f *fakeZendesk) apply(a *fakeAccount, id string, body []byte) (fakeAudit, bool) {
	var req struct {
		Ticket map[string]json.RawMessage `json:"ticket"`
	}
	if json.Unmarshal(body, &req) != nil || req.Ticket == nil {
		return fakeAudit{}, false
	}
	t, ok := a.tickets[id]
	if !ok {
		t = &fakeTicket{ID: id, Subject: "Ticket " + id, Status: "open", RequesterID: "1", BrandID: f.brand}
		a.tickets[id] = t
	}
	var custom map[string]any
	_ = json.Unmarshal(req.Ticket["metadata"], &custom)
	au := fakeAudit{ID: f.id(), TicketID: id, CreatedAt: time.Now().UTC().Format(time.RFC3339), Metadata: map[string]any{"custom": custom, "system": map[string]any{}}}
	if raw, ok := req.Ticket["comment"]; ok {
		var c map[string]any
		_ = json.Unmarshal(raw, &c)
		au.Events = append(au.Events, map[string]any{"id": f.id(), "type": "Comment", "body": c["body"], "public": c["public"]})
	}
	var status string
	if json.Unmarshal(req.Ticket["status"], &status) == nil && status != "" {
		t.Status = status
		au.Events = append(au.Events, map[string]any{"id": f.id(), "type": "Change", "field_name": "status", "value": status})
	}
	a.audits[id] = append(a.audits[id], au)
	return au, true
}

// callOf is the conformance view of one ticket update request.
func (f *fakeZendesk) callOf(a *fakeAccount, id string, body []byte, applied bool, au *fakeAudit) conformance.ProviderCall {
	var req struct {
		Ticket map[string]json.RawMessage `json:"ticket"`
	}
	_ = json.Unmarshal(body, &req)
	var m struct {
		Operation  string `json:"uma_operation"`
		Effect     string `json:"uma_effect"`
		Visibility string `json:"uma_visibility"`
		PolicyRef  string `json:"uma_policy_ref"`
	}
	_ = json.Unmarshal(req.Ticket["metadata"], &m)
	call := conformance.ProviderCall{
		Capability: model.CapabilityExtensionExecute,
		Operation:  model.OperationRef{Tenant: a.channel.Tenant, ID: model.ID(m.Operation)},
		Route:      route(a.channel, id), Applied: applied,
		Audience: model.Audience{Visibility: model.Visibility(m.Visibility), PolicyRef: m.PolicyRef},
	}
	var comment map[string]json.RawMessage
	_ = json.Unmarshal(req.Ticket["comment"], &comment)
	var text string
	_ = json.Unmarshal(comment["body"], &text)
	if text != "" {
		call.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}
	}
	switch m.Effect {
	case "reply":
		call.Capability = model.CapabilitySend
	case "message_note":
		call.Capability = model.CapabilityInternalNote
	case "note":
		delete(comment, "public")
		data, _ := json.Marshal(comment)
		call.Action = &model.Extension{Name: zendesk.ExtensionInternalNote, Version: zendesk.ExtensionVersion, Data: data}
	case "update":
		fields := map[string]json.RawMessage{}
		for k, v := range req.Ticket {
			if k != "metadata" {
				fields[k] = v
			}
		}
		data, _ := json.Marshal(fields)
		call.Action = &model.Extension{Name: zendesk.ExtensionUpdate, Version: zendesk.ExtensionVersion, Data: data}
	}
	if au != nil {
		for _, e := range au.Events {
			if e["type"] == "Comment" {
				call.References = append(call.References, model.ProviderMessageRef(a.channel, e["id"].(string)))
			}
		}
		call.References = append(call.References, model.ExternalRef{Channel: a.channel, Namespace: zendesk.Namespace, Kind: zendesk.RefKindAudit, ID: au.ID, ParentID: id})
	}
	return call
}

func route(ch model.ChannelRef, ticket string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: zendesk.Namespace, Kind: model.RefKindWorkItem, ID: ticket}
}

func ticketID(path string) string {
	i := strings.LastIndex(path, "/")
	return path[i+1:]
}

// failNext answers the account's next ticket update with status.
func (f *fakeZendesk) failNext(a *fakeAccount, status int, applied bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.fail = append(a.fail, failure{status: status, applied: applied})
}

// lastAudit returns the newest audit of a ticket.
func (f *fakeZendesk) lastAudit(a *fakeAccount, ticket string) fakeAudit {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := a.audits[ticket]
	if len(list) == 0 {
		f.t.Fatalf("ticket %s has no audit", ticket)
	}
	return list[len(list)-1]
}

func (f *fakeZendesk) callsOf(a *fakeAccount) []conformance.ProviderCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(a.calls)
}

func (f *fakeZendesk) writesOf(a *fakeAccount) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return a.writes
}

// inbound reports served events that were read past or have gone quiet.
func (f *fakeZendesk) inbound(a *fakeAccount) []conformance.InboundResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []conformance.InboundResult
	evs := slices.Clone(a.events)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].time < evs[j].time })
	for _, ev := range evs {
		if ev.servedAt.IsZero() || (!ev.acked && time.Since(ev.servedAt) < quiet) {
			continue
		}
		out = append(out, conformance.InboundResult{EventID: ev.eventID, Acked: ev.acked})
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
