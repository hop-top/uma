package whatsapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"hop.top/uma/conformance"
)

// fakeTwilio is a local Messages API answering in the shapes Twilio
// documents (https://www.twilio.com/docs/messaging/api/message-resource
// and https://www.twilio.com/docs/usage/twilios-response), for WhatsApp
// addresses ("whatsapp:+1..."). Each account
// SID is a separate account with its own scripted answers, consumed in
// call order; once used up it accepts.
type fakeTwilio struct {
	srv *httptest.Server

	mu       sync.Mutex
	accounts map[string]*fakeAccount
	seq      int
	hang     chan struct{}
	// lagging hides messages created from now on from listings until
	// catchUp, as Twilio's Messages list shows a new message only some
	// time after creating it.
	lagging bool
}

type fakeAccount struct {
	script   []conformance.Response
	calls    []fakeCall
	messages map[string]fakeMessage
	order    []string // message SIDs in creation order
	// media are the bytes of incoming media by media SID (ME...);
	// mediaStatus answers a media SID with that HTTP error instead.
	media       map[string][]byte
	mediaStatus map[string]int
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
	Created                     time.Time
	// Unlisted messages are not in listings yet.
	Unlisted bool
}

func newFakeTwilio(t *testing.T) *fakeTwilio {
	t.Helper()
	f := &fakeTwilio{accounts: map[string]*fakeAccount{}, hang: make(chan struct{})}
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
		a = &fakeAccount{messages: map[string]fakeMessage{}, media: map[string][]byte{}, mediaStatus: map[string]int{}}
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

func (f *fakeTwilio) calls(sid string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.account(sid).calls...)
}

// outcomeHang answers nothing until the test ends: a timeout after send.
const outcomeHang conformance.Outcome = "hang"

// outcomeWindowClosed answers 400 with error 63016, Twilio's error for a
// free-form message outside the customer service window
// (https://www.twilio.com/docs/api/errors/63016). The docs do not say
// whether it comes on create or only in a status callback; the adapter
// handles both.
const outcomeWindowClosed conformance.Outcome = "window_closed"

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
		apiError(w, http.StatusUnauthorized, 20003, "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	_ = r.ParseForm()
	f.mu.Lock()
	a := f.account(sid)
	call := fakeCall{Method: r.Method, Path: r.URL.Path, User: user, Form: r.PostForm}
	if parts := strings.Split(resource, "/"); r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "Messages" && parts[2] == "Media" {
		// A media file: its bytes, or the scripted error.
		body, found := a.media[parts[3]]
		code := a.mediaStatus[parts[3]]
		a.calls = append(a.calls, call)
		f.mu.Unlock()
		switch {
		case code != 0:
			apiError(w, code, 0, "")
		case !found:
			apiError(w, http.StatusNotFound, 20404, "")
		default:
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(body)
		}
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(resource, "Messages/") {
		m, found := a.messages[strings.TrimSuffix(strings.TrimPrefix(resource, "Messages/"), ".json")]
		a.calls = append(a.calls, call)
		f.mu.Unlock()
		if !found {
			apiError(w, http.StatusNotFound, 20404, "")
			return
		}
		writeMessage(w, http.StatusOK, sid, m)
		return
	}
	if r.Method == http.MethodGet && resource == "Messages.json" {
		// The Messages list: newest first, filtered by To and From.
		var page []fakeMessage
		for k := len(a.order) - 1; k >= 0; k-- {
			m := a.messages[a.order[k]]
			if !m.Unlisted && m.To == r.URL.Query().Get("To") && (r.URL.Query().Get("From") == "" || m.From == r.URL.Query().Get("From")) {
				page = append(page, m)
			}
		}
		a.calls = append(a.calls, call)
		f.mu.Unlock()
		writeList(w, sid, page)
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
	applies := next.Outcome == conformance.OutcomeAccepted || next.Outcome == conformance.OutcomeAnswerLost
	var m fakeMessage
	if applies {
		f.seq++
		m = fakeMessage{
			SID: fmt.Sprintf("SM%032x", f.seq), To: r.PostForm.Get("To"), From: r.PostForm.Get("From"),
			Body: r.PostForm.Get("Body"), Status: "queued", Created: time.Now().UTC(), Unlisted: f.lagging,
		}
		a.messages[m.SID] = m
		a.order = append(a.order, m.SID)
		call.Applied, call.SID = true, m.SID
	}
	a.calls = append(a.calls, call)
	f.mu.Unlock()

	switch next.Outcome {
	case conformance.OutcomeAccepted:
		writeMessage(w, http.StatusCreated, sid, m)
	case conformance.OutcomeRejected:
		apiError(w, http.StatusBadRequest, 21211, "")
	case outcomeWindowClosed:
		apiError(w, http.StatusBadRequest, 63016, "")
	case conformance.OutcomeRateLimited:
		apiError(w, http.StatusTooManyRequests, 20429, strconv.Itoa(int(next.RetryAfter/time.Second)))
	case conformance.OutcomeAnswerLost, conformance.OutcomeRequestLost:
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

func writeMessage(w http.ResponseWriter, status int, account string, m fakeMessage) {
	body, _ := json.Marshal(messageResource(account, m))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeList answers one Messages list page holding ms, the last page.
func writeList(w http.ResponseWriter, account string, ms []fakeMessage) {
	items := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		items = append(items, messageResource(account, m))
	}
	body, _ := json.Marshal(map[string]any{"messages": items, "next_page_uri": nil, "page": 0, "page_size": 1000})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// messageResource is m as Twilio's message resource. Messages carry the
// real creation time, after any test clock, so a listing for an attempt
// finds the messages created since.
func messageResource(account string, m fakeMessage) map[string]any {
	return map[string]any{
		"account_sid": account, "api_version": "2010-04-01", "body": m.Body,
		"date_created": m.Created.Format(time.RFC1123Z), "date_sent": nil, "date_updated": m.Created.Format(time.RFC1123Z),
		"direction": "outbound-api", "error_code": nil, "error_message": nil, "from": m.From,
		"messaging_service_sid": nil, "num_media": "0", "num_segments": "1", "price": nil, "price_unit": nil,
		"sid": m.SID, "status": m.Status, "to": m.To,
		"subresource_uris": map[string]string{"media": "/2010-04-01/Accounts/" + account + "/Messages/" + m.SID + "/Media.json"},
		"uri":              "/2010-04-01/Accounts/" + account + "/Messages/" + m.SID + ".json",
	}
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
