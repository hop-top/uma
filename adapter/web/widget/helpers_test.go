package widget_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

const tenant model.TenantID = "widget-tenant"

var (
	chanA = model.ChannelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000a1"}
	chanB = model.ChannelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000b1"}
	keyA  = []byte("0123456789abcdef0123456789abcdef")
)

// keyHost is the host credential port: one session key per channel, which
// a test can rotate or revoke.
type keyHost struct {
	mu      sync.Mutex
	keys    map[model.ChannelRef][]byte
	revoked bool
	reqs    []ports.CredentialRequest
}

func newKeyHost() *keyHost {
	return &keyHost{keys: map[model.ChannelRef][]byte{chanA: keyA, chanB: []byte("fedcba9876543210fedcba9876543210")}}
}

func (h *keyHost) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqs = append(h.reqs, r)
	if h.revoked {
		return nil, model.NewError(model.Unauthorized, "revoked")
	}
	if r.Purpose != widget.CredentialPurpose || !r.Accepts(ports.KindSecret) {
		return nil, model.NewError(model.Unsupported, "unexpected request")
	}
	return ports.Secret{Bytes: h.keys[r.Channel]}, nil
}

func (h *keyHost) set(ch model.ChannelRef, key []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keys[ch] = key
}

func (h *keyHost) revoke() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.revoked = true
}

// fakeSink records what reaches the engine side.
type fakeSink struct {
	mu           sync.Mutex
	ingress      []model.Ingress
	seen         map[string]bool
	interactions []model.InteractionInput
	acceptErr    error
}

func (s *fakeSink) Accept(_ context.Context, in model.Ingress) (model.IngressReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acceptErr != nil {
		return model.IngressReceipt{}, s.acceptErr
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	id := in.Envelope.MessageID
	if id == "" {
		id = in.Envelope.EventID
	}
	dup := s.seen[id]
	s.seen[id] = true
	s.ingress = append(s.ingress, in)
	return model.IngressReceipt{Committed: !dup, Duplicate: dup}, nil
}

func (s *fakeSink) EvaluateInteraction(_ context.Context, in model.InteractionInput) (model.InteractionDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interactions = append(s.interactions, in)
	return model.InteractionDecision{FieldErrors: map[string]string{"email": "required"}}, nil
}

func (s *fakeSink) received() []model.Ingress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Ingress(nil), s.ingress...)
}

func (s *fakeSink) evaluated() []model.InteractionInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.InteractionInput(nil), s.interactions...)
}

// headerVerifier is the host session verifier of the tests: the visitor
// is what the X-Visitor, X-Assurance and X-Site headers say.
var headerVerifier = widget.VerifierFunc(func(r *http.Request, _ model.ChannelRef) (widget.Visitor, error) {
	if r.Header.Get("X-Refuse") != "" {
		return widget.Visitor{}, model.NewError(model.Unauthorized, "not logged in")
	}
	return widget.Visitor{ID: r.Header.Get("X-Visitor"), Assurance: r.Header.Get("X-Assurance"), Site: r.Header.Get("X-Site")}, nil
})

// rig is one widget channel served over httptest: the receiver under
// /widget/ and the event stream at /widget/events.
type rig struct {
	t     *testing.T
	ch    model.ChannelRef
	clock *portstest.FixedClock
	keys  *keyHost
	sink  *fakeSink
	f     *widget.Factory
	inst  adapter.Instance
	srv   *httptest.Server
}

func newRig(t *testing.T, opts widget.Options) *rig {
	t.Helper()
	if opts.Verifier == nil {
		opts.Verifier = headerVerifier
	}
	f, err := widget.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{
		t: t, ch: chanA, clock: portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC()),
		keys: newKeyHost(), sink: &fakeSink{}, f: f,
	}
	r.inst = r.open(chanA, widget.Configuration("support", "sales"))
	mux := http.NewServeMux()
	mux.Handle("/widget/", r.inst.(adapter.HTTPReceiver).Handler(r.sink))
	mux.Handle("/widget/events", f.Events(chanA))
	r.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		f.Close()
		r.srv.Close()
	})
	return r
}

func (r *rig) open(ch model.ChannelRef, cfg model.Extension) adapter.Instance {
	r.t.Helper()
	inst, err := r.f.Open(r.t.Context(), model.Channel{
		Ref: ch, Type: widget.Type, Configuration: cfg, CredentialRef: "widget-key", Revision: "r1",
	}, adapter.Dependencies{Credentials: r.keys, Clock: r.clock})
	if err != nil {
		r.t.Fatalf("Open: %v", err)
	}
	r.t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
}

// post sends a JSON POST and returns the status and body.
func (r *rig) post(path, token string, body any, header ...string) (int, string) {
	r.t.Helper()
	var b []byte
	switch v := body.(type) {
	case nil:
		b = []byte("{}")
	case []byte:
		b = v
	default:
		var err error
		if b, err = json.Marshal(v); err != nil {
			r.t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodPost, r.srv.URL+path, bytes.NewReader(b))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := r.srv.Client().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

// session opens a session for visitor id under assurance and returns it.
func (r *rig) session(id, assurance string, header ...string) widget.SessionResponse {
	r.t.Helper()
	code, body := r.post("/widget/session", "", nil, append([]string{"X-Visitor", id, "X-Assurance", assurance, "X-Site", "support"}, header...)...)
	if code != http.StatusOK {
		r.t.Fatalf("session = %d %s", code, body)
	}
	var s widget.SessionResponse
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *rig) message(token, id, text string) int {
	r.t.Helper()
	code, _ := r.post("/widget/messages", token, widget.MessageRequest{ID: id, Text: text})
	return code
}

// sse is one open event stream.
type sse struct {
	events chan sseEvent
	cancel context.CancelFunc
	done   chan struct{}
}

type sseEvent struct {
	ID, Name string
	Data     widget.Event
	Expires  *time.Time
}

// connect opens the event stream with token and the given cursor header
// (empty for none) and checks the status.
func (r *rig) connect(token, lastEventID string) *sse {
	r.t.Helper()
	s, code := r.dial(token, lastEventID)
	if code != http.StatusOK {
		r.t.Fatalf("event stream = %d, want 200", code)
	}
	return s
}

// dial opens the event stream and returns the status; s is nil unless
// the status is 200.
func (r *rig) dial(token, lastEventID string) (*sse, int) {
	r.t.Helper()
	ctx, cancel := context.WithCancel(r.t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.srv.URL+"/widget/events?token="+url.QueryEscape(token), nil)
	if err != nil {
		r.t.Fatal(err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	res, err := r.srv.Client().Do(req)
	if err != nil {
		cancel()
		r.t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		cancel()
		return nil, res.StatusCode
	}
	s := &sse{events: make(chan sseEvent, 64), cancel: cancel, done: make(chan struct{})}
	go s.read(res.Body)
	r.t.Cleanup(s.close)
	return s, http.StatusOK
}

func (s *sse) close() {
	s.cancel()
	<-s.done
}

func (s *sse) read(body io.ReadCloser) {
	defer close(s.done)
	defer close(s.events)
	defer func() { _ = body.Close() }()
	sc := bufio.NewScanner(body)
	var ev sseEvent
	var data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data != "" {
				var we struct {
					widget.Event
					ExpiresAt *time.Time `json:"expires_at"`
				}
				_ = json.Unmarshal([]byte(data), &we)
				ev.Data, ev.Expires = we.Event, we.ExpiresAt
				s.events <- ev
			}
			ev, data = sseEvent{}, ""
		case strings.HasPrefix(line, "id: "):
			ev.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			ev.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// next returns the next event, failing after a timeout.
func (s *sse) next(t *testing.T) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			t.Fatal("event stream ended")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
	}
	return sseEvent{}
}

// ended waits until the stream closes, skipping events.
func (s *sse) ended(t *testing.T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-s.events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("event stream still open after 5s")
		}
	}
}

// route is the visitor route of an anonymous visitor or verified user.
func route(ch model.ChannelRef, s widget.SessionResponse) model.ExternalRef {
	if s.Assurance == widget.AssuranceVerified {
		return model.ExternalRef{Channel: ch, Namespace: widget.NamespaceUser, Kind: widget.KindUser, ID: s.Visitor}
	}
	return model.ExternalRef{Channel: ch, Namespace: widget.NamespaceVisitor, Kind: widget.KindVisitor, ID: s.Visitor}
}

func text(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

func (r *rig) send(to model.ExternalRef, s string) adapter.Outcome {
	r.t.Helper()
	out, err := r.inst.(adapter.Sender).Send(r.t.Context(), adapter.SendRequest{
		Effect:  adapter.Effect{Route: to, Audience: model.Audience{Visibility: model.Public}},
		Content: text(s),
	})
	if err != nil {
		r.t.Fatalf("Send: %v", err)
	}
	return out
}

func onlyText(e widget.Event) string {
	if len(e.Parts) != 1 {
		return ""
	}
	return e.Parts[0].Text
}
