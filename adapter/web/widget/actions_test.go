package widget_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

// sourceMessage has the shape of a message the widget sent.
const sourceMessage = "m0123456789abcdef0123456789abcdef"

var feedbackForm = model.Extension{Name: "org.example.feedback", Version: 1, Data: json.RawMessage(`{"email":"a@b.c"}`)}

// gateSink holds Accept until released, so a test sees whether the
// receiver answers before the engine accepted.
type gateSink struct {
	*fakeSink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	opened  sync.Once
}

// newGateSink returns a sink that holds Accept until open. The test's end
// opens it, so a failing test never leaves a handler blocked.
func newGateSink(t *testing.T) *gateSink {
	g := &gateSink{fakeSink: &fakeSink{}, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gateSink) open() { g.opened.Do(func() { close(g.release) }) }

func (g *gateSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return g.fakeSink.Accept(ctx, in)
}

// answer is the response to a request in flight: its status arrives with
// the response headers, so an early answer shows even while the handler
// still runs, and its body once the handler returned.
type answer struct {
	status chan int
	body   chan string
}

// wait returns the status and body of the finished response.
func (a answer) wait() (int, string) {
	code := <-a.status
	return code, <-a.body
}

// postAsync posts to the instance's handler served over sink.
func postAsync(t *testing.T, r *rig, sink adapter.Sink, path, token string, body any) answer {
	t.Helper()
	srv := httptest.NewServer(r.inst.(adapter.HTTPReceiver).Handler(sink))
	t.Cleanup(func() {
		if g, ok := sink.(*gateSink); ok {
			g.open() // a failed test must not leave a handler blocked in Accept
		}
		srv.Close()
	})
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	a := answer{status: make(chan int, 1), body: make(chan string, 1)}
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := srv.Client().Do(req)
		if err != nil {
			a.status <- -1
			a.body <- err.Error()
			return
		}
		defer func() { _ = res.Body.Close() }()
		a.status <- res.StatusCode
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(res.Body)
		a.body <- buf.String()
	}()
	return a
}

func TestActionsAcceptAfterDurable(t *testing.T) {
	r := newRig(t, widget.Options{InteractionTimeout: 3 * time.Second})
	s := r.session("", widget.AssuranceAnonymous)
	gate := newGateSink(t)
	pending := postAsync(t, r, gate, "/widget/actions", s.Token, widget.ActionRequest{
		ID: "a-1", Action: "approve", Value: "yes", Message: sourceMessage,
	})
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the receiver never handed the interaction to the engine")
	}
	select {
	case code := <-pending.status:
		t.Fatalf("answered %d before the engine accepted the interaction: the provider would be acknowledged without a durable record", code)
	case <-time.After(150 * time.Millisecond):
	}
	gate.open()
	code, body := pending.wait()
	if code != http.StatusAccepted {
		t.Fatalf("action = %d %s, want 202", code, body)
	}
	var res widget.AcceptedInteraction
	if err := json.Unmarshal([]byte(body), &res); err != nil || res.Interaction == "" || res.Duplicate {
		t.Fatalf("response = %s", body)
	}

	got := gate.received()
	if len(got) != 1 {
		t.Fatalf("ingress = %d", len(got))
	}
	env := got[0].Envelope
	in := env.Interaction
	wantSender := model.ExternalRef{Channel: chanA, Namespace: widget.NamespaceVisitor, Kind: widget.KindVisitor, ID: s.Visitor}
	wantRoom := model.ExternalRef{Channel: chanA, Namespace: widget.NamespaceVisitor, Kind: model.RefKindRoom, ID: s.Session, ParentID: s.Visitor}
	switch {
	case env.Kind != model.EnvelopeInteraction || in == nil:
		t.Fatalf("envelope = %+v, want an interaction envelope", env)
	case env.Sender != wantSender || in.Actor != wantSender:
		t.Errorf("sender = %+v, actor = %+v, want the session's visitor %+v", env.Sender, in.Actor, wantSender)
	case len(env.References) != 1 || env.References[0] != wantRoom:
		t.Errorf("references = %+v, want the session room", env.References)
	case len(in.Context) != 1 || in.Context[0] != wantRoom:
		t.Errorf("context = %+v, want the session room", in.Context)
	case env.EventID != res.Interaction || in.ID != res.Interaction || env.EventID == "a-1":
		t.Errorf("event %q interaction %q answered %q, want the visitor-scoped ID throughout", env.EventID, in.ID, res.Interaction)
	case env.MessageID != "" || len(env.Content.Parts) != 0:
		t.Errorf("envelope names message %q with content %+v: an interaction is no message", env.MessageID, env.Content)
	case in.Kind != model.InteractionAction || in.Action != "approve" || in.Arguments != "yes":
		t.Errorf("interaction = %+v, want the approve action with its value", in)
	case in.Message == nil || *in.Message != model.ProviderMessageRef(chanA, sourceMessage):
		t.Errorf("message = %+v, want the source message", in.Message)
	case in.ResponseToken != "" || !in.ResponseExpiresAt.IsZero():
		t.Errorf("interaction carries a response token %q expiring %v: the widget has none", in.ResponseToken, in.ResponseExpiresAt)
	case !in.Deadline.Equal(r.clock.Now().Add(3 * time.Second)):
		t.Errorf("deadline = %v, want the interaction timeout", in.Deadline)
	case env.Destination != "support" || env.Audience.Visibility != model.Public || env.Assurance != widget.AssuranceAnonymous:
		t.Errorf("envelope = %+v", env)
	case !env.OccurredAt.Equal(r.clock.Now()):
		t.Errorf("occurred_at = %v", env.OccurredAt)
	}
	if err := in.Validate(); err != nil {
		t.Errorf("interaction does not validate: %v", err)
	}

	// A retried press is the same interaction, answered as a duplicate.
	again := postAsync(t, r, gate, "/widget/actions", s.Token, widget.ActionRequest{ID: "a-1", Action: "approve", Value: "yes", Message: sourceMessage})
	if code, body := again.wait(); code != http.StatusAccepted || !strings.Contains(body, `"duplicate":true`) || !strings.Contains(body, res.Interaction) {
		t.Fatalf("retried action = %d %s, want the same interaction as a duplicate", code, body)
	}
}

func TestActionsCommandKind(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	code, body := r.post("/widget/actions", s.Token, widget.ActionRequest{ID: "c-1", Kind: model.InteractionCommand, Command: "help", Arguments: "me"})
	if code != http.StatusAccepted {
		t.Fatalf("command = %d %s", code, body)
	}
	in := r.sink.received()[0].Envelope.Interaction
	if in.Kind != model.InteractionCommand || in.Command != "help" || in.Arguments != "me" || in.Action != "" || in.Message != nil {
		t.Fatalf("interaction = %+v, want the help command with its arguments and no source message", in)
	}
	if in.Actor.Namespace != widget.NamespaceUser || in.Actor.ID != "alice" {
		t.Fatalf("actor = %+v, want the verified visitor", in.Actor)
	}
}

func TestSubmissionsDurable(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("", widget.AssuranceAnonymous)
	gate := newGateSink(t)
	pending := postAsync(t, r, gate, "/widget/submissions", s.Token, widget.SubmissionRequest{ID: "f-1", Fields: feedbackForm, Message: sourceMessage})
	<-gate.entered
	select {
	case code := <-pending.status:
		t.Fatalf("answered %d before the engine accepted the submission", code)
	case <-time.After(150 * time.Millisecond):
	}
	gate.open()
	if code, body := pending.wait(); code != http.StatusAccepted {
		t.Fatalf("submission = %d %s, want 202", code, body)
	}
	env := gate.received()[0].Envelope
	in := env.Interaction
	if env.Kind != model.EnvelopeInteraction || in.Kind != model.InteractionSubmission || in.Fields.Name != feedbackForm.Name ||
		string(in.Fields.Data) != string(feedbackForm.Data) || in.Action != "" || in.Command != "" {
		t.Fatalf("envelope = %+v, want a submission of the feedback form", env)
	}
	if in.Message == nil || *in.Message != model.ProviderMessageRef(chanA, sourceMessage) {
		t.Fatalf("message = %+v, want the form's message", in.Message)
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("interaction does not validate: %v", err)
	}
}

func TestInteractionsStillEvaluateOnly(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("", widget.AssuranceAnonymous)
	code, body := r.post("/widget/interactions", s.Token, widget.InteractionRequest{ID: "e-1", Kind: model.InteractionSubmission, Fields: feedbackForm})
	if code != http.StatusOK {
		t.Fatalf("interaction = %d %s, want 200", code, body)
	}
	if n := len(r.sink.received()); n != 0 {
		t.Fatalf("ingress = %d: the evaluate path must not accept the interaction", n)
	}
	if n := len(r.sink.evaluated()); n != 1 {
		t.Fatalf("evaluations = %d, want 1", n)
	}
	r.post("/widget/actions", s.Token, widget.ActionRequest{ID: "e-2", Action: "go"})
	if n := len(r.sink.evaluated()); n != 1 {
		t.Fatalf("evaluations = %d after an action: accepting evaluates nothing", n)
	}
}

func TestActionForeignSessionRejected(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	flipped := s.Token[:len(s.Token)-2] + "xx"
	for name, token := range map[string]string{"none": "", "garbage": "v1.x.y", "forged": flipped} {
		for _, path := range []string{"/widget/actions", "/widget/submissions"} {
			if code, _ := r.post(path, token, widget.ActionRequest{ID: "1", Action: "go"}); code != http.StatusUnauthorized {
				t.Errorf("%s token on %s = %d, want 401", name, path, code)
			}
		}
	}
	r.clock.Advance(2 * widget.DefaultSessionTTL)
	for _, path := range []string{"/widget/actions", "/widget/submissions"} {
		if code, _ := r.post(path, s.Token, widget.ActionRequest{ID: "1", Action: "go"}); code != http.StatusUnauthorized {
			t.Errorf("expired token on %s = %d, want 401", path, code)
		}
	}
	if n := len(r.sink.received()); n != 0 {
		t.Fatalf("ingress = %d, want none: nothing reaches the engine before the token verifies", n)
	}

	// The actor is the session's, never a field of the body.
	fresh := r.session("alice", widget.AssuranceVerified)
	for _, body := range []string{
		`{"id":"1","action":"go","actor":"bob"}`,
		`{"id":"1","action":"go","sender":{"id":"bob"}}`,
	} {
		if code, _ := r.post("/widget/actions", fresh.Token, []byte(body)); code != http.StatusBadRequest {
			t.Errorf("body %s = %d, want 400", body, code)
		}
	}
}

func TestActionsRefuseMalformedRequests(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	for name, c := range map[string]struct {
		path string
		body any
	}{
		"no ID":                  {"/widget/actions", widget.ActionRequest{Action: "go"}},
		"action without action":  {"/widget/actions", widget.ActionRequest{ID: "1"}},
		"command without name":   {"/widget/actions", widget.ActionRequest{ID: "1", Kind: model.InteractionCommand}},
		"options kind":           {"/widget/actions", widget.ActionRequest{ID: "1", Kind: model.InteractionOptions, Action: "go"}},
		"submission kind":        {"/widget/actions", widget.ActionRequest{ID: "1", Kind: model.InteractionSubmission, Action: "go"}},
		"value on a command":     {"/widget/actions", widget.ActionRequest{ID: "1", Kind: model.InteractionCommand, Command: "help", Value: "x"}},
		"arguments on an action": {"/widget/actions", widget.ActionRequest{ID: "1", Action: "go", Arguments: "x"}},
		"action with a command":  {"/widget/actions", widget.ActionRequest{ID: "1", Action: "go", Command: "help"}},
		"message of no shape":    {"/widget/actions", widget.ActionRequest{ID: "1", Action: "go", Message: "not a message"}},
		"unnamed fields":         {"/widget/submissions", widget.SubmissionRequest{ID: "1"}},
		"unversioned fields":     {"/widget/submissions", widget.SubmissionRequest{ID: "1", Fields: model.Extension{Name: "org.example.feedback"}}},
		"submission no ID":       {"/widget/submissions", widget.SubmissionRequest{Fields: feedbackForm}},
		"submission bad message": {"/widget/submissions", widget.SubmissionRequest{ID: "1", Fields: feedbackForm, Message: "x"}},
		"unknown field":          {"/widget/submissions", []byte(`{"id":"1","fields":{"name":"org.example.feedback","version":1},"token":"t"}`)},
	} {
		if code, _ := r.post(c.path, s.Token, c.body); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	if n := len(r.sink.received()); n != 0 {
		t.Fatalf("ingress = %d, want none", n)
	}
}

func TestActionsSinkFailures(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	for code, want := range map[model.ErrorCode]int{
		model.Transient:       http.StatusServiceUnavailable,
		model.Validation:      http.StatusUnprocessableEntity,
		model.DeadlineExpired: http.StatusGatewayTimeout,
	} {
		r.sink.mu.Lock()
		r.sink.acceptErr = model.NewError(code, "refused")
		r.sink.mu.Unlock()
		for path, body := range map[string]any{
			"/widget/actions":     widget.ActionRequest{ID: "1", Action: "go"},
			"/widget/submissions": widget.SubmissionRequest{ID: "1", Fields: feedbackForm},
		} {
			if got, text := r.post(path, s.Token, body); got != want || strings.TrimSpace(text) != http.StatusText(want) {
				t.Errorf("%s sink error on %s = %d %q, want %d with the status text only", code, path, got, text, want)
			}
		}
	}
}
