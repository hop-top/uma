package widget

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// maxBodyBytes bounds every receiver request body.
const maxBodyBytes = 64 << 10

// maxClientIDBytes bounds client-chosen message and interaction IDs.
const maxClientIDBytes = 128

// SessionResponse is the JSON answer to a session request.
type SessionResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Visitor   string    `json:"visitor"`
	Assurance string    `json:"assurance"`
	Session   string    `json:"session"`
}

// MessageRequest is the JSON body of a visitor message. ID is chosen by
// the client, unique per visitor: a retried request with the same ID is a
// duplicate, never a second message.
//
// Attachments lists handles of the visitor's uploads (UploadResponse) to
// attach; a message carries text, attachments or both.
type MessageRequest struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	Attachments []string `json:"attachments,omitempty"`
}

// MessageResponse is the JSON answer to an accepted visitor message.
type MessageResponse struct {
	Message   string `json:"message"`
	Duplicate bool   `json:"duplicate"`
}

// InteractionRequest is the JSON body of an interaction submission, such
// as a form: its client-chosen ID, kind and fields.
type InteractionRequest struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	Fields model.Extension `json:"fields"`
}

// InteractionResponse is the JSON answer to an evaluated submission.
type InteractionResponse struct {
	FieldErrors map[string]string `json:"field_errors,omitempty"`
	Options     model.Extension   `json:"options"`
}

// Handler returns the visitor-facing receiver. It routes POST requests on
// the last path segment:
//
//   - session: the host's SessionVerifier establishes the visitor, and the
//     answer is a SessionResponse. A request presenting a valid token of
//     the same visitor keeps its session ID (and an anonymous visitor's
//     ID), so renewing a token keeps the conversation.
//   - messages: a MessageRequest from the token's visitor, handed to sink
//     as a message to the token's site, with the uploads it lists as
//     attachment parts. 202 with a MessageResponse once the sink accepted
//     it, new or duplicate; 400 for a handle that is not the visitor's.
//   - uploads: a visitor upload staged through the sink (see Upload).
//     201 with an UploadResponse; 501 when the engine stages no
//     attachments (no blob store).
//   - interactions: an InteractionRequest from the token's visitor,
//     evaluated by sink within the interaction timeout, never accepted:
//     the synchronous path for field validation and dynamic options. 200
//     with an InteractionResponse.
//   - actions: an ActionRequest, a control press or a command of the
//     token's visitor, handed to sink as an interaction (see Interactions).
//     202 with an AcceptedInteraction once the sink accepted it, new or
//     duplicate.
//   - submissions: a SubmissionRequest, a form of the token's visitor,
//     handed to sink the same way and answered the same way.
//
// messages, interactions, actions and submissions need the token as a
// bearer token. Nothing reaches sink before the token verifies. Errors
// answer with the status text only: 405 for another method, 404 for
// another path, 415 without a JSON content type, 413 for a body over the
// limit, 400 for a malformed body, 401 for a missing, forged, expired or
// foreign token or a refused session, 503 when the session key cannot be
// resolved, the instance is closed or the sink fails retryably, 504 when
// an evaluation runs out of time or the interaction's deadline passed,
// and 422 for any other sink refusal.
func (x *Instance) Handler(sink adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			status(w, http.StatusMethodNotAllowed)
			return
		}
		if x.isClosed() {
			status(w, http.StatusServiceUnavailable)
			return
		}
		route := path.Base(r.URL.Path)
		switch route {
		case "session", "messages", "interactions", "uploads", "actions", "submissions":
		default:
			status(w, http.StatusNotFound)
			return
		}
		if route != "uploads" {
			// An upload's body is the file under its own type, bounded by
			// the engine's attachment limit; it needs the bearer token,
			// which no cross-site form can send.
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				status(w, http.StatusUnsupportedMediaType)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		key, err := resolveKey(r.Context(), x.b.creds, x.b.channel)
		if err != nil {
			status(w, http.StatusServiceUnavailable)
			return
		}
		if route == "session" {
			x.serveSession(w, r, key)
			return
		}
		c, err := verify(key, bearer(r), x.ref(), x.now())
		if err != nil {
			status(w, http.StatusUnauthorized)
			return
		}
		if _, ok := x.b.cfg.site(c.Site); !ok {
			status(w, http.StatusUnauthorized)
			return
		}
		switch route {
		case "messages":
			x.serveMessage(w, r, sink, c, key)
			return
		case "uploads":
			x.serveUpload(w, r, sink, c, key)
			return
		case "actions":
			x.serveAction(w, r, sink, c)
			return
		case "submissions":
			x.serveSubmission(w, r, sink, c)
			return
		}
		x.serveInteraction(w, r, sink, c)
	})
}

func (x *Instance) serveSession(w http.ResponseWriter, r *http.Request, key []byte) {
	v, err := x.f.verifier.VerifySession(r, x.ref())
	if err != nil {
		if model.CodeOf(err).DefaultRetry() == model.RetrySameOperation {
			status(w, http.StatusServiceUnavailable)
			return
		}
		status(w, http.StatusUnauthorized)
		return
	}
	site, ok := x.b.cfg.site(v.Site)
	switch {
	case !ok:
		status(w, http.StatusUnauthorized)
		return
	case v.Assurance != AssuranceAnonymous && v.Assurance != AssuranceVerified:
		status(w, http.StatusUnauthorized)
		return
	case v.Assurance == AssuranceVerified && v.ID == "":
		status(w, http.StatusUnauthorized)
		return
	case len(v.ID) > maxVisitorBytes:
		status(w, http.StatusUnauthorized)
		return
	}
	c := claims{Tenant: x.ref().Tenant, Channel: x.ref().ID, Visitor: v.ID, Assurance: v.Assurance, Site: site}
	if prev, err := verify(key, bearer(r), x.ref(), x.now()); err == nil && prev.Assurance == c.Assurance && prev.Site == c.Site &&
		(prev.Visitor == c.Visitor || c.Visitor == "" && c.Assurance == AssuranceAnonymous) {
		c.Visitor, c.Session = prev.Visitor, prev.Session
	}
	if c.Visitor == "" {
		if c.Visitor, err = randomID("a"); err != nil {
			status(w, http.StatusServiceUnavailable)
			return
		}
	}
	if err := c.sender(x.ref()).Validate(); err != nil {
		status(w, http.StatusUnauthorized)
		return
	}
	if c.Session == "" {
		if c.Session, err = randomID("s"); err != nil {
			status(w, http.StatusServiceUnavailable)
			return
		}
	}
	exp := x.now().Add(x.f.ttl).Truncate(time.Second)
	c.Expires = exp.Unix()
	token, err := sign(key, c)
	if err != nil {
		status(w, http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, SessionResponse{
		Token: token, ExpiresAt: exp.UTC(), Visitor: c.Visitor, Assurance: c.Assurance, Session: c.Session,
	})
}

func (x *Instance) serveMessage(w http.ResponseWriter, r *http.Request, sink adapter.Sink, c claims, key []byte) {
	var req MessageRequest
	if !decode(w, r, &req) {
		return
	}
	if !validClientID(req.ID) || (req.Text == "" && len(req.Attachments) == 0) || len(req.Attachments) > maxAttachments {
		status(w, http.StatusBadRequest)
		return
	}
	ch := x.ref()
	var content model.Content
	if req.Text != "" {
		content.Parts = append(content.Parts, model.Part{Kind: model.PartText, Text: req.Text})
	}
	for _, h := range req.Attachments {
		ref, ok := attachmentOf(key, ch, c, h)
		if !ok {
			status(w, http.StatusBadRequest)
			return
		}
		content.Parts = append(content.Parts, model.Part{Kind: model.PartAttachment, Attachment: &ref})
	}
	id := inboundID("message", c, req.ID)
	receipt, err := sink.Accept(r.Context(), model.Ingress{Envelope: model.Envelope{
		MessageID: id, Destination: c.Site, Kind: model.EnvelopeMessage,
		Sender: c.sender(ch), Assurance: c.Assurance, References: []model.ExternalRef{c.room(ch)},
		Content:  content,
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: x.now().UTC(),
	}})
	if err != nil {
		sinkError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, MessageResponse{Message: id, Duplicate: receipt.Duplicate})
}

func (x *Instance) serveInteraction(w http.ResponseWriter, r *http.Request, sink adapter.Sink, c claims) {
	var req InteractionRequest
	if !decode(w, r, &req) {
		return
	}
	if !validClientID(req.ID) || req.Kind == "" {
		status(w, http.StatusBadRequest)
		return
	}
	ch := x.ref()
	d, err := sink.EvaluateInteraction(r.Context(), model.InteractionInput{
		ID: inboundID("interaction", c, req.ID), Kind: req.Kind, Actor: c.sender(ch),
		Context: []model.ExternalRef{c.room(ch)}, Deadline: x.now().Add(x.f.interactionTimeout), Fields: req.Fields,
	})
	if err != nil {
		sinkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, InteractionResponse{FieldErrors: d.FieldErrors, Options: d.Options})
}

// inboundID scopes a client-chosen ID to its visitor, so equal IDs from
// two visitors never collide in deduplication.
func inboundID(kind string, c claims, clientID string) string {
	h := sha256.New()
	for _, s := range []string{kind, c.namespace(), c.Visitor, clientID} {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	return "v" + hex.EncodeToString(h.Sum(nil)[:16])
}

func validClientID(id string) bool {
	if id == "" || len(id) > maxClientIDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c <= ' ' || c == 0x7f {
			return false
		}
	}
	return true
}

// bearer returns the request's bearer token, or "".
func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// decode reads one JSON value into v, answering 413 or 400 on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		status(w, http.StatusRequestEntityTooLarge)
		return false
	case err != nil:
		status(w, http.StatusBadRequest)
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		status(w, http.StatusBadRequest)
		return false
	}
	return true
}

func sinkError(w http.ResponseWriter, err error) {
	switch code := model.CodeOf(err); {
	case code.DefaultRetry() == model.RetrySameOperation:
		status(w, http.StatusServiceUnavailable)
	case code == model.DeadlineExpired:
		status(w, http.StatusGatewayTimeout)
	default:
		status(w, http.StatusUnprocessableEntity)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status(w, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func status(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}
