package relay

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Bounds on one inbound message.
const (
	maxAddressBytes   = 128
	maxMessageIDBytes = 255
	maxMedia          = 10
	maxMediaURLBytes  = 2048
	maxContentType    = 255
)

// acceptedBody answers a message the engine took, new or duplicate, and a
// status update it applied to an attempt.
const acceptedBody = `{"status":"accepted"}`

// unmatchedBody answers a status update the engine took but applied to no
// attempt: it named no operation, or an operation or attempt the engine does
// not hold.
const unmatchedBody = `{"status":"accepted","matched":false}`

// Handler returns the handler the gateway posts to. Before the sink sees
// anything, and before a byte of the body is parsed, webhookauth
// authenticates the request under the channel's configuration, reading at
// most the body limit. It answers:
//
//   - 405 (Allow: POST) for any other method, before any secret is read;
//   - 401, 400, 409 or 413 for a request that fails authentication, as
//     webhookauth.Status maps it: a missing or wrong credential or a stale
//     timestamp, a malformed header, a replayed delivery, a body over the
//     limit;
//   - 400 for an authenticated body that is not a usable message: not a JSON
//     object, a required field missing or mistyped, a bad media entry; the
//     same for a body that is not a usable status update when the channel
//     enables them (see Instance.statusEnvelope);
//   - 503 when the instance is closed, when no secret or replay store can be
//     reached, and for a retryable sink failure; 422 for any other sink
//     failure;
//   - 200 with {"status":"accepted"} once the sink accepted the message, new
//     or duplicate. A status update the engine took but applied to no
//     attempt (it named none, or an operation or attempt the engine does
//     not hold) is answered {"status":"accepted","matched":false}: still
//     200, so a gateway does not retry what cannot succeed.
//
// A delivery that is not accepted releases its replay reservation, so the
// gateway's retry is a new attempt. The answer carries no reply: replies go
// to the callback (Send).
//
// When the channel enables status updates (Config.Inbound.Status), a body
// holding the status key is a delivery status update, not a message: a
// status envelope whose delivery report the engine applies to the attempt it
// names. It is authenticated, deduplicated and answered like a message.
func (i *Instance) Handler(sink adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			status(w, http.StatusMethodNotAllowed)
			return
		}
		if i.isClosed() {
			status(w, http.StatusServiceUnavailable)
			return
		}
		body, res, err := i.verifier.VerifyHTTP(r)
		if err != nil {
			i.verifier.Reject(w, err)
			return
		}
		fail := func(code int) {
			_ = res.Release(r.Context())
			status(w, code)
		}
		env, err := i.ingress(body)
		if err != nil {
			fail(http.StatusBadRequest)
			return
		}
		receipt, err := sink.Accept(r.Context(), model.Ingress{
			Call:     model.Call{Correlation: model.CorrelationID("relay-" + env.EventID)},
			Envelope: env,
		})
		if err != nil {
			if model.CodeOf(err).DefaultRetry() == model.RetrySameOperation {
				fail(http.StatusServiceUnavailable)
				return
			}
			fail(http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if env.Kind == model.EnvelopeStatus && receipt.Operation == nil {
			_, _ = io.WriteString(w, unmatchedBody)
			return
		}
		_, _ = io.WriteString(w, acceptedBody)
	})
}

func status(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}

var errBadMessage = errors.New("relay: not a usable message")

// ingress normalizes an authenticated body: a status update when the
// channel enables them and the body carries the status key, else a message.
func (i *Instance) ingress(body []byte) (model.Envelope, error) {
	fields, err := objectOf(body)
	if err != nil {
		return model.Envelope{}, err
	}
	if i.cfg.Inbound.Status.Enabled {
		if _, ok := pick(fields, i.cfg.status.Status); ok {
			return i.statusEnvelope(fields)
		}
	}
	return i.envelope(fields)
}

// envelope normalizes the fields of a message.
func (i *Instance) envelope(fields map[string]json.RawMessage) (model.Envelope, error) {
	f := i.cfg.fields
	from, ok := stringOf(fields, f.From, validAddress)
	if !ok {
		return model.Envelope{}, errBadMessage
	}
	to, ok := stringOf(fields, f.To, validAddress)
	if !ok {
		return model.Envelope{}, errBadMessage
	}
	from, to = i.handle(from), i.handle(to)
	text, _, ok := optionalString(fields, f.Body)
	if !ok {
		return model.Envelope{}, errBadMessage
	}
	media, ok := mediaOf(fields, f.Media)
	if !ok {
		return model.Envelope{}, errBadMessage
	}
	var content model.Content
	if strings.TrimSpace(text) != "" {
		content.Parts = append(content.Parts, model.Part{Kind: model.PartText, Text: text})
	}
	if len(media) > 0 {
		data, err := json.Marshal(struct {
			Items []mediaItem `json:"items"`
		}{media})
		if err != nil {
			return model.Envelope{}, errBadMessage
		}
		content.Parts = append(content.Parts, model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: PayloadMedia, Version: 1, Data: data}})
	}
	if len(content.Parts) == 0 {
		return model.Envelope{}, errBadMessage
	}
	id, found, ok := optionalString(fields, f.MessageID)
	if !ok || (found && !validMessageID(id)) {
		return model.Envelope{}, errBadMessage
	}
	eventID := ""
	switch {
	case found && id != "":
		eventID = digest("event", from, id)
	case i.cfg.Inbound.GenerateMissingID:
		var err error
		if id, err = randomID(); err != nil {
			return model.Envelope{}, err
		}
		eventID = id
	default:
		return model.Envelope{}, errBadMessage
	}
	return model.Envelope{
		EventID: eventID, MessageID: id, Destination: to, Kind: model.EnvelopeMessage,
		Sender:     model.ExternalRef{Channel: i.channel.Ref, Namespace: i.namespace, Kind: RouteKind, ID: from},
		Assurance:  "verified",
		Content:    content,
		Audience:   model.Audience{Visibility: model.Public},
		OccurredAt: i.clock.Now().UTC(),
	}, nil
}

// objectOf decodes body as exactly one JSON object.
func objectOf(body []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, errBadMessage
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errBadMessage
	}
	return m, nil
}

// pick returns the value of the first key of names the object holds.
func pick(m map[string]json.RawMessage, names []string) (json.RawMessage, bool) {
	for _, n := range names {
		if v, ok := m[n]; ok {
			return v, true
		}
	}
	return nil, false
}

// stringOf reads a required string field that satisfies valid.
func stringOf(m map[string]json.RawMessage, names []string, valid func(string) bool) (string, bool) {
	s, found, ok := optionalString(m, names)
	return s, found && ok && valid(s)
}

// optionalString reads a string field: found says a key was present, ok that
// its value is a string (JSON null is no value).
func optionalString(m map[string]json.RawMessage, names []string) (s string, found, ok bool) {
	raw, found := pick(m, names)
	if !found || string(bytes.TrimSpace(raw)) == "null" {
		return "", false, true
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", true, false
	}
	return s, true, true
}

// validAddress reports whether s can be a phone-number identity: kept
// exactly as sent, so only what cannot be an identifier is refused.
func validAddress(s string) bool {
	return s != "" && len(s) <= maxAddressBytes && printable(s)
}

// handle returns address as the identity it names on this channel's medium;
// see handleOf.
func (i *Instance) handle(address string) string { return handleOf(i.namespace, address) }

// handleOf returns address as the identity it names in namespace. On iMessage
// an address is a phone number or an Apple ID email address, and Apple IDs
// ignore case, so an email address (one holding "@", which no number does) is
// lowercased, ASCII only: one Apple ID stays one identity whatever case the
// relay reports or a start names. Every other address, and every other
// namespace, is kept exactly as sent. Lowercasing ASCII keeps length and
// printability, so a valid address stays valid.
func handleOf(namespace, address string) string {
	if namespace != NamespaceIMessage || !strings.Contains(address, "@") {
		return address
	}
	b := []byte(address)
	for k, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[k] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func validMessageID(s string) bool {
	return s != "" && len(s) <= maxMessageIDBytes && printable(s)
}

func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// mediaItem is one entry of the PayloadMedia part. The adapter never fetches
// the URL; the sender chose it.
type mediaItem struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type,omitempty"`
}

// mediaOf reads the media list: an array of URL strings or of objects with
// url and content_type. Absent or null is none.
func mediaOf(m map[string]json.RawMessage, names []string) ([]mediaItem, bool) {
	raw, found := pick(m, names)
	if !found {
		return nil, true
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) > maxMedia {
		return nil, false
	}
	out := make([]mediaItem, 0, len(entries))
	for _, e := range entries {
		var it mediaItem
		if err := json.Unmarshal(e, &it.URL); err != nil {
			var obj struct {
				URL         *string `json:"url"`
				ContentType *string `json:"content_type"`
			}
			if err := json.Unmarshal(e, &obj); err != nil || obj.URL == nil {
				return nil, false
			}
			it.URL = *obj.URL
			if obj.ContentType != nil {
				it.ContentType = *obj.ContentType
			}
		}
		if !validMediaURL(it.URL) || len(it.ContentType) > maxContentType || !printable(it.ContentType) {
			return nil, false
		}
		out = append(out, it)
	}
	return out, true
}

// validMediaURL reports whether raw is an http(s) URL naming a host and no
// credentials.
func validMediaURL(raw string) bool {
	if len(raw) > maxMediaURLBytes || !printable(raw) {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// digest is a length-prefixed hash of fields: stable, collision-free across
// field boundaries, and short enough for an event ID.
func digest(fields ...string) string {
	h := sha256.New()
	for _, s := range fields {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	return "r" + hex.EncodeToString(h.Sum(nil)[:16])
}

// randomID is a new opaque message ID: 128 random bits.
func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", model.NewError(model.Unavailable, "no randomness")
	}
	return "g" + hex.EncodeToString(b[:]), nil
}

var _ adapter.HTTPReceiver = (*Instance)(nil)
