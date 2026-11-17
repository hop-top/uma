package twilio_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

const (
	accountSID = "AC00000000000000000000000000000001"
	authToken  = "auth-tokenCred-fixture" // #nosec G101 -- fixture value, not a credential
	messageSID = "SM00000000000000000000000000000001"
)

// createdBody is the documented Create Message response
// (https://www.twilio.com/docs/messaging/api/message-resource), with
// fixture SIDs and numbers.
const createdBody = `{
  "account_sid": "AC00000000000000000000000000000001",
  "api_version": "2010-04-01",
  "body": "Hi there",
  "date_created": "Thu, 24 Aug 2023 05:01:45 +0000",
  "date_sent": "Thu, 24 Aug 2023 05:01:45 +0000",
  "date_updated": "Thu, 24 Aug 2023 05:01:45 +0000",
  "direction": "outbound-api",
  "error_code": null,
  "error_message": null,
  "from": "+15557122661",
  "messaging_service_sid": null,
  "num_media": "0",
  "num_segments": "1",
  "price": null,
  "price_unit": null,
  "sid": "SM00000000000000000000000000000001",
  "status": "queued",
  "subresource_uris": {"media": "/2010-04-01/Accounts/AC00000000000000000000000000000001/Messages/SM00000000000000000000000000000001/Media.json"},
  "to": "+15558675310",
  "uri": "/2010-04-01/Accounts/AC00000000000000000000000000000001/Messages/SM00000000000000000000000000000001.json"
}`

// errorBody is the documented REST error shape
// (https://www.twilio.com/docs/usage/twilios-response).
const errorBody = `{"status": 400, "message": "No to number is specified", "code": 21201, "more_info": "http://www.twilio.com/docs/errors/21201"}`

type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func (s *seen) add(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r)
	s.body = append(s.body, string(b))
}

func (s *seen) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func newClient(t *testing.T, h http.HandlerFunc) (*twilio.Client, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.add(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := twilio.NewClient(twilio.ClientConfig{
		HTTP: srv.Client(), BaseURL: srv.URL, Timeout: 2 * time.Second,
		Clock: portstest.NewFixedClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func reply(status int, body string, hdr ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

var tokenCred = ports.Secret{Bytes: []byte(authToken)}

func TestCreateMessageRequestShape(t *testing.T) {
	c, s := newClient(t, reply(http.StatusCreated, createdBody))
	m, err := c.CreateMessage(t.Context(), accountSID, tokenCred, twilio.CreateMessage{
		To: "+15558675310", From: "+15557122661", Body: "Hi there", StatusCallback: "https://hooks.example.com/sms?operation=op1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.SID != messageSID || m.Status != "queued" || m.ErrorCode != nil || m.To != "+15558675310" {
		t.Fatalf("message = %+v", m)
	}
	r := s.reqs[0]
	if r.Method != http.MethodPost || r.URL.Path != "/2010-04-01/Accounts/"+accountSID+"/Messages.json" {
		t.Fatalf("request %s %s", r.Method, r.URL.Path)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Fatalf("content type %q", ct)
	}
	if user, pass, ok := r.BasicAuth(); !ok || user != accountSID || pass != authToken {
		t.Fatalf("basic auth = %q, %v; want the account SID and auth tokenCred", user, ok)
	}
	form, _ := url.ParseQuery(s.body[0])
	want := url.Values{
		"To": {"+15558675310"}, "From": {"+15557122661"}, "Body": {"Hi there"},
		"StatusCallback": {"https://hooks.example.com/sms?operation=op1"},
	}
	if form.Encode() != want.Encode() {
		t.Fatalf("form = %v, want %v", form, want)
	}
}

func TestCreateMessageAPIKeyAuthAndService(t *testing.T) {
	c, s := newClient(t, reply(http.StatusCreated, createdBody))
	key := ports.Password{User: "SK00000000000000000000000000000001", Secret: []byte("key-secret")}
	if _, err := c.CreateMessage(t.Context(), accountSID, key, twilio.CreateMessage{
		To: "+15558675310", MessagingServiceSID: "MG00000000000000000000000000000001", Body: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if user, pass, _ := s.reqs[0].BasicAuth(); user != key.User || pass != "key-secret" {
		t.Fatalf("basic auth user %q", user)
	}
	form, _ := url.ParseQuery(s.body[0])
	if form.Get("MessagingServiceSid") == "" || form.Has("From") || form.Has("StatusCallback") {
		t.Fatalf("form = %v", form)
	}
}

// A content template send carries ContentSid and ContentVariables
// (https://www.twilio.com/docs/content/send-templates-created-with-the-content-template-builder)
// and no Body.
func TestCreateMessageContentTemplate(t *testing.T) {
	c, s := newClient(t, reply(http.StatusCreated, createdBody))
	if _, err := c.CreateMessage(t.Context(), accountSID, tokenCred, twilio.CreateMessage{
		To: "whatsapp:+15558675310", From: "whatsapp:+15557122661",
		ContentSID: "HX00000000000000000000000000000001", ContentVariables: `{"1":"Jad"}`,
	}); err != nil {
		t.Fatal(err)
	}
	form, _ := url.ParseQuery(s.body[0])
	want := url.Values{
		"To": {"whatsapp:+15558675310"}, "From": {"whatsapp:+15557122661"},
		"ContentSid": {"HX00000000000000000000000000000001"}, "ContentVariables": {`{"1":"Jad"}`},
	}
	if form.Encode() != want.Encode() {
		t.Fatalf("form = %v, want %v", form, want)
	}
	if !twilio.ValidContentSID("HX00000000000000000000000000000001") || twilio.ValidContentSID("HXnope") || twilio.ValidContentSID(messageSID) {
		t.Fatal("ValidContentSID misjudges")
	}
}

func TestFetchMessage(t *testing.T) {
	c, s := newClient(t, reply(http.StatusOK, strings.Replace(createdBody, `"queued"`, `"delivered"`, 1)))
	m, err := c.FetchMessage(t.Context(), accountSID, tokenCred, messageSID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "delivered" {
		t.Fatalf("status = %q", m.Status)
	}
	if r := s.reqs[0]; r.Method != http.MethodGet || r.URL.Path != "/2010-04-01/Accounts/"+accountSID+"/Messages/"+messageSID+".json" {
		t.Fatalf("request %s %s", r.Method, r.URL.Path)
	}
	if _, err := c.FetchMessage(t.Context(), accountSID, tokenCred, "../x"); model.CodeOf(err) != model.Validation {
		t.Fatalf("malformed SID = %v, want validation", err)
	}
}

func TestAPIErrorShape(t *testing.T) {
	c, _ := newClient(t, reply(http.StatusTooManyRequests, `{"code":20429,"message":"Too Many Requests","more_info":"https://www.twilio.com/docs/errors/20429","status":429}`, "Retry-After", "7"))
	_, err := c.CreateMessage(t.Context(), accountSID, tokenCred, twilio.CreateMessage{To: "+1", From: "+2", Body: "x"})
	var ae *twilio.APIError
	if !errors.As(err, &ae) || ae.Status != 429 || ae.Code != 20429 || ae.RetryAfter != 7*time.Second {
		t.Fatalf("err = %#v", err)
	}
	c, _ = newClient(t, reply(http.StatusTooManyRequests, `{}`, "Retry-After", "Sun, 11 Oct 2026 12:01:00 GMT"))
	_, err = c.CreateMessage(t.Context(), accountSID, tokenCred, twilio.CreateMessage{To: "+1", From: "+2", Body: "x"})
	if !errors.As(err, &ae) || ae.RetryAfter != time.Minute {
		t.Fatalf("HTTP-date Retry-After: err = %#v", err)
	}
	c, _ = newClient(t, reply(http.StatusBadRequest, errorBody))
	_, err = c.CreateMessage(t.Context(), accountSID, tokenCred, twilio.CreateMessage{From: "+2", Body: "x"})
	if !errors.As(err, &ae) || ae.Status != 400 || ae.Code != 21201 {
		t.Fatalf("err = %#v", err)
	}
	if strings.Contains(err.Error(), "No to number") {
		t.Fatalf("error text %q echoes the provider message", err)
	}
}

func TestCredentialKinds(t *testing.T) {
	c, s := newClient(t, reply(http.StatusCreated, createdBody))
	for name, cred := range map[string]ports.Credential{
		"oauth2":         ports.OAuth2{AccessToken: "x"},
		"empty secret":   ports.Secret{},
		"nil":            nil,
		"user not a SID": ports.Password{User: "someone", Secret: []byte("x")},
		"other account":  ports.Password{User: "AC00000000000000000000000000000009", Secret: []byte("x")},
	} {
		_, err := c.CreateMessage(t.Context(), accountSID, cred, twilio.CreateMessage{To: "+1", From: "+2", Body: "x"})
		if code := model.CodeOf(err); code != model.Unsupported && code != model.Unauthorized {
			t.Errorf("%s credential = %v", name, err)
		}
	}
	if _, err := c.CreateMessage(t.Context(), "not-a-sid", tokenCred, twilio.CreateMessage{}); model.CodeOf(err) != model.Validation {
		t.Errorf("bad account SID = %v", err)
	}
	if n := s.count(); n != 0 {
		t.Fatalf("requests = %d, want none", n)
	}
}

// A request the server read but never answered is possibly sent; one the
// transport could not even connect for was not.
func TestRequestErrorSent(t *testing.T) {
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := c.CreateMessage(ctx, accountSID, tokenCred, twilio.CreateMessage{To: "+1", From: "+2", Body: "x"})
	var re *twilio.RequestError
	if !errors.As(err, &re) || !re.Sent || s.count() != 1 {
		t.Fatalf("timeout after send: err = %v", err)
	}
	if strings.Contains(err.Error(), accountSID) {
		t.Fatalf("error %q names the request URL", err)
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	dead, err := twilio.NewClient(twilio.ClientConfig{HTTP: http.DefaultClient, BaseURL: base})
	if err != nil {
		t.Fatal(err)
	}
	_, err = dead.CreateMessage(t.Context(), accountSID, tokenCred, twilio.CreateMessage{To: "+1", From: "+2", Body: "x"})
	if !errors.As(err, &re) || re.Sent {
		t.Fatalf("connection refused: err = %v, want not sent", err)
	}
}

// A transport that reports no trace events cannot prove the request
// stayed local.
func TestUntracedTransportCountsAsSent(t *testing.T) {
	c, err := twilio.NewClient(twilio.ClientConfig{HTTP: doFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("replay miss")
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateMessage(t.Context(), accountSID, tokenCred, twilio.CreateMessage{To: "+1", From: "+2", Body: "x"})
	var re *twilio.RequestError
	if !errors.As(err, &re) || !re.Sent {
		t.Fatalf("err = %v, want possibly sent", err)
	}
}

type doFunc func(*http.Request) (*http.Response, error)

func (f doFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewClientConfig(t *testing.T) {
	for _, base := range []string{"ftp://x", "https://", "https://u:p@api.twilio.com", "https://api.twilio.com?x=1", "::"} {
		if _, err := twilio.NewClient(twilio.ClientConfig{HTTP: http.DefaultClient, BaseURL: base}); err == nil {
			t.Errorf("base %q accepted", base)
		}
	}
	if _, err := twilio.NewClient(twilio.ClientConfig{}); err == nil {
		t.Error("nil HTTP accepted")
	}
}
