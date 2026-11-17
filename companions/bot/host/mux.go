package host

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"hop.top/uma"
	"hop.top/uma/model"
)

// Binding mounts one receiver on the application's mux. Pattern is an
// http.ServeMux pattern such as "POST /widget/"; Channel is the channel the
// runtime resolves into the receiver's handler. Nothing is inferred: a
// channel is served only where a Binding says so.
type Binding struct {
	Pattern string
	Channel model.ChannelRef
}

// Route is one application-owned handler. Pattern is an http.ServeMux
// pattern.
type Route struct {
	Pattern string
	Handler http.Handler
}

// registeredAt matches the "(registered at /abs/path/file.go:12)" note
// ServeMux adds to a conflict, which names a source file of the build
// machine.
var registeredAt = regexp.MustCompile(` \(registered at [^)]*\)`)

// register adds h at pattern, turning the panics ServeMux raises for an
// invalid or conflicting pattern into a Validation error.
func register(mux *http.ServeMux, pattern string, h http.Handler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			msg, _, _ := strings.Cut(fmt.Sprint(r), "\n") // later lines restate the conflict
			msg = strings.TrimSuffix(registeredAt.ReplaceAllString(msg, ""), ":")
			err = model.NewError(model.Validation, fmt.Sprintf("host: route %q: %s", pattern, msg))
		}
	}()
	if pattern == "" || h == nil {
		return model.NewError(model.Validation, fmt.Sprintf("host: route %q requires a pattern and a handler", pattern))
	}
	mux.Handle(pattern, h)
	return nil
}

// checkRoutes registers every binding pattern and every route on a scratch
// mux, so an invalid or conflicting pattern is reported before any receiver
// is resolved. A binding's handler is not known yet, so a placeholder stands
// in for it.
func checkRoutes(bindings []Binding, routes []Route) error {
	scratch := http.NewServeMux()
	placeholder := http.NotFoundHandler()
	for _, b := range bindings {
		if err := register(scratch, b.Pattern, placeholder); err != nil {
			return err
		}
	}
	for _, r := range routes {
		if err := register(scratch, r.Pattern, r.Handler); err != nil {
			return err
		}
	}
	return nil
}

// Mux resolves each receiver binding through the runtime and registers it
// with the application's routes on one ServeMux. It validates every pattern
// and route before it resolves a receiver, and panics never: an invalid or
// conflicting pattern is a Validation error. It starts nothing and owns no
// server; mount the result in the server the application runs, or hand it to
// ListenAndServe.
func Mux(ctx context.Context, rt uma.Runtime, call uma.RuntimeCall, bindings []Binding, routes []Route) (http.Handler, error) {
	if err := checkRoutes(bindings, routes); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	for _, b := range bindings {
		h, err := rt.Webhook(ctx, call, b.Channel)
		if err != nil {
			return nil, fmt.Errorf("host: receiver for %q: %w", b.Pattern, err)
		}
		if err := register(mux, b.Pattern, h); err != nil {
			return nil, err
		}
	}
	for _, r := range routes {
		if err := register(mux, r.Pattern, r.Handler); err != nil {
			return nil, err
		}
	}
	return mux, nil
}
