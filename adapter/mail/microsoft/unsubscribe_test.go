package microsoft

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/internal/unsubscribe"
	"hop.top/uma/adapter/mail/internal/unsubscribe/unsubscribetest"
	"hop.top/uma/model"
)

const (
	hUnsub = "List-Unsubscribe: "
	hPost  = "List-Unsubscribe-Post: List-Unsubscribe=One-Click"
	// newsletter is the sender of the messages these tests deliver.
	newsletter = "news@mail.example.com"
)

// unsubEnv is an env whose instance reaches the one-click stand-in and
// verifies DKIM against a locally generated key published for
// mail.example.com.
func unsubEnv(t *testing.T) (*env, *unsubscribetest.Target, *unsubscribetest.Signer) {
	t.Helper()
	e := newEnv(t, Config{})
	tg := unsubscribetest.NewTarget(t)
	s := unsubscribetest.NewSigner(t)
	e.inst.unsub = unsubscribe.Options{
		Public:   func(string) bool { return true },
		AnyPort:  true,
		Resolver: s.Resolver("mail.example.com"),
	}
	e.inst.unsubPost = unsubscribe.Poster{Resolver: tg.Resolve, Dial: tg.Dial, RootCAs: tg.RootCAs()}
	return e, tg, s
}

func unsubExt(data string) model.Extension {
	return model.Extension{Name: ExtensionUnsubscribe, Version: ExtensionVersion, Data: json.RawMessage(data)}
}

// deliverMIME stores a message from the newsletter whose MIME form Graph
// answers with raw.
func deliverMIME(e *env, id string, raw []byte, headers ...gHeader) {
	e.g.deliver(e.box.name, gMessage{
		ID: id, InternetMessageID: "<n1@mail.example.com>", Subject: "Weekly",
		From:                   &gRecipient{EmailAddress: gEmailAddress{Address: newsletter}},
		ToRecipients:           []gRecipient{{EmailAddress: gEmailAddress{Address: testMailbox}}},
		Body:                   &gItemBody{ContentType: "text", Content: "hello"},
		InternetMessageHeaders: headers,
	})
	e.g.mu.Lock()
	if e.g.mimes == nil {
		e.g.mimes = map[string][]byte{}
	}
	e.g.mimes[id] = raw
	e.g.mu.Unlock()
}

func unsubscribeMsg(t *testing.T, e *env, id string, raw []byte, data string) (adapter.Outcome, error) {
	t.Helper()
	deliverMIME(e, id, raw)
	return e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: effect(route(newsletter, id), "a1"), Action: unsubExt(data)})
}

func wantRejected(t *testing.T, out adapter.Outcome, err error, code model.ErrorCode, msg string) {
	t.Helper()
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != code || !strings.Contains(out.Failure.SafeMessage, msg) {
		t.Fatalf("Execute = %+v, %v; want rejected %s containing %q", out, err, code, msg)
	}
}

// graphWrites are the Graph requests that change the mailbox.
func graphWrites(e *env) []string {
	var out []string
	for _, r := range e.g.requests() {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

func TestExtensionsDeclareUnsubscribe(t *testing.T) {
	defs := NewFactory(Options{}).Extensions()
	i := slices.IndexFunc(defs, func(d adapter.ExtensionDefinition) bool { return d.Name == "microsoft.mail.unsubscribe" })
	if i < 0 {
		t.Fatalf("Extensions = %+v", defs)
	}
	d := defs[i]
	if d.Version != 1 || d.Kind != model.ExtensionKindEffect || d.RequiredCapability != model.CapabilityExtensionExecute || d.AuthorizeAs != model.ActionExtensionExecute {
		t.Errorf("definition = %+v", d)
	}
}

func TestValidateActionUnsubscribe(t *testing.T) {
	f := NewFactory(Options{})
	aud := model.Audience{Visibility: model.Internal}
	for _, c := range []struct{ name, data, field string }{
		{"no data", ``, ""},
		{"auto", `{"method":"auto"}`, ""},
		{"one click", `{"method":"one_click"}`, ""},
		{"mailto", `{"method":"mailto"}`, ""},
		{"unknown method", `{"method":"get"}`, "data.method"},
		{"not an object", `"mailto"`, "data"},
	} {
		t.Run(c.name, func(t *testing.T) {
			issues := f.ValidateAction(aud, unsubExt(c.data))
			switch {
			case c.field == "" && len(issues) != 0:
				t.Fatalf("issues = %+v", issues)
			case c.field != "" && (len(issues) != 1 || issues[0].Field != c.field || issues[0].Code != string(model.Validation)):
				t.Fatalf("issues = %+v, want one on %s", issues, c.field)
			}
		})
	}
}

func TestUnsubscribeOneClick(t *testing.T) {
	e, tg, s := unsubEnv(t)
	raw := s.Sign(t, unsubscribetest.Message(hUnsub+"<"+tg.URI("/u?id=7")+">, <mailto:unsub@example.com>", hPost), "mail.example.com", unsubscribetest.Covering)
	out, err := unsubscribeMsg(t, e, "m1", raw, ``)
	if err != nil || out.State != model.ProviderAccepted || !slices.Equal(out.References, []model.ExternalRef{model.ProviderMessageRef(testChan, "m1")}) {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if got := tg.Requests(); len(got) != 1 || got[0] != "POST /u?id=7 application/x-www-form-urlencoded cookie= auth=" || tg.Bodies()[0] != "List-Unsubscribe=One-Click" {
		t.Errorf("target saw %q body %q", got, tg.Bodies())
	}
	if w := graphWrites(e); len(w) != 0 {
		t.Errorf("Graph writes = %q, want none", w)
	}
}

func TestUnsubscribeOneClickAnswers(t *testing.T) {
	for _, c := range []struct {
		status int
		code   model.ErrorCode
		isErr  bool
	}{
		{http.StatusFound, model.Validation, false},
		{http.StatusNotFound, model.Permanent, false},
		{http.StatusTooManyRequests, model.Transient, true},
		{http.StatusBadGateway, model.Transient, true},
	} {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			e, tg, s := unsubEnv(t)
			tg.SetStatus(c.status)
			raw := s.Sign(t, unsubscribetest.Message(hUnsub+"<"+tg.URI("/u")+">", hPost), "mail.example.com", unsubscribetest.Covering)
			out, err := unsubscribeMsg(t, e, "m1", raw, `{"method":"one_click"}`)
			if c.isErr {
				wantCode(t, err, c.code)
			} else {
				wantRejected(t, out, err, c.code, "")
			}
			if got := tg.Requests(); len(got) != 1 {
				t.Errorf("target saw %q; a redirect must not be followed", got)
			}
		})
	}
}

// Mailto: the message is sent through the adapter's own draft-and-send, to
// the one address, with the URI's subject and body.
func TestUnsubscribeMailto(t *testing.T) {
	e, tg, _ := unsubEnv(t)
	raw := []byte(unsubscribetest.Message(hUnsub + "<https://u.example.net/o>, <mailto:Leave@lists.example.com?subject=Remove%20me&body=bye>"))
	out, err := unsubscribeMsg(t, e, "m1", raw, ``)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if !slices.Contains(out.References, model.ProviderMessageRef(testChan, "m1")) {
		t.Errorf("references = %+v", out.References)
	}
	if got := tg.Requests(); len(got) != 0 {
		t.Errorf("target saw %q for a mailto", got)
	}
	e.g.mu.Lock()
	var sent []gMessage
	for _, fm := range e.box.messages {
		if fm.folder == "sentitems" {
			sent = append(sent, fm.m)
		}
	}
	e.g.mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("sent = %d", len(sent))
	}
	m := sent[0]
	if len(m.ToRecipients) != 1 || m.ToRecipients[0].EmailAddress.Address != "Leave@lists.example.com" || m.Subject != "Remove me" || m.Body.Content != "bye" {
		t.Errorf("sent = %+v", m)
	}
}

func TestUnsubscribeRefusals(t *testing.T) {
	for _, c := range []struct {
		name    string
		headers func(tg *unsubscribetest.Target) []string
		data    string
		msg     string
	}{
		{"no header", func(*unsubscribetest.Target) []string { return nil }, ``, "no List-Unsubscribe header"},
		{"http, not https", func(*unsubscribetest.Target) []string { return []string{hUnsub + "<http://u.example.net/o>", hPost} }, ``, "http URI, not https"},
		{"other schemes", func(*unsubscribetest.Target) []string {
			return []string{hUnsub + "<ftp://u.example.net/o>, <tel:+15551234>", hPost}
		}, ``, "other schemes are never followed"},
		{"missing Post", func(tg *unsubscribetest.Target) []string { return []string{hUnsub + "<" + tg.URI("/u") + ">"} }, `{"method":"one_click"}`, "no List-Unsubscribe-Post"},
		{"mailto in another organization", func(*unsubscribetest.Target) []string { return []string{hUnsub + "<mailto:unsub@evil.example.org>"} }, ``, "not in the sender's organization"},
		{"mailto adds a recipient", func(*unsubscribetest.Target) []string {
			return []string{hUnsub + "<mailto:unsub@example.com?cc=victim@example.org>"}
		}, ``, "more recipients"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, tg, s := unsubEnv(t)
			raw := s.Sign(t, unsubscribetest.Message(c.headers(tg)...), "mail.example.com", unsubscribetest.Covering)
			out, err := unsubscribeMsg(t, e, "m1", raw, c.data)
			wantRejected(t, out, err, model.Validation, c.msg)
			if got := tg.Requests(); len(got) != 0 {
				t.Errorf("target saw %q", got)
			}
			if w := graphWrites(e); len(w) != 0 {
				t.Errorf("Graph writes = %q", w)
			}
		})
	}
}

func TestUnsubscribeDKIM(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  func(t *testing.T, tg *unsubscribetest.Target, s *unsubscribetest.Signer) []byte
		msg  string
	}{
		{"unsigned", func(t *testing.T, tg *unsubscribetest.Target, _ *unsubscribetest.Signer) []byte {
			return []byte(unsubscribetest.Message(hUnsub+"<"+tg.URI("/u")+">", hPost))
		}, "no valid DKIM signature"},
		{"Post not covered", func(t *testing.T, tg *unsubscribetest.Target, s *unsubscribetest.Signer) []byte {
			return s.Sign(t, unsubscribetest.Message(hUnsub+"<"+tg.URI("/u")+">", hPost), "mail.example.com", []string{"From", "To", "Subject", "Date", "List-Unsubscribe"})
		}, "covering List-Unsubscribe and List-Unsubscribe-Post"},
		{"re-encoded by the store", func(t *testing.T, tg *unsubscribetest.Target, s *unsubscribetest.Signer) []byte {
			signed := s.Sign(t, unsubscribetest.Message(hUnsub+"<"+tg.URI("/u")+">", hPost), "mail.example.com", unsubscribetest.Covering)
			return []byte(strings.Replace(string(signed), "hello", "hello ", 1))
		}, "no valid DKIM signature"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, tg, s := unsubEnv(t)
			out, err := unsubscribeMsg(t, e, "m1", c.raw(t, tg, s), ``)
			wantRejected(t, out, err, model.Validation, c.msg)
			if got := tg.Requests(); len(got) != 0 {
				t.Errorf("target saw %q", got)
			}
		})
	}
}

func TestUnsubscribeRefusesLocalTarget(t *testing.T) {
	e, tg, s := unsubEnv(t)
	e.inst.unsub = unsubscribe.Options{Resolver: s.Resolver("mail.example.com")}
	raw := s.Sign(t, unsubscribetest.Message(hUnsub+"<"+tg.URI("/u")+">", hPost), "mail.example.com", unsubscribetest.Covering)
	out, err := unsubscribeMsg(t, e, "m1", raw, ``)
	wantRejected(t, out, err, model.Validation, "default port")
	if len(tg.Requests()) != 0 {
		t.Errorf("target saw %q", tg.Requests())
	}
}

func TestUnsubscribeMessageGone(t *testing.T) {
	e, tg, _ := unsubEnv(t)
	out, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: effect(route(newsletter, "nope"), "a1"), Action: unsubExt(``)})
	wantRejected(t, out, err, model.NotFound, "gone")
	if len(tg.Requests()) != 0 {
		t.Errorf("target saw %q", tg.Requests())
	}
}

func TestUnsubscribeNoMessage(t *testing.T) {
	e, _, _ := unsubEnv(t)
	out, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: effect(route(newsletter, ""), "a1"), Action: unsubExt(``)})
	wantRejected(t, out, err, model.Validation, "no provider message")
	if n := len(e.g.requests()); n != 0 {
		t.Errorf("requests = %q", e.g.requests())
	}
}

func TestUnsubscribeVersion(t *testing.T) {
	e, _, _ := unsubEnv(t)
	act := unsubExt(``)
	act.Version = 9
	out, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: effect(route(newsletter, "m1"), "a1"), Action: act})
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.SchemaMismatch {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
}

// The headers are kept on the stored message: the inbound payload carries
// them as Graph reports them.
func TestInboundKeepsUnsubscribeHeaders(t *testing.T) {
	e := newEnv(t, Config{})
	m := gMessage{
		ID: "m1", InternetMessageID: "<n1@mail.example.com>", Subject: "Weekly",
		From:         &gRecipient{EmailAddress: gEmailAddress{Address: newsletter}},
		ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: testMailbox}}},
		Body:         &gItemBody{ContentType: "text", Content: "hello"},
		InternetMessageHeaders: []gHeader{
			{Name: "List-Unsubscribe", Value: "<https://u.example.net/o>, <mailto:u@example.com>"},
			{Name: "List-Unsubscribe-Post", Value: "List-Unsubscribe=One-Click"},
		},
	}
	ing := e.inst.normalize(m, nil, staged{})
	var p messagePayload
	if err := json.Unmarshal(ing.Envelope.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.ListUnsubscribe != "<https://u.example.net/o>, <mailto:u@example.com>" || p.ListUnsubscribePost != "List-Unsubscribe=One-Click" {
		t.Errorf("payload = %+v", p)
	}
}
