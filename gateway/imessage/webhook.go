package imessage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// Headers of what the gateway posts to uma. The channel's auth matches
// them: scheme hmac, header SignatureHeader, timestamp header
// TimestampHeader (Unix seconds); the signed payload is the timestamp, a
// dot and the body, HMAC-SHA-256, hex.
const (
	SignatureHeader = "X-Webhook-Signature"
	TimestampHeader = "X-Webhook-Timestamp"
)

// umaMessage is the relay's inbound JSON, in its neutral field names.
type umaMessage struct {
	From      string  `json:"from"`
	To        string  `json:"to"`
	Body      string  `json:"body,omitempty"`
	MessageID string  `json:"message_id"`
	Media     []Media `json:"media,omitempty"`
}

// webhook takes one backend webhook: authenticated (signature, or the path
// token for a backend that signs nothing) before the body is parsed, then
// each inbound message it carries is posted to uma. It answers 200 once uma
// took every message (or there was none), 401 for a request that fails
// authentication and 502 when uma did not take a message, so a backend that
// retries delivers it again.
func (g *Gateway) webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		answer(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if g.cfg.WebhookToken != "" {
		got, want := sha256.Sum256([]byte(r.PathValue("token"))), sha256.Sum256([]byte(g.cfg.WebhookToken))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			answer(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes+1))
	if err != nil || len(body) > maxWebhookBytes {
		answer(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
		return
	}
	if err := g.cfg.Backend.VerifyWebhook(r.Header, body); err != nil {
		g.log.Warn("webhook: refused", "backend", g.cfg.Backend.Name())
		answer(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	msgs, err := g.cfg.Backend.Messages(body)
	if err != nil {
		answer(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	for _, m := range msgs {
		if err := g.forward(r.Context(), m); err != nil {
			g.log.Warn("webhook: uma did not take a message", "backend", g.cfg.Backend.Name(), "message_id", m.MessageID)
			answer(w, http.StatusBadGateway, map[string]string{"error": "uma_unavailable"})
			return
		}
		g.log.Info("webhook: forwarded", "backend", g.cfg.Backend.Name(), "message_id", m.MessageID)
		if g.cfg.Forwarded != nil {
			g.cfg.Forwarded(body, m.From)
		}
	}
	answer(w, http.StatusOK, map[string]string{"status": "ok"})
}

// forward posts one message to uma's webhook, signed.
func (g *Gateway) forward(ctx context.Context, m Inbound) error {
	data, err := json.Marshal(umaMessage(m))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(g.cfg.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.WebhookURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(SignatureHeader, Sign(g.cfg.WebhookSecret, ts, data))
	resp, err := g.cfg.UmaClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return &statusError{resp.StatusCode}
	}
	return nil
}

// Sign is the signature the gateway sends uma: hex HMAC-SHA-256 under
// secret of the timestamp, a dot and the body.
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "uma answered http " + strconv.Itoa(e.code) }
