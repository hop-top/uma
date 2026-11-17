package widget_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

func TestReceiverMessageNormalization(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	code, body := r.post("/widget/messages", s.Token, widget.MessageRequest{ID: "c-1", Text: "hello"})
	if code != http.StatusAccepted {
		t.Fatalf("message = %d %s", code, body)
	}
	var res widget.MessageResponse
	if err := json.Unmarshal([]byte(body), &res); err != nil || res.Message == "" || res.Duplicate {
		t.Fatalf("response = %s", body)
	}
	got := r.sink.received()
	if len(got) != 1 {
		t.Fatalf("ingress = %d", len(got))
	}
	env := got[0].Envelope
	wantSender := model.ExternalRef{Channel: chanA, Namespace: widget.NamespaceUser, Kind: widget.KindUser, ID: "alice"}
	wantRoom := model.ExternalRef{Channel: chanA, Namespace: widget.NamespaceUser, Kind: model.RefKindRoom, ID: s.Session, ParentID: "alice"}
	switch {
	case env.Sender != wantSender:
		t.Errorf("sender = %+v, want %+v", env.Sender, wantSender)
	case len(env.References) != 1 || env.References[0] != wantRoom:
		t.Errorf("references = %+v, want the session room %+v", env.References, wantRoom)
	case env.MessageID != res.Message || env.Destination != "support" || env.Audience.Visibility != model.Public:
		t.Errorf("envelope = %+v", env)
	case len(env.Content.Parts) != 1 || env.Content.Parts[0].Text != "hello":
		t.Errorf("content = %+v", env.Content)
	case !env.OccurredAt.Equal(r.clock.Now()):
		t.Errorf("occurred_at = %v", env.OccurredAt)
	}
	if code, body := r.post("/widget/messages", s.Token, widget.MessageRequest{ID: "c-1", Text: "hello"}); code != http.StatusAccepted ||
		!strings.Contains(body, `"duplicate":true`) {
		t.Fatalf("retried message = %d %s, want an accepted duplicate", code, body)
	}
}

func TestReceiverRefusesMalformedRequests(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	big := widget.MessageRequest{ID: "1", Text: strings.Repeat("x", 70<<10)}
	for name, c := range map[string]struct {
		path string
		body any
		want int
	}{
		"unknown path":   {"/widget/other", widget.MessageRequest{ID: "1", Text: "x"}, http.StatusNotFound},
		"no ID":          {"/widget/messages", widget.MessageRequest{Text: "x"}, http.StatusBadRequest},
		"no text":        {"/widget/messages", widget.MessageRequest{ID: "1"}, http.StatusBadRequest},
		"unknown field":  {"/widget/messages", []byte(`{"id":"1","text":"x","history":[]}`), http.StatusBadRequest},
		"two values":     {"/widget/messages", []byte(`{"id":"1","text":"x"}{}`), http.StatusBadRequest},
		"not JSON":       {"/widget/messages", []byte(`id=1`), http.StatusBadRequest},
		"oversized body": {"/widget/messages", big, http.StatusRequestEntityTooLarge},
		"long ID":        {"/widget/messages", widget.MessageRequest{ID: strings.Repeat("i", 129), Text: "x"}, http.StatusBadRequest},
	} {
		if code, _ := r.post(c.path, s.Token, c.body); code != c.want {
			t.Errorf("%s = %d, want %d", name, code, c.want)
		}
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, r.srv.URL+"/widget/messages", strings.NewReader(`{"id":"1","text":"x"}`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+s.Token)
	if res, err := r.srv.Client().Do(req); err != nil || res.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form content type = %v %v, want 415", res.StatusCode, err)
	}
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, r.srv.URL+"/widget/messages", nil)
	if res, err := r.srv.Client().Do(req); err != nil || res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET = %v %v, want 405", res.StatusCode, err)
	}
	if n := len(r.sink.received()); n != 0 {
		t.Fatalf("ingress = %d, want none", n)
	}
}

func TestReceiverSinkFailures(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	for code, want := range map[model.ErrorCode]int{
		model.Transient:  http.StatusServiceUnavailable,
		model.Validation: http.StatusUnprocessableEntity,
		model.Disabled:   http.StatusUnprocessableEntity,
	} {
		r.sink.mu.Lock()
		r.sink.acceptErr = model.NewError(code, "refused")
		r.sink.mu.Unlock()
		if got, body := r.post("/widget/messages", s.Token, widget.MessageRequest{ID: "1", Text: "x"}); got != want ||
			strings.TrimSpace(body) != http.StatusText(want) {
			t.Errorf("%s sink error = %d %q, want %d with the status text only", code, got, body, want)
		}
	}
}

func TestReceiverClosedInstance(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	_ = r.inst.Close(context.Background())
	if code := r.message(s.Token, "1", "x"); code != http.StatusServiceUnavailable {
		t.Fatalf("closed instance = %d, want 503", code)
	}
}

func TestReceiverInteraction(t *testing.T) {
	r := newRig(t, widget.Options{InteractionTimeout: 3 * time.Second})
	s := r.session("", widget.AssuranceAnonymous)
	code, body := r.post("/widget/interactions", s.Token, widget.InteractionRequest{
		ID: "form-1", Kind: "submit", Fields: model.Extension{Name: "com.example.contact.form", Version: 1, Data: json.RawMessage(`{"email":""}`)},
	})
	if code != http.StatusOK {
		t.Fatalf("interaction = %d %s", code, body)
	}
	var res widget.InteractionResponse
	if err := json.Unmarshal([]byte(body), &res); err != nil || res.FieldErrors["email"] != "required" {
		t.Fatalf("response = %s", body)
	}
	got := r.sink.evaluated()
	if len(got) != 1 {
		t.Fatalf("evaluations = %d", len(got))
	}
	in := got[0]
	wantActor := model.ExternalRef{Channel: chanA, Namespace: widget.NamespaceVisitor, Kind: widget.KindVisitor, ID: s.Visitor}
	if in.Actor != wantActor || in.Kind != "submit" || in.ID == "form-1" || !in.Deadline.Equal(r.clock.Now().Add(3*time.Second)) {
		t.Fatalf("interaction = %+v", in)
	}
	if len(in.Context) != 1 || in.Context[0].Kind != model.RefKindRoom || in.Context[0].ID != s.Session {
		t.Fatalf("context = %+v, want the session", in.Context)
	}
	if code, _ := r.post("/widget/interactions", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("interaction without a token = %d, want 401", code)
	}
}
