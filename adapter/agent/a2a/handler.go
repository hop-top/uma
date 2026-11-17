package a2a

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Instance is one opened channel: the JSON-RPC receiver and the sender. It
// is safe for concurrent use.
type Instance struct {
	f     *Factory
	ch    model.ChannelRef
	cfg   config
	clock ports.Clock
	card  card

	mu     sync.Mutex
	closed bool
}

// Capabilities reports every declared capability available.
func (x *Instance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	if err := x.ready(ctx); err != nil {
		return nil, err
	}
	out := make([]model.CapabilityStatus, len(capabilities))
	for i, c := range capabilities {
		out[i] = model.CapabilityStatus{Name: c, Available: true}
	}
	return out, nil
}

// Close closes the instance; it is idempotent. A closed instance's handler
// answers 503 and its calls fail with model.Unavailable.
func (x *Instance) Close(context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.closed = true
	return nil
}

func (x *Instance) isClosed() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.closed
}

func (x *Instance) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if x.isClosed() {
		return model.NewError(model.Unavailable, "a2a instance closed")
	}
	return nil
}

// sinks are the engine-side interfaces the handler needs of its sink.
type sinks struct {
	adapter.Sink
	tasks adapter.TaskSink
	files adapter.AttachmentSink
}

// Handler returns the A2A receiver, to be mounted by Runtime.Webhook at
// any path. GET (or HEAD) of agent-card.json, under any prefix, serves the
// Agent Card without authentication. A POST is a JSON-RPC 2.0 request,
// whatever its path: the host's Authenticator runs first and the body is
// read only after it accepts, then the A2A-Version header must name
// ProtocolVersion, then the method runs. The sink must implement
// adapter.TaskSink and adapter.AttachmentSink, as the engine's does; if not
// every request but the card answers 503.
//
// HTTP statuses are for transport failures only: 401 (with WWW-Authenticate
// naming the declared security schemes) or 503 for a refused
// authentication, 405, 415, and 413 from the engine's body limit. Every
// other answer, JSON-RPC errors included, is 200.
func (x *Instance) Handler(sink adapter.Sink) http.Handler {
	s := sinks{Sink: sink}
	s.tasks, _ = sink.(adapter.TaskSink)
	s.files, _ = sink.(adapter.AttachmentSink)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if x.isClosed() {
			status(w, http.StatusServiceUnavailable)
			return
		}
		switch {
		case (r.Method == http.MethodGet || r.Method == http.MethodHead) && path.Base(r.URL.Path) == cardPath:
			x.serveCard(w, r)
		case r.Method != http.MethodPost:
			w.Header().Set("Allow", "POST, GET, HEAD")
			status(w, http.StatusMethodNotAllowed)
		case s.tasks == nil || s.files == nil:
			status(w, http.StatusServiceUnavailable)
		default:
			x.serveRPC(w, r, s)
		}
	})
}

func (x *Instance) serveRPC(w http.ResponseWriter, r *http.Request, s sinks) {
	p, err := x.f.authenticate(r, x.ch)
	if err != nil {
		if model.CodeOf(err).DefaultRetry() == model.RetrySameOperation {
			status(w, http.StatusServiceUnavailable)
			return
		}
		x.unauthorized(w)
		return
	}
	principal := x.principal(p)
	if principal.Validate() != nil {
		x.unauthorized(w)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || (mt != "application/json" && mt != "application/a2a+json") {
		status(w, http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		status(w, http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		status(w, http.StatusBadRequest)
		return
	}
	req, rerr := parseRequest(body)
	if version := requestedVersion(r); rerr == nil && version != ProtocolVersion {
		rerr = a2aError(codeVersion, "Version not supported", "VERSION_NOT_SUPPORTED",
			map[string]string{"requestedVersion": version, "supportedVersions": ProtocolVersion})
	}
	if rerr != nil {
		writeResponse(w, req.ID, nil, rerr)
		return
	}
	c := &call{x: x, s: s, principal: principal}
	result, rerr := c.dispatch(r.Context(), req)
	if rerr == nil && result == nil {
		return
	}
	writeResponse(w, req.ID, result, rerr)
}

// unauthorized answers a refused authentication: 401 naming the declared
// schemes.
func (x *Instance) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", strings.Join(x.card.challenges, ", "))
	status(w, http.StatusUnauthorized)
}

// principal is the sender reference of an authenticated agent.
func (x *Instance) principal(p Principal) model.ExternalRef {
	return model.ExternalRef{Channel: x.ch, Namespace: NamespacePrincipal, Kind: KindPrincipal, ID: p.ID}
}

// requestedVersion is the protocol version a request names: the A2A-Version
// header, else the A2A-Version query parameter; empty means 0.3 by the
// specification, which no version here matches.
func requestedVersion(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("A2A-Version")); v != "" {
		return v
	}
	return strings.TrimSpace(r.URL.Query().Get("A2A-Version"))
}

func status(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}

// call is one authenticated JSON-RPC request.
type call struct {
	x         *Instance
	s         sinks
	principal model.ExternalRef
}

var (
	_ adapter.HTTPReceiver = (*Instance)(nil)
	_ adapter.Sender       = (*Instance)(nil)
)
