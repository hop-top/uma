package whatsapp_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

const testTenant model.TenantID = "wa-tenant"

const (
	testPNID = "106540352242922"
	testUser = "16505551234"
)

var testChannel = model.ChannelRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-00000000c0a1"}

// creds resolves one fixed value per purpose and records every request.
type creds struct {
	mu     sync.Mutex
	values map[string]ports.Credential
	reqs   []ports.CredentialRequest
}

func newCreds() *creds {
	return &creds{values: map[string]ports.Credential{
		whatsapp.PurposeGraph:       ports.OAuth2{AccessToken: "EAAG-access-token"}, // #nosec G101 -- fixture value, not a credential
		whatsapp.PurposeAppSecret:   ports.Secret{Bytes: []byte("app-secret")},
		whatsapp.PurposeVerifyToken: ports.Secret{Bytes: []byte("verify-token")},
	}}
}

func (c *creds) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, r)
	v, ok := c.values[r.Purpose]
	if !ok {
		return nil, model.NewError(model.Unauthorized, "no credential for purpose")
	}
	return v, nil
}

func (c *creds) requests() []ports.CredentialRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ports.CredentialRequest(nil), c.reqs...)
}

// protocol is an in-memory adapter.ProtocolState.
type protocol struct {
	mu   sync.Mutex
	vals map[string]model.Extension
	revs map[string]model.Revision
	n    int
	fail error
}

func newProtocol() *protocol {
	return &protocol{vals: map[string]model.Extension{}, revs: map[string]model.Revision{}}
}

func (p *protocol) Load(_ context.Context, key string) (model.Extension, model.Revision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return model.Extension{}, "", p.fail
	}
	v, ok := p.vals[key]
	if !ok {
		return model.Extension{}, "", model.NewError(model.NotFound, "no state")
	}
	return v, p.revs[key], nil
}

func (p *protocol) CompareAndSwap(_ context.Context, key string, expected model.Revision, v model.Extension) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return p.fail
	}
	if p.revs[key] != expected {
		return model.NewError(model.Conflict, "revision moved")
	}
	p.n++
	p.vals[key], p.revs[key] = v, model.Revision("r"+strconv.Itoa(p.n))
	return nil
}

// setLastInbound stores user's window as last seen at.
func (p *protocol) setLastInbound(user string, at time.Time) {
	data, _ := json.Marshal(map[string]int64{"last_inbound": at.Unix()})
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	p.vals["window/"+user] = model.Extension{Name: whatsapp.WindowSchema, Version: whatsapp.WindowVersion, Data: data}
	p.revs["window/"+user] = model.Revision("r" + strconv.Itoa(p.n))
}

func (p *protocol) lastInbound(user string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var w struct {
		LastInbound int64 `json:"last_inbound"`
	}
	_ = json.Unmarshal(p.vals["window/"+user].Data, &w)
	return w.LastInbound
}

type clock struct{ t time.Time }

func (c clock) Now() time.Time { return c.t }

var testNow = time.Unix(1_750_030_073, 0).UTC()

func channel(pnid string) model.Channel {
	return model.Channel{
		Ref: testChannel, Type: model.TypeMetaWhatsapp, CredentialRef: "meta-app", Revision: "ch1",
		Configuration: whatsapp.Config{PhoneNumberID: pnid}.Extension(),
	}
}

func openInstance(t *testing.T, f *whatsapp.Factory, deps adapter.Dependencies) *whatsapp.Instance {
	t.Helper()
	if deps.Clock == nil {
		deps.Clock = clock{testNow}
	}
	inst, err := f.Open(t.Context(), channel(testPNID), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst.(*whatsapp.Instance)
}

func userRoute(id string) model.ExternalRef {
	return model.ExternalRef{Channel: testChannel, Namespace: whatsapp.Namespace, Kind: whatsapp.RouteKind, ID: id}
}

func effect(op string) adapter.Effect {
	return adapter.Effect{
		Operation: model.OperationRef{Tenant: testTenant, ID: model.ID(op)},
		Attempt:   "00000000-0000-7000-8000-0000000000a1",
		Route:     userRoute(testUser), Audience: model.Audience{Visibility: model.Public},
	}
}

func textContent(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

func templateContent(data string) model.Content {
	e := model.Extension{Name: whatsapp.TemplateSchema, Version: whatsapp.TemplateVersion, Data: json.RawMessage(data)}
	return model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &e}}}
}

const helloTemplate = `{"name":"hello_world","language":{"code":"en_US"}}`
