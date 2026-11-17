package imessage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// BlooioBaseURL is the Blooio v4 API (https://docs.blooio.com/llms.txt).
const BlooioBaseURL = "https://api.blooio.com/v4"

// BlooioSignatureHeader carries Blooio's webhook signature:
// "t=<unix seconds>,v1=<hex HMAC-SHA-256 of '<t>.<raw body>'>"
// (https://docs.blooio.com/guides/webhook-signatures).
const BlooioSignatureHeader = "X-Blooio-Signature"

// blooioTolerance is the webhook timestamp window Blooio's guide uses.
const blooioTolerance = 5 * time.Minute

// BlooioOptions configure the Blooio backend.
type BlooioOptions struct {
	// BaseURL is the v4 API; empty is BlooioBaseURL.
	BaseURL string
	// APIKey is the bearer API key. Required; never logged.
	APIKey string
	// From is the sender: the line's number in E.164, as POST /messages
	// takes it. Required.
	From string
	// WebhookSecret is the webhook's signing secret (whsec_...), as
	// POST /webhooks returned it. Required.
	WebhookSecret []byte
	// Client is the HTTP client. Nil is http.DefaultClient.
	Client *http.Client
	// Now is the clock for the signature window; nil is time.Now.
	Now func() time.Time
}

// Blooio is the Blooio backend: POST /messages with Blooio's own
// Idempotency-Key, webhooks signed with X-Blooio-Signature.
type Blooio struct{ o BlooioOptions }

// NewBlooio checks o and returns the backend.
func NewBlooio(o BlooioOptions) (*Blooio, error) {
	switch {
	case o.APIKey == "":
		return nil, errors.New("blooio: API key required")
	case o.From == "":
		return nil, errors.New("blooio: sender number required")
	case len(o.WebhookSecret) == 0:
		return nil, errors.New("blooio: webhook signing secret required")
	}
	if o.BaseURL == "" {
		o.BaseURL = BlooioBaseURL
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Blooio{o: o}, nil
}

// Name is "blooio".
func (*Blooio) Name() string { return "blooio" }

// SignsWebhooks is true.
func (*Blooio) SignsWebhooks() bool { return true }

// Send posts {"from","to","text"} to /messages with the relay's key as
// Blooio's Idempotency-Key: Blooio answers a repeat of the key with the
// original message instead of sending again. Answers:
//
//   - 200, 201, 202: sent, with the message's id (id, or message_id on the
//     older shape);
//   - 401, 503 (no sender available): not sent; 429: not sent, rate
//     limited, with Retry-After;
//   - 207 (partly sent), 408, 409 (the key with another body) and any other
//     5xx: unknown;
//   - any other 4xx: rejected.
func (b *Blooio) Send(ctx context.Context, m Outbound) Result {
	data, err := json.Marshal(map[string]string{"from": b.o.From, "to": m.To, "text": m.Text})
	if err != nil {
		return Result{Outcome: Rejected, Detail: "blooio: message cannot be encoded"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.o.BaseURL+"/messages", bytes.NewReader(data))
	if err != nil {
		return Result{Outcome: Rejected, Detail: "blooio: request cannot be built"}
	}
	req.Header.Set("Authorization", "Bearer "+b.o.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Idempotency-Key", m.Key)
	resp, err := b.o.Client.Do(req)
	if err != nil {
		return classify(err, "blooio")
	}
	defer func() { _ = resp.Body.Close() }()
	code := resp.StatusCode
	detail := "blooio: http " + strconv.Itoa(code)
	switch {
	case code == http.StatusOK || code == http.StatusCreated || code == http.StatusAccepted:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return Result{Outcome: Sent, MessageID: blooioID(body), Detail: detail}
	case code == http.StatusTooManyRequests:
		return Result{Outcome: NotSent, RateLimited: true, RetryAfter: retryAfter(resp.Header, b.o.Now()), Detail: detail}
	case code == http.StatusUnauthorized || code == http.StatusServiceUnavailable:
		return Result{Outcome: NotSent, Detail: detail}
	case code == http.StatusMultiStatus || code == http.StatusRequestTimeout || code == http.StatusConflict || code >= 500:
		return Result{Outcome: Unknown, Detail: detail}
	}
	return Result{Outcome: Rejected, Detail: detail}
}

// blooioID reads the message id from a send answer: the v4 message object's
// id, else the older message_id.
func blooioID(body []byte) string {
	var m struct {
		ID        string `json:"id"`
		MessageID string `json:"message_id"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	if m.ID != "" {
		return m.ID
	}
	return m.MessageID
}

// errSignature is every webhook signature failure; the cause stays out of
// the answer.
var errSignature = errors.New("webhook signature refused")

// VerifyWebhook checks X-Blooio-Signature: a timestamp within five minutes
// of now and a v1 HMAC-SHA-256 of "<t>.<raw body>" under the signing secret.
func (b *Blooio) VerifyWebhook(h http.Header, body []byte) error {
	vals := h.Values(BlooioSignatureHeader)
	if len(vals) != 1 {
		return errSignature
	}
	var ts string
	var sigs [][]byte
	for part := range strings.SplitSeq(vals[0], ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			if sig, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, sig)
			}
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return errSignature
	}
	if d := b.o.Now().Sub(time.Unix(sec, 0)); d > blooioTolerance || d < -blooioTolerance {
		return errSignature
	}
	mac := hmac.New(sha256.New, b.o.WebhookSecret)
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write(body)
	want := mac.Sum(nil)
	ok := false
	for _, s := range sigs {
		if hmac.Equal(s, want) {
			ok = true
		}
	}
	if !ok {
		return errSignature
	}
	return nil
}

// blooioEvent is the v4 webhook envelope (payload version 2026-10-01).
type blooioEvent struct {
	Type string `json:"type"`
	Data struct {
		MessageID      string          `json:"message_id"`
		Direction      string          `json:"direction"`
		Sender         string          `json:"sender"`
		Recipient      string          `json:"recipient"`
		ChannelAddress string          `json:"channel_address"`
		Text           string          `json:"text"`
		Group          json.RawMessage `json:"group"`
		Attachments    []struct {
			URL       string `json:"url"`
			MediaType string `json:"media_type"`
		} `json:"attachments"`
	} `json:"data"`
}

// Messages maps a message.received event of a one-to-one chat. Every other
// event type (sent, delivered, read, failed, reactions, typing), an outbound
// message (the line's own echo) and a group message carry nothing.
func (b *Blooio) Messages(body []byte) ([]Inbound, error) {
	var e blooioEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, err
	}
	d := e.Data
	switch {
	case e.Type != "message.received", d.Direction == "outbound",
		len(d.Group) > 0 && string(d.Group) != "null":
		return nil, nil
	}
	m := Inbound{From: d.Sender, To: d.Recipient, Body: d.Text, MessageID: d.MessageID}
	if m.To == "" {
		m.To = d.ChannelAddress
	}
	for _, a := range d.Attachments {
		if a.URL != "" {
			m.Media = append(m.Media, Media{URL: a.URL, ContentType: a.MediaType})
		}
	}
	if m.From == "" || m.To == "" || m.MessageID == "" || (strings.TrimSpace(m.Body) == "" && len(m.Media) == 0) {
		return nil, nil
	}
	return []Inbound{m}, nil
}

var _ Backend = (*Blooio)(nil)
