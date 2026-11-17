package sms_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"hop.top/uma/conformance"
)

// fakeTwilio is a local Messages API answering in Twilio's recorded
// shapes (recordedAnswers): an accepted create (the recorded create answer
// as a template), message resources, list pages, a refused create, refused
// credentials and an unknown message. Its other answers
// (429, 5xx, media) follow the shapes Twilio documents
// (https://www.twilio.com/docs/messaging/api/message-resource and
// https://www.twilio.com/docs/usage/twilios-response). Each account SID is
// a separate account with its own scripted answers, consumed in call
// order; once used up it accepts.
type fakeTwilio struct {
	srv      *httptest.Server
	recorded *recordedAnswers

	mu       sync.Mutex
	accounts map[string]*fakeAccount
	seq      int
	hang     chan struct{}
	// lagging hides messages created from now on from listings until
	// catchUp, as Twilio's Messages list shows a new message only some
	// time after creating it.
	lagging bool
	// pageCap caps a list page below the requested PageSize, zero none,
	// so a test reads a window across several pages.
	pageCap int
}

type fakeAccount struct {
	script   []conformance.Response
	calls    []fakeCall
	messages map[string]fakeMessage
	order    []string // message SIDs in creation order
	// media are the bytes of incoming media, by media SID (ME...).
	media map[string][]byte
}

// fakeCall is one request as the fake received it.
type fakeCall struct {
	Method  string
	Path    string
	User    string
	Form    url.Values
	Applied bool
	SID     string
}

type fakeMessage struct {
	SID, To, From, Body, Status string
	// Direction is Twilio's direction; empty is outbound-api, a create.
	Direction string
	// Sent is the date_sent of an inbound message; zero uses Created.
	Sent time.Time
	// Media is the num_media of an inbound message.
	Media int
	// ErrorCode is Twilio's error code of a failed message, zero for none.
	ErrorCode int
	Created   time.Time
	// Unlisted messages are not in listings yet.
	Unlisted bool
}

func newFakeTwilio(t *testing.T) *fakeTwilio {
	t.Helper()
	recorded, err := loadRecordedAnswers()
	if err != nil {
		t.Fatalf("recorded Twilio answers: %v", err)
	}
	f := &fakeTwilio{accounts: map[string]*fakeAccount{}, hang: make(chan struct{}), recorded: recorded}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		close(f.hang)
		f.srv.Close()
	})
	return f
}

func (f *fakeTwilio) account(sid string) *fakeAccount {
	a, ok := f.accounts[sid]
	if !ok {
		a = &fakeAccount{messages: map[string]fakeMessage{}, media: map[string][]byte{}}
		f.accounts[sid] = a
	}
	return a
}

func (f *fakeTwilio) respond(sid string, rs ...conformance.Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.account(sid)
	a.script = append(a.script, rs...)
}

func (f *fakeTwilio) calls(sid string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.account(sid).calls...)
}

// holdInbound places m on the account as a message Twilio received,
// once: an inbound message as the documented resource reports one
// (direction inbound, status received). This is a stand-in: no inbound
// message is recorded yet.
func (f *fakeTwilio) holdInbound(account string, m fakeMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.account(account)
	m.Direction, m.Status = "inbound", "received"
	if m.Created.IsZero() {
		m.Created = m.Sent
	}
	if _, ok := a.messages[m.SID]; !ok {
		a.order = append(a.order, m.SID)
	}
	a.messages[m.SID] = m
}

// lagListings hides the messages created from now on from listings until
// catchUp.
func (f *fakeTwilio) lagListings() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lagging = true
}

// catchUp lists every message created so far.
func (f *fakeTwilio) catchUp() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lagging = false
	for _, a := range f.accounts {
		for sid, m := range a.messages {
			m.Unlisted = false
			a.messages[sid] = m
		}
	}
}

// outcomeHang answers nothing until the test ends: a timeout after send.
const outcomeHang conformance.Outcome = "hang"

// outcomeRefusedAnswerLost is a create Twilio refuses with error 21266
// whose answer is lost: like Twilio, the fake keeps the refused message, as
// failed.
const outcomeRefusedAnswerLost conformance.Outcome = "refused_answer_lost"

// outcomeStatus answers a Twilio error with HTTP status code, after the
// request reached the server; the fake applies nothing.
func outcomeStatus(code int) conformance.Outcome {
	return conformance.Outcome("http_" + strconv.Itoa(code))
}

// client returns the HTTP client the adapter uses. A scripted
// conformance.OutcomeTransient is a connection refused before anything is
// written: the request never reaches the server.
func (f *fakeTwilio) client() *http.Client {
	refuse := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}}
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if f.refuseNext(r) {
			return refuse.RoundTrip(r)
		}
		return f.srv.Client().Transport.RoundTrip(r)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

// refuseNext consumes a scripted transient answer for a create request and
// records the unapplied call.
func (f *fakeTwilio) refuseNext(r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/2010-04-01/Accounts/")
	if !ok || r.Method != http.MethodPost {
		return false
	}
	sid, _, _ := strings.Cut(rest, "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.account(sid)
	if len(a.script) == 0 || a.script[0].Outcome != conformance.OutcomeTransient {
		return false
	}
	a.script = a.script[1:]
	var form url.Values
	if r.GetBody != nil {
		if b, err := r.GetBody(); err == nil {
			data, _ := io.ReadAll(b)
			form, _ = url.ParseQuery(string(data))
		}
	}
	a.calls = append(a.calls, fakeCall{Method: r.Method, Path: r.URL.Path, Form: form})
	return true
}

func (f *fakeTwilio) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/2010-04-01/Accounts/")
	if !ok {
		apiError(w, http.StatusNotFound, 20404, "")
		return
	}
	sid, resource, _ := strings.Cut(rest, "/")
	user, pass, okAuth := r.BasicAuth()
	// Basic auth is the account SID and auth token, or an API key SID and
	// secret.
	apiKey := len(user) == 34 && strings.HasPrefix(user, "SK")
	if !okAuth || (user != sid && !apiKey) || pass == "" {
		f.recorded.unauthorized.write(w, recordedPlaceholder, sid)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	_ = r.ParseForm()
	f.mu.Lock()
	a := f.account(sid)
	call := fakeCall{Method: r.Method, Path: r.URL.Path, User: user, Form: r.PostForm}
	if parts := strings.Split(resource, "/"); r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "Messages" && parts[2] == "Media" {
		// A media file: its bytes, under its content type.
		body, found := a.media[parts[3]]
		a.calls = append(a.calls, call)
		f.mu.Unlock()
		if !found {
			apiError(w, http.StatusNotFound, 20404, "")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(resource, "Messages/") {
		m, found := a.messages[strings.TrimSuffix(strings.TrimPrefix(resource, "Messages/"), ".json")]
		a.calls = append(a.calls, call)
		f.mu.Unlock()
		if !found {
			f.recorded.notFound.write(w, f.recorded.notFoundPath, r.URL.Path)
			return
		}
		f.writeMessage(w, http.StatusOK, sid, m)
		return
	}
	if r.Method == http.MethodGet && resource == "Messages.json" {
		page, ok := f.list(a, r.URL.Query())
		a.calls = append(a.calls, call)
		f.mu.Unlock()
		if !ok {
			apiError(w, http.StatusBadRequest, 20001, "")
			return
		}
		f.writeList(w, r, sid, page)
		return
	}
	if r.Method != http.MethodPost || resource != "Messages.json" {
		f.mu.Unlock()
		apiError(w, http.StatusMethodNotAllowed, 0, "")
		return
	}
	next := conformance.Response{Outcome: conformance.OutcomeAccepted}
	if len(a.script) > 0 {
		next, a.script = a.script[0], a.script[1:]
	}
	applies := next.Outcome == conformance.OutcomeAccepted || next.Outcome == conformance.OutcomeAnswerLost ||
		next.Outcome == outcomeRefusedAnswerLost
	var m fakeMessage
	if applies {
		f.seq++
		m = fakeMessage{
			SID: fmt.Sprintf("SM%032x", f.seq), To: r.PostForm.Get("To"), From: r.PostForm.Get("From"),
			Body: r.PostForm.Get("Body"), Status: "queued", Created: time.Now().UTC(), Unlisted: f.lagging,
		}
		if next.Outcome == outcomeRefusedAnswerLost {
			m.Status, m.ErrorCode = "failed", 21266
		}
		a.messages[m.SID] = m
		a.order = append(a.order, m.SID)
		call.Applied, call.SID = true, m.SID
	}
	a.calls = append(a.calls, call)
	f.mu.Unlock()

	switch next.Outcome {
	case conformance.OutcomeAccepted:
		f.writeCreated(w, sid, m)
	case conformance.OutcomeRejected:
		f.recorded.rejected.write(w)
	case conformance.OutcomeRateLimited:
		apiError(w, http.StatusTooManyRequests, 20429, strconv.Itoa(int(next.RetryAfter/time.Second)))
	case conformance.OutcomeAnswerLost, conformance.OutcomeRequestLost, outcomeRefusedAnswerLost:
		drop(w)
	case outcomeHang:
		select {
		case <-f.hang:
		case <-r.Context().Done():
		}
	default:
		code, err := strconv.Atoi(strings.TrimPrefix(string(next.Outcome), "http_"))
		if err != nil {
			code = http.StatusInternalServerError
		}
		apiError(w, code, 0, "")
	}
}

// drop closes the connection without an answer.
func drop(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func (f *fakeTwilio) writeMessage(w http.ResponseWriter, status int, account string, m fakeMessage) {
	body, _ := json.Marshal(f.messageResource(account, m))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeCreated answers an accepted create as Twilio's recorded one does
// (status, content type and message resource).
func (f *fakeTwilio) writeCreated(w http.ResponseWriter, account string, m fakeMessage) {
	body, _ := json.Marshal(f.createdResource(account, m))
	w.Header().Set("Content-Type", f.recorded.created.contentType)
	w.WriteHeader(f.recorded.created.status)
	_, _ = w.Write(body)
}

// createdResource is the message resource of an accepted create of m:
// the recorded answer to a create, its account and message SIDs replaced
// and its per-call fields (to, from, body and the creation dates) set to
// m's. Every other field is as recorded: the field set, the status Twilio
// answers with (queued), the null date_sent, price and error code, the
// price unit, the segment count and the subresource URIs.
func (f *fakeTwilio) createdResource(account string, m fakeMessage) map[string]any {
	text := strings.NewReplacer(recordedPlaceholder, account, f.recorded.createdSID, m.SID).Replace(f.recorded.created.body)
	var res map[string]any
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		panic("recorded create answer: " + err.Error())
	}
	created := m.Created.Format(time.RFC1123Z)
	res["to"], res["from"], res["body"] = m.To, m.From, m.Body
	res["date_created"], res["date_updated"] = created, created
	return res
}

// listPage is one page of a Messages list: its messages and the query of
// the next page, nil on the last.
type listPage struct {
	messages []fakeMessage
	next     url.Values
}

// list answers a Messages list query: messages newest first by date_sent
// (later creations first within one second), filtered by To and From and
// by the documented DateSent> filter, which the stand-in reads as Twilio
// documents it, a whole UTC day, inclusive
// (https://www.twilio.com/docs/messaging/api/message-resource). A
// message without date_sent (queued) has none to match. The page holds
// PageSize messages, at most pageCap, from Page; ok is false for a query
// Twilio documents no meaning for.
func (f *fakeTwilio) list(a *fakeAccount, q url.Values) (listPage, bool) {
	var day time.Time
	if v := q.Get("DateSent>"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return listPage{}, false
		}
		day = t.UTC().Truncate(24 * time.Hour)
	}
	size, err := strconv.Atoi(q.Get("PageSize"))
	if err != nil || size < 1 || size > 1000 {
		size = 50
	}
	if f.pageCap > 0 {
		size = min(size, f.pageCap)
	}
	n, _ := strconv.Atoi(q.Get("Page"))
	var all []fakeMessage
	for k := len(a.order) - 1; k >= 0; k-- {
		m := a.messages[a.order[k]]
		sent, dated := m.sentAt()
		switch {
		case m.Unlisted, q.Get("To") != "" && m.To != q.Get("To"), q.Get("From") != "" && m.From != q.Get("From"):
		case !day.IsZero() && (!dated || sent.Before(day)):
		default:
			all = append(all, m)
		}
	}
	slices.SortStableFunc(all, func(x, y fakeMessage) int {
		xs, _ := x.sentAt()
		ys, _ := y.sentAt()
		return ys.Truncate(time.Second).Compare(xs.Truncate(time.Second))
	})
	start := min(n*size, len(all))
	end := min(start+size, len(all))
	p := listPage{messages: all[start:end]}
	if end < len(all) {
		p.next = url.Values{}
		for k, v := range q {
			p.next[k] = v
		}
		p.next.Set("Page", strconv.Itoa(n+1))
		p.next.Set("PageToken", "PA"+all[end-1].SID)
	}
	return p, true
}

// sentAt is m's date_sent: none while queued.
func (m fakeMessage) sentAt() (time.Time, bool) {
	switch {
	case !m.Sent.IsZero():
		return m.Sent, true
	case m.Status == "queued":
		return time.Time{}, false
	}
	return m.Created, true
}

// writeList answers one Messages list page in the envelope of a recorded
// page; its next_page_uri stays on the account's list, as Twilio's does.
func (f *fakeTwilio) writeList(w http.ResponseWriter, r *http.Request, account string, lp listPage) {
	ms := lp.messages
	var page map[string]any
	if err := json.Unmarshal([]byte(f.recorded.listing), &page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	items := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		items = append(items, f.messageResource(account, m))
	}
	page["messages"], page["start"], page["end"] = items, 0, max(len(items)-1, 0)
	page["uri"], page["first_page_uri"], page["next_page_uri"] = r.URL.RequestURI(), r.URL.RequestURI(), nil
	if lp.next != nil {
		page["next_page_uri"] = r.URL.Path + "?" + lp.next.Encode()
	}
	body, _ := json.Marshal(page)
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	_, _ = w.Write(body)
}

// messageResource is m as Twilio's message resource, on a recorded one
// from a listing (a create's answer is createdResource).
// Messages carry the real creation time, after any test clock, so a
// listing for an attempt finds the messages created since.
func (f *fakeTwilio) messageResource(account string, m fakeMessage) map[string]any {
	var res map[string]any
	_ = json.Unmarshal([]byte(f.recorded.message), &res)
	var code, sent any
	if m.ErrorCode != 0 {
		code = m.ErrorCode
	}
	if at, ok := m.sentAt(); ok {
		sent = at.Format(time.RFC1123Z)
	}
	direction := m.Direction
	if direction == "" {
		direction = "outbound-api"
	}
	uri := "/2010-04-01/Accounts/" + account + "/Messages/" + m.SID
	for k, v := range map[string]any{
		"account_sid": account, "body": m.Body, "date_created": m.Created.Format(time.RFC1123Z),
		"date_sent": sent, "date_updated": m.Created.Format(time.RFC1123Z), "direction": direction,
		"error_code": code, "error_message": nil, "from": m.From, "messaging_service_sid": nil,
		"num_media": strconv.Itoa(m.Media), "num_segments": "1", "price": nil, "sid": m.SID, "status": m.Status, "to": m.To,
		"subresource_uris": map[string]string{"media": uri + "/Media.json", "feedback": uri + "/Feedback.json"},
		"uri":              uri + ".json",
	} {
		res[k] = v
	}
	return res
}

func apiError(w http.ResponseWriter, status, code int, retryAfter string) {
	body := map[string]any{"status": status, "message": http.StatusText(status)}
	if code != 0 {
		body["code"] = code
		body["more_info"] = "https://www.twilio.com/docs/errors/" + strconv.Itoa(code)
	}
	b, _ := json.Marshal(body)
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
