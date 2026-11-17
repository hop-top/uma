package whatsapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/whatsapp"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func whatsappFactory(fake *fakeTwilio) *whatsapp.Factory {
	return whatsapp.NewFactory(whatsapp.Options{BaseURL: fake.srv.URL, Timeout: 5 * time.Second})
}

// otherWhatsApp stands in for the meta.whatsapp adapter, which lives in
// another module: the same capabilities on its own type key, schema and
// namespace. Its instances report send withdrawn, so capability discovery
// shows which adapter answered.
type otherWhatsApp struct {
	mu    sync.Mutex
	sends int
}

const otherSchema = "meta.whatsapp.config"

func (o *otherWhatsApp) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: model.TypeMetaWhatsapp, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: otherSchema, Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend},
		Events:              []string{model.EventMessageReceived, model.EventDeliveryUpdated},
		IdentityNamespaces:  []string{"meta.whatsapp"},
		Maturity:            "experimental",
	}
}

func (o *otherWhatsApp) Validate(e model.Extension) []model.ValidationIssue {
	var cfg struct {
		PhoneNumberID string `json:"phone_number_id"`
	}
	if e.Name != otherSchema || e.Version != 1 || json.Unmarshal(e.Data, &cfg) != nil || cfg.PhoneNumberID == "" {
		return []model.ValidationIssue{{Field: "configuration", Code: string(model.SchemaMismatch), Message: "want " + otherSchema}}
	}
	return nil
}

func (o *otherWhatsApp) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return otherInstance{o}, nil
}

type otherInstance struct{ o *otherWhatsApp }

func (otherInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{
		{Name: model.CapabilityReceiveHTTP, Available: true},
		{Name: model.CapabilitySend, Available: false, Reason: model.Unauthorized},
	}, nil
}

func (otherInstance) Close(context.Context) error { return nil }

func (i otherInstance) Send(context.Context, adapter.SendRequest) (adapter.Outcome, error) {
	i.o.mu.Lock()
	defer i.o.mu.Unlock()
	i.o.sends++
	return adapter.Outcome{}, model.NewError(model.Unauthorized, "withdrawn")
}

// otherHookStatus marks an answer of the stand-in's webhook.
const otherHookStatus = http.StatusTeapot

func (otherInstance) Handler(adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(otherHookStatus) })
}

// twilio.whatsapp and a meta.whatsapp adapter registered in one engine keep
// distinct configuration, routing and capability discovery.
func TestCoexistsWithMetaWhatsApp(t *testing.T) {
	fake := newFakeTwilio(t)
	other := &otherWhatsApp{}
	h := newHost(t, fake.client(), whatsappFactory(fake), other)

	descs := map[model.TypeKey]adapter.Descriptor{}
	for _, d := range h.engine.Describe() {
		descs[d.Type] = d
	}
	tw, mw := descs[model.TypeTwilioWhatsapp], descs[model.TypeMetaWhatsapp]
	if tw.ConfigurationSchema.Name != whatsapp.ConfigSchema || mw.ConfigurationSchema.Name != otherSchema ||
		len(tw.IdentityNamespaces) != 1 || tw.IdentityNamespaces[0] != whatsapp.Namespace || mw.IdentityNamespaces[0] == whatsapp.Namespace {
		t.Fatalf("descriptors %+v / %+v", tw, mw)
	}

	metaCfg := model.Extension{Name: otherSchema, Version: 1, Data: []byte(`{"phone_number_id":"123456"}`)}
	if issues := h.engine.ValidateChannel(model.TypeTwilioWhatsapp, hookConfig().Extension()); len(issues) != 0 {
		t.Fatalf("twilio config on twilio.whatsapp: %+v", issues)
	}
	if issues := h.engine.ValidateChannel(model.TypeTwilioWhatsapp, metaCfg); len(issues) == 0 {
		t.Fatal("meta config accepted by twilio.whatsapp")
	}
	if issues := h.engine.ValidateChannel(model.TypeMetaWhatsapp, hookConfig().Extension()); len(issues) == 0 {
		t.Fatal("twilio config accepted by meta.whatsapp")
	}

	metaChannel := model.ChannelRef{Tenant: hostTenant, ID: "00000000-0000-7000-8000-0000000008c0"}
	metaInbox := model.InboxRef{Tenant: hostTenant, ID: "00000000-0000-7000-8000-0000000008d2"}
	twConv := h.channel(hostChannel, model.TypeTwilioWhatsapp, hookConfig().Extension(), hostInbox, senderNumber, hostRoute)
	h.channel(metaChannel, model.TypeMetaWhatsapp, metaCfg, metaInbox, "123456",
		model.ExternalRef{Channel: metaChannel, Namespace: "meta.whatsapp", Kind: "user", ID: "15558675310"})

	caps := func(ch model.ChannelRef) map[model.Capability]bool {
		t.Helper()
		st, err := h.client().Configuration().Capabilities(h.ctx, model.Lookup[model.ChannelRef]{Call: model.Call{Correlation: "caps"}, Ref: ch})
		if err != nil {
			t.Fatal(err)
		}
		out := map[model.Capability]bool{}
		for _, s := range st {
			out[s.Name] = s.Available
		}
		return out
	}
	if c := caps(hostChannel); !c[model.CapabilitySend] || !c[model.CapabilityReceiveHTTP] {
		t.Fatalf("twilio.whatsapp capabilities %v", c)
	}
	if c := caps(metaChannel); c[model.CapabilitySend] || !c[model.CapabilityReceiveHTTP] {
		t.Fatalf("meta.whatsapp capabilities %v, want its own answer (send withdrawn)", c)
	}

	// Inbound routing: each channel's webhook is its own adapter's.
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: hostTenant, Actor: "hooks"}, Call: model.Call{Correlation: "hooks"}}
	twHook, err := h.engine.Runtime().Webhook(h.ctx, call, hostChannel)
	if err != nil {
		t.Fatal(err)
	}
	metaHook, err := h.engine.Runtime().Webhook(h.ctx, call, metaChannel)
	if err != nil {
		t.Fatal(err)
	}
	body := fixture(t, "incoming_text.form")
	if rec := post(twHook, "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("twilio webhook = %d %q", rec.Code, rec.Body)
	}
	rec := httptest.NewRecorder()
	metaHook.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/meta", strings.NewReader(string(body))))
	if rec.Code != otherHookStatus {
		t.Fatalf("meta webhook = %d, want the stand-in's answer", rec.Code)
	}
	var msgs []model.Message
	err = h.store.View(h.ctx, hostTenant, func(r store.Reader) error {
		p, err := r.Messages(h.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{hostInbox, metaInbox}}, Page: model.PageRequest{Limit: 50}})
		msgs = p.Items
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var inbound []model.Message
	for _, m := range msgs {
		if m.Author.External.ID != "" {
			inbound = append(inbound, m)
		}
	}
	if len(inbound) != 1 || inbound[0].Source != model.TypeTwilioWhatsapp ||
		inbound[0].Author.External.Namespace != whatsapp.Namespace || inbound[0].Author.External.Channel != hostChannel {
		t.Fatalf("inbound messages %+v", inbound)
	}

	// Outbound routing: a twilio.whatsapp reply reaches Twilio only.
	acc := h.reply(twConv, "op-coexist")
	h.run(0)
	if op := h.operation(acc.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("operation = %s (%v)", op.State, op.Failure)
	}
	calls := fake.calls(hookAccount)
	if len(calls) != 1 || calls[0].Form.Get("To") != "whatsapp:"+userNumber {
		t.Fatalf("Twilio calls %+v", calls)
	}
	other.mu.Lock()
	defer other.mu.Unlock()
	if other.sends != 0 {
		t.Fatalf("meta.whatsapp sends = %d, want 0", other.sends)
	}
}
