package imessage

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Paths the gateway serves.
const (
	// PathReply is the relay's callback: one reply per POST.
	PathReply = "/uma/reply"
	// PathLookup is the relay's lookup: GET ?operation_id=<key>.
	PathLookup = "/uma/lookup"
	// PathWebhook is where the backend posts its webhooks; with a webhook
	// token the path is PathWebhook + "/" + token.
	PathWebhook = "/webhook"
)

// Medium is the only relay medium the gateway serves.
const Medium = "imessage"

// Limits.
const (
	maxCallbackBytes = 128 << 10
	maxWebhookBytes  = 1 << 20
	maxKeyBytes      = 255
	minTokenBytes    = 16
	// DefaultSendTimeout bounds one backend send.
	DefaultSendTimeout = 60 * time.Second
)

// Config configures a Gateway.
type Config struct {
	// Backend sends and receives. Required.
	Backend Backend
	// Keys remembers Idempotency-Key values. Required.
	Keys *KeyStore

	// CallbackSecret is what uma presents on the callback and the lookup:
	// the channel's relay.callback credential. Required.
	CallbackSecret []byte
	// CallbackHeader names the header carrying CallbackSecret as its whole
	// value, for a channel whose callback.auth is "header". Empty: the
	// secret comes as "Authorization: Bearer <secret>".
	CallbackHeader string

	// WebhookURL is uma's webhook for the channel (the handler the host
	// mounts from Runtime.Webhook). Required.
	WebhookURL string
	// WebhookSecret signs what the gateway posts to WebhookURL: the
	// channel's relay.webhook credential. Required, at least 16 bytes.
	WebhookSecret []byte
	// UmaClient posts to WebhookURL. Nil: a client with a 15 second
	// timeout that follows no redirect.
	UmaClient *http.Client

	// WebhookToken is the secret path segment of the backend's webhook URL
	// (PathWebhook/<token>). Required, at least 16 bytes, for a backend
	// that signs no webhook; optional otherwise.
	WebhookToken string

	// SendTimeout bounds one backend send; zero is DefaultSendTimeout. The
	// send is not cut short when uma stops waiting for the answer.
	SendTimeout time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger receives one line per callback and webhook, with keys and
	// statuses only, never text, handles or credentials. Nil discards.
	Logger *slog.Logger
	// Forwarded, when set, is called with each backend webhook body the
	// gateway forwarded to uma and the sender of the message forwarded,
	// after uma accepted it: an audit hook.
	Forwarded func(body []byte, from string)
}

// Gateway is the relay gateway: uma's callback and lookup on one side, the
// backend's send API and webhook on the other.
type Gateway struct {
	cfg Config
	log *slog.Logger
}

// New checks cfg and returns the gateway.
func New(cfg Config) (*Gateway, error) {
	switch {
	case cfg.Backend == nil:
		return nil, errors.New("gateway: backend required")
	case cfg.Keys == nil:
		return nil, errors.New("gateway: key store required")
	case len(cfg.CallbackSecret) == 0 || !headerValue(cfg.CallbackSecret):
		return nil, errors.New("gateway: callback secret required, printable ASCII without spaces")
	case cfg.WebhookURL == "":
		return nil, errors.New("gateway: uma webhook URL required")
	case len(cfg.WebhookSecret) < minTokenBytes:
		return nil, errors.New("gateway: webhook secret of at least 16 bytes required")
	case !cfg.Backend.SignsWebhooks() && len(cfg.WebhookToken) < minTokenBytes:
		return nil, errors.New("gateway: " + cfg.Backend.Name() + " signs no webhook: a webhook token of at least 16 characters is required")
	case cfg.WebhookToken != "" && !pathToken(cfg.WebhookToken):
		return nil, errors.New("gateway: webhook token must be letters, digits, '-' or '_'")
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = DefaultSendTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.UmaClient == nil {
		cfg.UmaClient = &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Gateway{cfg: cfg, log: log}, nil
}

// Handler serves PathReply, PathLookup and the webhook path.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PathReply, g.reply)
	mux.HandleFunc(PathLookup, g.lookup)
	webhook := PathWebhook
	if g.cfg.WebhookToken != "" {
		mux.HandleFunc(PathWebhook+"/{token}", g.webhook)
	} else {
		mux.HandleFunc(webhook, g.webhook)
	}
	return mux
}

// callbackBody is the relay's reply JSON.
type callbackBody struct {
	Medium      string `json:"medium"`
	To          string `json:"to"`
	Body        string `json:"body"`
	InReplyTo   string `json:"in_reply_to"`
	OperationID string `json:"operation_id"`
	AttemptID   string `json:"attempt_id"`
}

// reply takes one callback. The key is recorded, synced to disk, before the
// backend is called; a key already held is answered from its record and
// never sent again.
func (g *Gateway) reply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		answer(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if !g.authorized(r) {
		answer(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !validKey(key) {
		answer(w, http.StatusBadRequest, map[string]string{"error": "idempotency_key_required"})
		return
	}
	var b callbackBody
	data, err := io.ReadAll(io.LimitReader(r.Body, maxCallbackBytes+1))
	if err != nil || len(data) > maxCallbackBytes || json.Unmarshal(data, &b) != nil {
		answer(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	switch {
	case b.OperationID != key:
		answer(w, http.StatusBadRequest, map[string]string{"error": "operation_id_mismatch"})
		return
	case b.Medium != Medium:
		answer(w, http.StatusUnprocessableEntity, map[string]string{"error": "unsupported_medium"})
		return
	case b.To == "" || strings.TrimSpace(b.Body) == "":
		answer(w, http.StatusUnprocessableEntity, map[string]string{"error": "to_and_body_required"})
		return
	}
	rec, fresh, err := g.cfg.Keys.Reserve(key)
	if err != nil {
		g.log.Error("callback: key store write failed", "key", key)
		answer(w, http.StatusServiceUnavailable, map[string]string{"error": "key_store_unavailable"})
		return
	}
	if !fresh {
		g.log.Info("callback: repeat", "key", key, "state", string(rec.State))
		g.answerRecord(w, rec)
		return
	}
	// uma may stop waiting; the backend call runs to its own bound so its
	// outcome is recorded either way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), g.cfg.SendTimeout)
	defer cancel()
	res := g.cfg.Backend.Send(ctx, Outbound{Key: key, To: b.To, Text: b.Body})
	g.log.Info("callback: sent to backend", "key", key, "backend", g.cfg.Backend.Name(), "outcome", res.Outcome.String(), "detail", res.Detail)
	g.settle(w, key, res)
}

// settle records a backend outcome and answers the relay.
func (g *Gateway) settle(w http.ResponseWriter, key string, res Result) {
	var err error
	switch res.Outcome {
	case NotSent:
		if err = g.cfg.Keys.Release(key); err == nil {
			if res.RateLimited {
				if s := int(res.RetryAfter.Seconds()); s > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(s))
				}
				answer(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
				return
			}
			answer(w, http.StatusServiceUnavailable, map[string]string{"error": "backend_unavailable"})
			return
		}
	case Sent:
		err = g.cfg.Keys.Settle(key, StateSent, res.MessageID)
	case Rejected:
		err = g.cfg.Keys.Settle(key, StateRejected, "")
	default:
		err = g.cfg.Keys.Settle(key, StateUnknown, "")
	}
	rec, ok := g.cfg.Keys.Get(key)
	if err != nil || !ok {
		// The record stays as it was (pending): never acted on again.
		g.log.Error("callback: key store write failed", "key", key)
		answer(w, http.StatusBadGateway, map[string]string{"error": "outcome_unrecorded"})
		return
	}
	g.answerRecord(w, rec)
}

// answerRecord answers a callback from what the gateway holds for its key:
// the same answer for every callback with the key.
func (g *Gateway) answerRecord(w http.ResponseWriter, rec Record) {
	switch rec.State {
	case StateSent:
		out := map[string]string{}
		if rec.MessageID != "" {
			out["message_id"] = rec.MessageID
		}
		answer(w, http.StatusOK, out)
	case StateRejected:
		answer(w, http.StatusUnprocessableEntity, map[string]string{"error": "rejected_by_backend"})
	case StatePending:
		// Another callback with the key is in flight: uma takes 409 as
		// outcome unknown and asks the lookup.
		answer(w, http.StatusConflict, map[string]string{"error": "in_progress"})
	default:
		answer(w, http.StatusBadGateway, map[string]string{"error": "outcome_unknown"})
	}
}

// lookup answers what the gateway holds for operation_id: sent with the
// message id, not_found only for a key it holds no record of, pending for
// anything else.
func (g *Gateway) lookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		answer(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if !g.authorized(r) {
		answer(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	key := r.URL.Query().Get("operation_id")
	if !validKey(key) {
		answer(w, http.StatusBadRequest, map[string]string{"error": "operation_id_required"})
		return
	}
	rec, ok := g.cfg.Keys.Get(key)
	switch {
	case !ok:
		answer(w, http.StatusNotFound, map[string]string{"status": "not_found"})
	case rec.State == StateSent:
		out := map[string]string{"status": "sent"}
		if rec.MessageID != "" {
			out["message_id"] = rec.MessageID
		}
		answer(w, http.StatusOK, out)
	case rec.State == StateRejected:
		answer(w, http.StatusOK, map[string]string{"status": "rejected"})
	default:
		answer(w, http.StatusOK, map[string]string{"status": "pending"})
	}
}

// authorized checks the callback credential in constant time.
func (g *Gateway) authorized(r *http.Request) bool {
	var got string
	if g.cfg.CallbackHeader != "" {
		vals := r.Header.Values(g.cfg.CallbackHeader)
		if len(vals) != 1 {
			return false
		}
		got = vals[0]
	} else {
		vals := r.Header.Values("Authorization")
		if len(vals) != 1 {
			return false
		}
		scheme, token, ok := strings.Cut(vals[0], " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return false
		}
		got = strings.TrimLeft(token, " ")
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256(g.cfg.CallbackSecret)
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func answer(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func validKey(k string) bool {
	if k == "" || len(k) > maxKeyBytes || !utf8.ValidString(k) {
		return false
	}
	for _, c := range k {
		if c <= ' ' || c == 0x7f {
			return false
		}
	}
	return true
}

func headerValue(b []byte) bool {
	for _, c := range b {
		if c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

func pathToken(s string) bool {
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// String names an outcome in logs.
func (o Outcome) String() string {
	switch o {
	case Sent:
		return "sent"
	case Rejected:
		return "rejected"
	case NotSent:
		return "not_sent"
	}
	return "unknown"
}
