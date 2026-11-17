package sms_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter/messaging/twilio/sms"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// cdnHost stands for the media store a Media URL may redirect to. The
// world routes it to a TLS test server; any other target has no route.
const cdnHost = "media.cdn.example"

// seenCall is one request that reached a test server.
type seenCall struct{ Server, Path, Auth string }

// world is the network the adapter's HTTP client sees: the Twilio API at a
// plain test server, a media store at cdnHost behind TLS, and nothing else.
// A request to any other place is a stray: the redirect policy exists to
// make sure there are none.
type world struct {
	api, cdn *httptest.Server
	secret   *httptest.Server // a loopback service no redirect may reach

	mu      sync.Mutex
	seen    []seenCall
	stray   []string
	hosts   map[string]bool // hosts routed to the media store
	apiFn   http.HandlerFunc
	cdnFn   http.HandlerFunc
	secrets int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{hosts: map[string]bool{cdnHost: true}}
	record := func(name string, fn func() http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			w.mu.Lock()
			w.seen = append(w.seen, seenCall{name, r.URL.Path, r.Header.Get("Authorization")})
			h := fn()
			w.mu.Unlock()
			h(rw, r)
		})
	}
	w.api = httptest.NewServer(record("api", func() http.HandlerFunc { return w.apiFn }))
	w.cdn = httptest.NewTLSServer(record("cdn", func() http.HandlerFunc { return w.cdnFn }))
	w.secret = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.secrets++
		w.mu.Unlock()
		_, _ = rw.Write([]byte("secret"))
	}))
	t.Cleanup(func() { w.api.Close(); w.cdn.Close(); w.secret.Close() })
	return w
}

// RoundTrip routes the API origin and the media store, nothing else.
func (w *world) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case r.URL.Scheme == "http" && r.URL.Host == strings.TrimPrefix(w.api.URL, "http://"):
		return http.DefaultTransport.RoundTrip(r)
	case r.URL.Scheme == "https" && r.URL.Port() == "" && w.hosts[r.URL.Hostname()]:
		r2 := r.Clone(r.Context())
		r2.URL.Host = strings.TrimPrefix(w.cdn.URL, "https://")
		return w.cdn.Client().Transport.RoundTrip(r2)
	}
	w.mu.Lock()
	w.stray = append(w.stray, r.URL.Redacted())
	w.mu.Unlock()
	return nil, errors.New("no route to host")
}

func (w *world) calls(server string) []seenCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []seenCall
	for _, c := range w.seen {
		if c.Server == server {
			out = append(out, c)
		}
	}
	return out
}

// The ways a host can give the adapter its HTTP client.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func (w *world) clients() map[string]ports.HTTPClient {
	noFollow := &http.Client{Transport: w, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return map[string]ports.HTTPClient{
		// The default policy follows 10 redirects on its own.
		"std client": &http.Client{Transport: w},
		// A host client of another type that never follows.
		"non-following client": doerFunc(noFollow.Do),
	}
}

// oneMedia is the golden MMS with its first media URL only, on origin.
func oneMedia(t *testing.T, origin string) []byte {
	t.Helper()
	form, err := url.ParseQuery(string(mms(t, origin)))
	if err != nil {
		t.Fatal(err)
	}
	form.Set("NumMedia", "1")
	form.Del("MediaUrl1")
	form.Del("MediaContentType1")
	return []byte(form.Encode())
}

func serveImage(rw http.ResponseWriter, _ *http.Request) {
	rw.Header().Set("Content-Type", "image/jpeg")
	_, _ = rw.Write([]byte("\xff\xd8\xff\xe0 jpeg"))
}

func statusOf(code int) http.HandlerFunc {
	return func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(code)
		_, _ = rw.Write([]byte(`{"code":20404,"message":"x","status":` + fmt.Sprint(code) + `}`))
	}
}

func redirectTo(code int, target string) http.HandlerFunc {
	return func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Location", target)
		rw.WriteHeader(code)
	}
}

func cdnURL(path string) string { return "https://" + cdnHost + path }

// hang answers nothing until the client gives up, then drops the
// connection. It never returns normally: the server would answer a handler
// that returns with an empty 200, which a client racing its own deadline
// can read as a success.
func hang(_ http.ResponseWriter, r *http.Request) {
	<-r.Context().Done()
	panic(http.ErrAbortHandler)
}

const (
	// patient bounds a download that must not time out: far beyond what a
	// loaded machine needs for a TLS handshake and two local round trips.
	patient = 30 * time.Second
	// impatient bounds one that is meant to time out; every way it can
	// expire ends in a retry, so only the time the case takes depends on it.
	impatient = 100 * time.Millisecond
)

// reset drops the connection mid-request.
func reset(rw http.ResponseWriter, _ *http.Request) {
	c, _, err := rw.(http.Hijacker).Hijack()
	if err == nil {
		_ = c.Close()
	}
}

// gapsOf returns the media the message reports as unavailable.
func gapsOf(t *testing.T, msg model.Message) map[int]string {
	t.Helper()
	out := map[int]string{}
	for _, p := range msg.Content.Parts {
		if p.Kind != model.PartStructured || p.Structured == nil || p.Structured.Name != sms.PayloadMedia {
			continue
		}
		var m struct {
			Unavailable []struct {
				Index  int    `json:"index"`
				Reason string `json:"reason"`
			} `json:"unavailable"`
		}
		if err := json.Unmarshal(p.Structured.Data, &m); err != nil {
			t.Fatal(err)
		}
		for _, g := range m.Unavailable {
			out[g.Index] = g.Reason
		}
	}
	return out
}

func attachmentCount(msg model.Message) int {
	n := 0
	for _, p := range msg.Content.Parts {
		if p.Kind == model.PartAttachment && p.Attachment != nil {
			n++
		}
	}
	return n
}

// What a media download does for each answer of the API origin or of the
// store it redirects to. A failure a redelivery may fix answers 503, before
// the engine sees the message. A media that is gone for good, or cannot be
// fetched at all, leaves the message with its text and a summary that names
// the media unavailable and why, so the loss is never silent.
func TestMediaDownloadOutcomes(t *testing.T) {
	const (
		gone, denied, refused, failed = "gone", "unauthorized", "redirect", "failed"
		retry                         = "retry"
		staged                        = ""
	)
	secret := func(w *world) string { return w.secret.URL + "/latest/meta-data" }
	cases := []struct {
		name string
		api  func(w *world) http.HandlerFunc
		cdn  http.HandlerFunc
		want string // staged, a gap reason, or retry
	}{
		{"served directly", func(*world) http.HandlerFunc { return serveImage }, nil, staged},

		// Redirects to the media store are followed, without the credential.
		{"redirect 301", func(*world) http.HandlerFunc { return redirectTo(301, cdnURL("/m/1")) }, serveImage, staged},
		{"redirect 302", func(*world) http.HandlerFunc { return redirectTo(302, cdnURL("/m/1")) }, serveImage, staged},
		{"redirect 303", func(*world) http.HandlerFunc { return redirectTo(303, cdnURL("/m/1")) }, serveImage, staged},
		{"redirect 307", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/m/1")) }, serveImage, staged},
		{"redirect 308", func(*world) http.HandlerFunc { return redirectTo(308, cdnURL("/m/1")) }, serveImage, staged},
		{"three redirects", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/h/1")) }, func(rw http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/h/1":
				redirectTo(307, cdnURL("/h/2"))(rw, r)
			case "/h/2":
				redirectTo(307, cdnURL("/h/3"))(rw, r)
			default:
				serveImage(rw, r)
			}
		}, staged},
		{"four redirects", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/h/1")) }, func(rw http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/h/1":
				redirectTo(307, cdnURL("/h/2"))(rw, r)
			case "/h/2":
				redirectTo(307, cdnURL("/h/3"))(rw, r)
			case "/h/3":
				redirectTo(307, cdnURL("/h/4"))(rw, r)
			default:
				serveImage(rw, r)
			}
		}, refused},
		{"redirect loop at the store", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/loop")) }, redirectTo(307, cdnURL("/loop")), refused},
		{"redirect loop at the API", func(w *world) http.HandlerFunc { return redirectTo(307, w.api.URL+mediaBase+mediaOne) }, nil, refused},

		// Redirects to somewhere the credential or the host must not go.
		{"redirect to loopback", func(w *world) http.HandlerFunc { return redirectTo(307, secret(w)) }, nil, refused},
		{"redirect to loopback by address", func(*world) http.HandlerFunc { return redirectTo(307, "https://127.0.0.1/") }, nil, refused},
		{"redirect to loopback by name", func(*world) http.HandlerFunc { return redirectTo(307, "https://localhost/") }, nil, refused},
		{"redirect to IPv6 loopback", func(*world) http.HandlerFunc { return redirectTo(307, "https://[::1]/") }, nil, refused},
		{"redirect to metadata address", func(*world) http.HandlerFunc { return redirectTo(307, "https://169.254.169.254/latest/meta-data/") }, nil, refused},
		{"redirect to private address", func(*world) http.HandlerFunc { return redirectTo(307, "https://10.0.0.5/") }, nil, refused},
		{"redirect to a decimal address", func(*world) http.HandlerFunc { return redirectTo(307, "https://2130706433/") }, nil, refused},
		{"redirect to a shared address", func(*world) http.HandlerFunc { return redirectTo(307, "https://100.64.0.1/") }, nil, refused},
		{"redirect to a documentation address", func(*world) http.HandlerFunc { return redirectTo(307, "https://192.0.2.10/") }, nil, refused},
		{"redirect to an RFC 1918 address", func(*world) http.HandlerFunc { return redirectTo(307, "https://192.168.1.1/") }, nil, refused},
		{"redirect to 0.0.0.0", func(*world) http.HandlerFunc { return redirectTo(307, "https://0.0.0.0/") }, nil, refused},
		{"redirect to a multicast address", func(*world) http.HandlerFunc { return redirectTo(307, "https://224.0.0.1/") }, nil, refused},
		{"redirect to a mapped loopback address", func(*world) http.HandlerFunc { return redirectTo(307, "https://[::ffff:127.0.0.1]/") }, nil, refused},
		{"redirect to an IPv6 link-local address", func(*world) http.HandlerFunc { return redirectTo(307, "https://[fe80::1]/") }, nil, refused},
		{"redirect to an IPv6 unique local address", func(*world) http.HandlerFunc { return redirectTo(307, "https://[fd00:ec2::254]/") }, nil, refused},
		{"redirect to a NAT64 address", func(*world) http.HandlerFunc { return redirectTo(307, "https://[64:ff9b::7f00:1]/") }, nil, refused},
		{"redirect to a single-label name", func(*world) http.HandlerFunc { return redirectTo(307, "https://intranet/") }, nil, refused},
		{"redirect to a hexadecimal address", func(*world) http.HandlerFunc { return redirectTo(307, "https://0x7f.1/") }, nil, refused},
		{"redirect to a .internal name", func(*world) http.HandlerFunc { return redirectTo(307, "https://metadata.internal/") }, nil, refused},
		{"redirect to a .local name", func(*world) http.HandlerFunc { return redirectTo(307, "https://printer.local/") }, nil, refused},
		{"redirect to a mapped documentation address", func(*world) http.HandlerFunc { return redirectTo(307, "https://[::ffff:192.0.2.1]/") }, nil, refused},
		{"redirect to a zoned address", func(*world) http.HandlerFunc { return redirectTo(307, "https://[2606:4700::1%25eth0]/") }, nil, refused},
		{"redirect to plain http", func(*world) http.HandlerFunc { return redirectTo(307, "http://"+cdnHost+"/m/1") }, serveImage, refused},
		{"redirect to another port", func(*world) http.HandlerFunc { return redirectTo(307, "https://"+cdnHost+":8443/m/1") }, serveImage, refused},
		{"redirect with credentials in the URL", func(*world) http.HandlerFunc { return redirectTo(307, "https://u:p@"+cdnHost+"/m/1") }, serveImage, refused},
		{"redirect without Location", func(*world) http.HandlerFunc { return statusOf(307) }, nil, refused},
		{"not modified", func(*world) http.HandlerFunc { return statusOf(304) }, nil, refused},

		// Answers of the API origin, which got the credential.
		{"API 401", func(*world) http.HandlerFunc { return statusOf(401) }, nil, denied},
		{"API 403", func(*world) http.HandlerFunc { return statusOf(403) }, nil, denied},
		{"API 404", func(*world) http.HandlerFunc { return statusOf(404) }, nil, gone},
		{"API 410", func(*world) http.HandlerFunc { return statusOf(410) }, nil, gone},
		{"API 400", func(*world) http.HandlerFunc { return statusOf(400) }, nil, failed},
		{"API 405", func(*world) http.HandlerFunc { return statusOf(405) }, nil, failed},
		{"API 408", func(*world) http.HandlerFunc { return statusOf(408) }, nil, retry},
		{"API 429", func(*world) http.HandlerFunc { return statusOf(429) }, nil, retry},
		{"API 500", func(*world) http.HandlerFunc { return statusOf(500) }, nil, retry},
		{"API 502", func(*world) http.HandlerFunc { return statusOf(502) }, nil, retry},
		{"API 503", func(*world) http.HandlerFunc { return statusOf(503) }, nil, retry},
		{"API 504", func(*world) http.HandlerFunc { return statusOf(504) }, nil, retry},
		{"API timeout", func(*world) http.HandlerFunc { return hang }, nil, retry},
		{"API connection reset", func(*world) http.HandlerFunc { return reset }, nil, retry},

		// Answers of the store, which got no credential: a refusal there is
		// not a wrong credential.
		{"store 401", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/m/1")) }, statusOf(401), failed},
		{"store 403", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/m/1")) }, statusOf(403), failed},
		{"store 404", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/m/1")) }, statusOf(404), gone},
		{"store 410", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/m/1")) }, statusOf(410), gone},
		{"store 429", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/m/1")) }, statusOf(429), retry},
		{"store 500", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/m/1")) }, statusOf(500), retry},
		{"store timeout", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/m/1")) }, hang, retry},
		{"store connection reset", func(*world) http.HandlerFunc { return redirectTo(307, cdnURL("/m/1")) }, reset, retry},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, name := range []string{"std client", "non-following client"} {
				t.Run(name, func(t *testing.T) {
					w := newWorld(t)
					w.apiFn, w.cdnFn = c.api(w), c.cdn
					if w.cdnFn == nil {
						w.cdnFn = serveImage
					}
					timeout := patient
					if strings.HasSuffix(c.name, " timeout") {
						timeout = impatient
					}
					h := newMMSHostOn(t, w.api.URL, w.clients()[name], timeout)
					body := oneMedia(t, w.api.URL)
					rec := post(h.handler(), "", body, formType, signTwilio(hookToken, hookURL, body))
					msgs := h.messages()

					if c.want == retry {
						if rec.Code != http.StatusServiceUnavailable || len(msgs) != 0 {
							t.Fatalf("status %d with %d messages, want 503 and none", rec.Code, len(msgs))
						}
					} else {
						if rec.Code != http.StatusOK || len(msgs) != 1 {
							t.Fatalf("status %d with %d messages, want 200 and one", rec.Code, len(msgs))
						}
						gaps := gapsOf(t, msgs[0])
						switch c.want {
						case staged:
							if attachmentCount(msgs[0]) != 1 || len(gaps) != 0 {
								t.Fatalf("parts %+v, want the media staged", msgs[0].Content.Parts)
							}
						default:
							if attachmentCount(msgs[0]) != 0 || len(gaps) != 1 || gaps[0] != c.want {
								t.Fatalf("attachments %d, unavailable %v, want media 0 %q", attachmentCount(msgs[0]), gaps, c.want)
							}
						}
					}
					// Whatever the answer, nothing reached a place with no
					// route and the credential stayed on the API origin.
					if len(w.stray) != 0 || w.secrets != 0 {
						t.Fatalf("stray requests %v, secret service hit %d times", w.stray, w.secrets)
					}
					for _, s := range w.calls("cdn") {
						if s.Auth != "" {
							t.Fatalf("store request %s carried a credential", s.Path)
						}
					}
					for _, s := range w.calls("api") {
						if !strings.HasPrefix(s.Auth, "Basic ") {
							t.Fatalf("API request %s carried no credential", s.Path)
						}
					}
					if n := len(w.calls("cdn")); n > 4 {
						t.Fatalf("store saw %d requests, want a bounded few", n)
					}
				})
			}
		})
	}
}

// A redirect to the API origin itself keeps the credential, and only
// there; one that leaves the origin never gets it back.
func TestMediaRedirectWithinTheAPIOrigin(t *testing.T) {
	w := newWorld(t)
	moved := "/moved" + mediaBase + mediaOne
	w.apiFn = func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == moved {
			serveImage(rw, r)
			return
		}
		redirectTo(307, moved)(rw, r)
	}
	h := newMMSHostOn(t, w.api.URL, w.clients()["std client"], patient)
	body := oneMedia(t, w.api.URL)
	if rec := post(h.handler(), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	msgs := h.messages()
	if len(msgs) != 1 || attachmentCount(msgs[0]) != 1 {
		t.Fatalf("messages %+v, want the media staged", msgs)
	}
	calls := w.calls("api")
	if len(calls) != 2 || !strings.HasPrefix(calls[1].Auth, "Basic ") {
		t.Fatalf("API calls %+v, want both with the credential", calls)
	}

	// Away and back: the way back carries none, so the API refuses it.
	w = newWorld(t)
	w.apiFn = func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			statusOf(401)(rw, r)
			return
		}
		redirectTo(307, cdnURL("/back"))(rw, r)
	}
	w.cdnFn = redirectTo(307, w.api.URL+mediaBase+mediaOne)
	h = newMMSHostOn(t, w.api.URL, w.clients()["std client"], patient)
	body = oneMedia(t, w.api.URL)
	if rec := post(h.handler(), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	msgs = h.messages()
	if len(msgs) != 1 || attachmentCount(msgs[0]) != 0 || gapsOf(t, msgs[0])[0] != "failed" {
		t.Fatalf("messages %+v, want the media unavailable", msgs)
	}
	for k, s := range w.calls("api") {
		if k > 0 && s.Auth != "" {
			t.Fatalf("the credential came back with the redirect: %+v", w.calls("api"))
		}
	}
}

// A credential Twilio refuses is a configuration problem every media of
// the message shares: it is reported for all of them after one request,
// not retried per media.
func TestMediaCredentialRefusedStopsTheMessage(t *testing.T) {
	w := newWorld(t)
	w.apiFn = statusOf(401)
	h := newMMSHostOn(t, w.api.URL, w.clients()["std client"], patient)
	body := mms(t, w.api.URL)
	if rec := post(h.handler(), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	msgs := h.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	gaps := gapsOf(t, msgs[0])
	if len(gaps) != 2 || gaps[0] != "unauthorized" || gaps[1] != "unauthorized" {
		t.Fatalf("unavailable %v, want both media unauthorized", gaps)
	}
	if n := len(w.calls("api")); n != 1 {
		t.Fatalf("API requests = %d, want 1", n)
	}
	// The summary keeps what it said before.
	for _, p := range msgs[0].Content.Parts {
		if p.Structured != nil && p.Structured.Name == sms.PayloadMedia {
			var m sms.Media
			if err := json.Unmarshal(p.Structured.Data, &m); err != nil || m.Count != 2 || len(m.ContentTypes) != 2 || len(m.Unavailable) != 2 {
				t.Fatalf("media summary %+v, %v", m, err)
			}
		}
	}
}
