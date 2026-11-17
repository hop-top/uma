package host_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hop.top/uma"
	"hop.top/uma/companions/bot/host"
	"hop.top/uma/model"
)

func appHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
}

func serve(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func widget() model.ChannelRef { return model.ChannelRef{ID: "widget"} }

// build calls Mux and fails the test if it panics instead of returning.
func build(t *testing.T, rt uma.Runtime, bindings []host.Binding, routes []host.Route) (h http.Handler, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Mux panicked: %v", r)
		}
	}()
	return host.Mux(context.Background(), rt, uma.RuntimeCall{}, bindings, routes)
}

func wantValidation(t *testing.T, err error, pattern string) {
	t.Helper()
	if err == nil {
		t.Fatal("Mux returned no error")
	}
	if got := model.CodeOf(err); got != model.Validation {
		t.Fatalf("code = %v, want Validation (err: %v)", got, err)
	}
	if !strings.Contains(err.Error(), pattern) {
		t.Errorf("error %q does not name the pattern %q", err, pattern)
	}
}

func TestMuxServesBindingAndRoute(t *testing.T) {
	h, err := build(t, newFakeRuntime(),
		[]host.Binding{{Pattern: "POST /widget/", Channel: widget()}},
		[]host.Route{{Pattern: "GET /widget/events", Handler: appHandler("app")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := serve(h, http.MethodPost, "/widget/").Body.String(); got != "hook:widget" {
		t.Errorf("POST /widget/ = %q, want hook:widget", got)
	}
	if got := serve(h, http.MethodGet, "/widget/events").Body.String(); got != "app" {
		t.Errorf("GET /widget/events = %q, want app", got)
	}
}

func TestMuxPassesCallAndChannelToRuntime(t *testing.T) {
	rt := newFakeRuntime()
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: "acme", Actor: "bot"}, Call: model.Call{Correlation: "c1"}}
	if _, err := host.Mux(context.Background(), rt, call, []host.Binding{{Pattern: "POST /a", Channel: widget()}}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := rt.lastArgs(); got != call {
		t.Errorf("call = %+v, want %+v", got, call)
	}
}

func TestMuxRejectsBadPatterns(t *testing.T) {
	ok := appHandler("app")
	tests := []struct {
		name     string
		bindings []host.Binding
		routes   []host.Route
		pattern  string
	}{
		{"duplicate binding and route", []host.Binding{{Pattern: "POST /widget/", Channel: widget()}},
			[]host.Route{{Pattern: "POST /widget/", Handler: ok}}, "POST /widget/"},
		{"duplicate routes", nil,
			[]host.Route{{Pattern: "GET /a", Handler: ok}, {Pattern: "GET /a", Handler: ok}}, "GET /a"},
		{"ambiguous overlap", nil,
			[]host.Route{{Pattern: "GET /{x}/z", Handler: ok}, {Pattern: "GET /a/{y}", Handler: ok}}, "GET /a/{y}"},
		{"empty route pattern", nil, []host.Route{{Pattern: "", Handler: ok}}, `""`},
		{"empty binding pattern", []host.Binding{{Pattern: "", Channel: widget()}}, nil, `""`},
		{"malformed pattern", nil, []host.Route{{Pattern: "GET", Handler: ok}}, `"GET"`},
		{"nil route handler", nil, []host.Route{{Pattern: "GET /nil"}}, "GET /nil"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := build(t, newFakeRuntime(), tc.bindings, tc.routes)
			wantValidation(t, err, tc.pattern)
			if h != nil {
				t.Errorf("handler = %v, want nil on error", h)
			}
			if strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), ".go:") {
				t.Errorf("error leaks panic detail (extra lines or a source path): %q", err)
			}
		})
	}
}

// Validation comes before resolution: a bad route resolves no receiver.
func TestMuxValidatesBeforeResolvingReceivers(t *testing.T) {
	ok := appHandler("app")
	for name, routes := range map[string][]host.Route{
		"conflicting route": {{Pattern: "POST /widget/", Handler: ok}},
		"nil handler":       {{Pattern: "GET /nil"}},
	} {
		t.Run(name, func(t *testing.T) {
			rt := newFakeRuntime()
			h, err := build(t, rt, []host.Binding{{Pattern: "POST /widget/", Channel: widget()}}, routes)
			wantValidation(t, err, routes[0].Pattern)
			if h != nil {
				t.Errorf("handler = %v, want nil on error", h)
			}
			if w, _, _ := rt.counts(); w != 0 {
				t.Errorf("Webhook called %d times before the routes were validated", w)
			}
		})
	}
}

func TestMuxWrapsWebhookError(t *testing.T) {
	h, err := build(t, newFakeRuntime(),
		[]host.Binding{{Pattern: "POST /bad/", Channel: model.ChannelRef{ID: "bad"}}}, nil)
	if err == nil {
		t.Fatal("Mux returned no error")
	}
	if h != nil {
		t.Errorf("handler = %v, want nil on error", h)
	}
	if !strings.Contains(err.Error(), "POST /bad/") {
		t.Errorf("error %q does not name the binding pattern", err)
	}
	if got := model.CodeOf(err); got != model.NotFound {
		t.Errorf("code = %v, want the runtime's NotFound preserved", got)
	}
}

func TestMuxMethodSpecializationIsLegal(t *testing.T) {
	h, err := build(t, newFakeRuntime(), nil, []host.Route{
		{Pattern: "GET /x", Handler: appHandler("get")},
		{Pattern: "/x", Handler: appHandler("any")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := serve(h, http.MethodGet, "/x").Body.String(); got != "get" {
		t.Errorf("GET /x = %q, want get", got)
	}
	if got := serve(h, http.MethodPut, "/x").Body.String(); got != "any" {
		t.Errorf("PUT /x = %q, want any", got)
	}
}

func TestMuxEmptyIsValid(t *testing.T) {
	h, err := build(t, newFakeRuntime(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := serve(h, http.MethodGet, "/").Code; got != http.StatusNotFound {
		t.Errorf("empty mux status = %d, want 404", got)
	}
}
