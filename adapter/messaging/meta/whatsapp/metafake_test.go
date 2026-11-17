package whatsapp_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"hop.top/uma/adapter/messaging/meta"
	"hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// fakeMeta is a Graph API server replaying the documented Cloud API send
// message request and response shapes. Each phone number ID is one
// business account; scripted answers are consumed in call order and the
// server accepts once they run out.
type fakeMeta struct {
	srv *httptest.Server

	mu       sync.Mutex
	accounts map[string]*account // by phone number ID
	byChan   map[model.ChannelRef]*account
	seq      int
	requests []recorded
	// media holds downloadable media by ID; mediaURL, when set, is the
	// download URL every lookup answers instead of the server's own.
	media    map[string][]byte
	mediaURL string
}

// putMedia makes data downloadable under media ID id.
func (f *fakeMeta) putMedia(id string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.media == nil {
		f.media = map[string][]byte{}
	}
	f.media[id] = data
}

// serveMedia answers the media lookup (GET /{version}/{media-id}) and the
// download (GET /media/{media-id}) of the Cloud API media endpoints. It
// reports whether the request was one of them.
func (f *fakeMeta) serveMedia(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if r.Method != http.MethodGet || len(parts) != 2 {
		return false
	}
	f.mu.Lock()
	data, ok := f.media[parts[1]]
	override := f.mediaURL
	f.mu.Unlock()
	switch {
	case !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "):
		graphError(w, http.StatusUnauthorized, 190, "Invalid OAuth access token")
	case !ok:
		graphError(w, http.StatusNotFound, 100, "Unsupported get request")
	case parts[0] == "media":
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(data)
	default:
		u := f.srv.URL + "/media/" + parts[1]
		if override != "" {
			u = override
		}
		sum := sha256.Sum256(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"messaging_product":"whatsapp","url":%q,"mime_type":"image/jpeg","sha256":%q,"file_size":%d,"id":%q}`, u, hex.EncodeToString(sum[:]), len(data), parts[1]) // #nosec G705 -- test server; values quoted as JSON strings, served as application/json
	}
	return true
}

type account struct {
	ch      model.ChannelRef
	pnid    string
	script  []conformance.Response
	calls   []conformance.ProviderCall
	queued  []conformance.InboundEvent
	inbound []conformance.InboundResult
	// held is the emoji each user's reaction on each message holds, by
	// "user|wamid": WhatsApp keeps one per user and message, and removes it
	// with an empty emoji.
	held map[string]string
}

// recorded is one request as the server saw it.
type recorded struct {
	Path   string
	Auth   string
	Body   map[string]any
	Header http.Header
}

func newFakeMeta(t *testing.T) *fakeMeta {
	t.Helper()
	f := &fakeMeta{accounts: map[string]*account{}, byChan: map[model.ChannelRef]*account{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMeta) factory() *whatsapp.Factory {
	return whatsapp.NewFactory(whatsapp.Options{BaseURL: f.srv.URL})
}

// client is the HTTP client reaching the server.
func (f *fakeMeta) client() *http.Client {
	return &http.Client{Transport: unreachable{f: f, next: f.srv.Client().Transport}}
}

// unreachable fails the dial of a send whose scripted answer is
// conformance.OutcomeTransient: the request never reaches the server, the
// one way a send provably applied nothing.
type unreachable struct {
	f    *fakeMeta
	next http.RoundTripper
}

func (u unreachable) RoundTrip(r *http.Request) (*http.Response, error) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	u.f.mu.Lock()
	var a *account
	if len(parts) == 3 {
		a = u.f.accounts[parts[1]]
	}
	if a == nil || len(a.script) == 0 || a.script[0].Outcome != conformance.OutcomeTransient {
		u.f.mu.Unlock()
		return u.next.RoundTrip(r)
	}
	a.script = a.script[1:]
	call := conformance.ProviderCall{Capability: model.CapabilitySend}
	if r.Body != nil {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		call = a.callOf(req)
	}
	a.calls = append(a.calls, call)
	u.f.mu.Unlock()
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

// account returns ch's account, creating it with the next phone number ID.
func (f *fakeMeta) account(ch model.ChannelRef) *account {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byChan[ch]
	if !ok {
		a = &account{ch: ch, pnid: strconv.Itoa(106540352242922 + len(f.byChan))}
		f.byChan[ch] = a
		f.accounts[a.pnid] = a
	}
	return a
}

func (f *fakeMeta) recordedRequests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

func graphError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"message":"(#%d) %s","type":"OAuthException","code":%d,"error_data":{"messaging_product":"whatsapp","details":"%s"},"fbtrace_id":"AazmGlsnSLXMk9sMwJU8KZm"}}`, code, msg, code, msg)
}

func (f *fakeMeta) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	f.mu.Lock()
	f.requests = append(f.requests, recorded{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: req, Header: r.Header.Clone()})
	f.mu.Unlock()
	if f.serveMedia(w, r, parts) {
		return
	}
	f.mu.Lock()
	var a *account
	if len(parts) == 3 && parts[2] == "messages" && r.Method == http.MethodPost {
		a = f.accounts[parts[1]]
	}
	if a == nil {
		f.mu.Unlock()
		graphError(w, http.StatusBadRequest, 100, "Unsupported post request")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || len(r.Header.Get("Authorization")) == len("Bearer ") {
		f.mu.Unlock()
		graphError(w, http.StatusUnauthorized, 190, "Invalid OAuth access token")
		return
	}
	resp := conformance.Response{Outcome: conformance.OutcomeAccepted}
	if len(a.script) > 0 {
		resp, a.script = a.script[0], a.script[1:]
	}
	call := a.callOf(req)
	to, _ := req["to"].(string)
	applied := resp.Outcome == conformance.OutcomeAccepted || resp.Outcome == conformance.OutcomeAnswerLost || resp.Outcome == outcomeServerFault
	var wamid string
	if applied {
		f.seq++
		wamid = fmt.Sprintf("wamid.HBgLMTY1MDM4Nzk0MzkVAgARGBI%04d", f.seq)
		call.Applied = true
		call.References = []model.ExternalRef{model.ProviderMessageRef(a.ch, wamid)}
		if call.Capability == model.CapabilityReact {
			a.hold(to, call)
		}
	}
	a.calls = append(a.calls, call)
	f.mu.Unlock()

	switch resp.Outcome {
	case conformance.OutcomeAccepted:
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"messaging_product":"whatsapp","contacts":[{"input":%q,"wa_id":%q}],"messages":[{"id":%q,"message_status":"accepted"}]}`, to, to, wamid)
	case conformance.OutcomeRejected:
		graphError(w, http.StatusBadRequest, 131026, "Message undeliverable")
	case outcomeServerFault:
		graphError(w, http.StatusInternalServerError, 131000, "Something went wrong")
	case conformance.OutcomeRateLimited:
		if resp.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(resp.RetryAfter.Seconds())))
		}
		graphError(w, http.StatusBadRequest, 130429, "Rate limit hit")
	case conformance.OutcomeAnswerLost, conformance.OutcomeRequestLost:
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
}

// callOf is the conformance view of a request to the messages endpoint: a
// send, or a reaction on a message, with the operation and attempt its
// callback data names. A removal (an empty emoji) names the emoji the user
// holds, as the provider knows it. Callers hold f.mu.
func (a *account) callOf(req map[string]any) conformance.ProviderCall {
	call := conformance.ProviderCall{Capability: model.CapabilitySend, Content: sentContent(req)}
	to, _ := req["to"].(string)
	call.Route = model.ExternalRef{Channel: a.ch, Namespace: whatsapp.Namespace, Kind: whatsapp.RouteKind, ID: to}
	if cb, _ := req["biz_opaque_callback_data"].(string); cb != "" {
		if op, at, ok := whatsapp.ParseCallbackData(cb); ok {
			call.Operation, call.Attempt = model.OperationRef{Tenant: a.ch.Tenant, ID: op}, at
		}
	}
	if rx, ok := req["reaction"].(map[string]any); ok && req["type"] == "reaction" {
		id, _ := rx["message_id"].(string)
		emoji, _ := rx["emoji"].(string)
		call.Capability, call.Target = model.CapabilityReact, model.ProviderMessageRef(a.ch, id)
		call.Emoji, call.Remove = emoji, emoji == ""
		if call.Remove {
			call.Emoji = a.held[to+"|"+id]
		}
	}
	return call
}

// hold records the effect of an applied reaction call.
func (a *account) hold(user string, call conformance.ProviderCall) {
	if a.held == nil {
		a.held = map[string]string{}
	}
	key := user + "|" + call.Target.ID
	if call.Remove {
		delete(a.held, key)
		return
	}
	a.held[key] = call.Emoji
}

// sentContent reads the content a send request carried.
func sentContent(req map[string]any) model.Content {
	if text, ok := req["text"].(map[string]any); ok {
		body, _ := text["body"].(string)
		return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: body}}}
	}
	if tpl, ok := req["template"]; ok {
		data, _ := json.Marshal(tpl)
		e := model.Extension{Name: whatsapp.TemplateSchema, Version: whatsapp.TemplateVersion, Data: data}
		return model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &e}}}
	}
	return model.Content{}
}

// provider is the conformance view of fakeMeta.
type provider struct{ f *fakeMeta }

// HTTPClient is the client the suite's engine injects.
func (p provider) HTTPClient() ports.HTTPClient { return p.f.client() }

func (p provider) Account(ch model.ChannelRef) conformance.Account {
	return conformance.Account{
		Configuration: whatsapp.Config{PhoneNumberID: p.f.account(ch).pnid}.Extension(),
		CredentialRef: "meta-app",
	}
}

func (p provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: whatsapp.Namespace, Kind: whatsapp.RouteKind, ID: peer}
}

// ShapeID maps the suite's destination to the account's own phone number:
// Meta addresses every notification to the number it arrived on.
func (p provider) ShapeID(ch model.ChannelRef, kind conformance.FixtureKind, id string) string {
	if kind == conformance.FixtureDestination && id == "support" {
		return p.f.account(ch).pnid
	}
	return id
}

// Deliver queues ev for Push as one text message or one reaction message.
// WhatsApp messages have no private audience, and it sends no edit,
// deletion or reaction count.
func (p provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if ev.Visibility != "" && ev.Visibility != model.Public {
		return conformance.ErrNotExpressible
	}
	switch ev.Kind {
	case conformance.InboundMessage, conformance.InboundReactionAdded, conformance.InboundReactionRemoved:
	default:
		return conformance.ErrNotExpressible
	}
	a := p.f.account(ch)
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	a.queued = append(a.queued, ev)
	return nil
}

// Push posts each queued event as a signed notification addressed to its
// destination phone number, as Meta calls the subscribed webhook.
func (p provider) Push(ch model.ChannelRef, h http.Handler) error {
	a := p.f.account(ch)
	p.f.mu.Lock()
	evs := a.queued
	a.queued = nil
	p.f.mu.Unlock()
	for _, ev := range evs {
		msg := fmt.Sprintf(`{"from":%q,"id":%q,"timestamp":"1700000000","type":"text","text":{"body":%q}}`, ev.Sender, ev.MessageID, ev.Text)
		switch ev.Kind {
		case conformance.InboundReactionAdded:
			msg = fmt.Sprintf(`{"from":%q,"id":%q,"timestamp":"1700000000","type":"reaction","reaction":{"message_id":%q,"emoji":%q}}`, ev.Sender, ev.EventID, ev.MessageID, ev.Emoji)
		case conformance.InboundReactionRemoved:
			// WhatsApp names no emoji for a removal.
			msg = fmt.Sprintf(`{"from":%q,"id":%q,"timestamp":"1700000000","type":"reaction","reaction":{"message_id":%q}}`, ev.Sender, ev.EventID, ev.MessageID)
		}
		body := fmt.Appendf(nil, `{"object":"whatsapp_business_account","entry":[{"id":"102290129340398","changes":[{"value":{"messaging_product":"whatsapp","metadata":{"display_phone_number":"15550783881","phone_number_id":%q},"contacts":[{"profile":{"name":"Peer"},"wa_id":%q}],"messages":[%s]},"field":"messages"}]}]}`,
			ev.Destination, ev.Sender, msg)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", bytes.NewReader(body))
		req.Header.Set(meta.SignatureHeader, meta.Sign([]byte(conformance.CredentialSecret), body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		p.f.mu.Lock()
		a.inbound = append(a.inbound, conformance.InboundResult{EventID: ev.EventID, Acked: rec.Code == http.StatusOK})
		p.f.mu.Unlock()
	}
	return nil
}

func (p provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	a := p.f.account(ch)
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	return append([]conformance.InboundResult(nil), a.inbound...)
}

func (p provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	a := p.f.account(ch)
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	a.script = append(a.script, rs...)
	return nil
}

func (p provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	a := p.f.account(ch)
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	return append([]conformance.ProviderCall(nil), a.calls...)
}

// Revoke is not expressible: the adapter learns of a withdrawn permission
// only from a refused call.
func (provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}
