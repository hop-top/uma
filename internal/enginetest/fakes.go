// Package enginetest holds the fixtures the engine packages share in their
// tests: host fakes, scripted adapters and seeded engines over the memory
// store. Only tests import it.
package enginetest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

const (
	TenantA  model.TenantID = "tenant-a"
	TenantB  model.TenantID = "tenant-b"
	Actor    model.ActorID  = "actor-1"
	InboxOne model.ID       = "00000001-0000-7000-8000-000000000001"
	InboxTwo model.ID       = "00000001-0000-7000-8000-000000000002"
	ConvOne  model.ID       = "00000001-0000-7000-8000-000000000003"
)

func PrincipalA() model.Principal { return model.Principal{Tenant: TenantA, Actor: Actor} }

// Allow is an Authorizer that allows everything and counts calls.
type Allow struct{ Calls atomic.Int32 }

func (a *Allow) Authorize(context.Context, policy.Authorization) (policy.Decision, error) {
	a.Calls.Add(1)
	return policy.Decision{Allow: true, PolicyVersion: "test"}, nil
}

// ValidConfig is the smallest valid composition: memory store, allow-all
// policy, fixed clock and sequential IDs.
func ValidConfig() engine.Config {
	return engine.Config{
		Store:      memory.New(memory.Options{}),
		Authorizer: &Allow{},
		Clock:      portstest.NewFixedClock(time.Unix(0, 0)),
		IDs:        portstest.NewSequenceIDs(1),
	}
}

// MustNew composes an engine from cfg or fails t.
func MustNew(t testing.TB, cfg engine.Config) *engine.Engine {
	t.Helper()
	e, err := engine.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// RequireCode fails unless err is a *model.Error with code whose safe
// message contains want.
func RequireCode(t testing.TB, err error, code model.ErrorCode, want string) {
	t.Helper()
	var me *model.Error
	if !errors.As(err, &me) {
		t.Fatalf("error = %v (%T), want *model.Error %s", err, err, code)
	}
	if me.Code != code {
		t.Fatalf("code = %s (%q), want %s", me.Code, me.SafeMessage, code)
	}
	if want != "" && !strings.Contains(me.SafeMessage, want) {
		t.Fatalf("message = %q, want it to contain %q", me.SafeMessage, want)
	}
}

// Factory is a test adapter.Factory. It records Describe and Open calls and
// never opens an instance.
type Factory struct {
	Desc       adapter.Descriptor
	Describes  *atomic.Int32
	Opens      *atomic.Int32
	OnValidate func(model.Extension) []model.ValidationIssue
}

func NewFactory(d adapter.Descriptor) *Factory {
	return &Factory{Desc: d, Describes: new(atomic.Int32), Opens: new(atomic.Int32)}
}

func (f *Factory) Describe() adapter.Descriptor { f.Describes.Add(1); return f.Desc }
func (f *Factory) Validate(c model.Extension) []model.ValidationIssue {
	if f.OnValidate != nil {
		return f.OnValidate(c)
	}
	return nil
}
func (f *Factory) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	f.Opens.Add(1)
	return nil, errors.New("test factory never opens")
}

const ThirdPartyKey model.TypeKey = "com.example.chat.messages"

func ReceiveDescriptor(k model.TypeKey) adapter.Descriptor {
	return adapter.Descriptor{
		Type:          k,
		ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{
			Name: string(k) + ".config", Version: 1,
		},
		Capabilities: []model.Capability{model.CapabilityReceive},
		Events:       []string{model.EventMessageReceived},
	}
}

// CloseLog counts Close calls on host-owned fakes.
type CloseLog struct {
	mu    sync.Mutex
	names []string
}

// Record notes one Close of name.
func (l *CloseLog) Record(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
}

// All returns every recorded Close.
func (l *CloseLog) All() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.names...)
}

// Host-owned fakes exposing every common close shape.

type OwnedStore struct {
	store.Store
	Log *CloseLog
}

func (s OwnedStore) Close() error { s.Log.Record("store"); return nil }

type OwnedHTTP struct{ Log *CloseLog }

func (OwnedHTTP) Do(*http.Request) (*http.Response, error) { return nil, errors.New("no I/O") }
func (h OwnedHTTP) CloseIdleConnections()                  { h.Log.Record("http") }

type OwnedCredentials struct{ Log *CloseLog }

func (OwnedCredentials) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return nil, errors.New("no I/O")
}
func (c OwnedCredentials) Close(context.Context) error { c.Log.Record("credentials"); return nil }

type OwnedBlobs struct {
	ports.BlobStore
	Log *CloseLog
}

func (b OwnedBlobs) Close() error { b.Log.Record("blobs"); return nil }

type OwnedObserver struct{ Log *CloseLog }

func (OwnedObserver) Observe(context.Context, ports.Observation) {}
func (o OwnedObserver) Close(context.Context) error              { o.Log.Record("observer"); return nil }

type OwnedClock struct {
	ports.Clock
	Log *CloseLog
}

func (c OwnedClock) Close() error { c.Log.Record("clock"); return nil }

type OwnedIDs struct {
	ports.IDs
	Log *CloseLog
}

func (i OwnedIDs) Close() error { i.Log.Record("ids"); return nil }

type OwnedAuthorizer struct {
	Allow
	Log *CloseLog
}

func (a *OwnedAuthorizer) Close() error { a.Log.Record("authorizer"); return nil }

type OwnedVerifier struct{ Log *CloseLog }

func (OwnedVerifier) Verify(context.Context, model.Principal, model.LinkRequest) (model.Evidence, error) {
	return model.Evidence{}, errors.New("no I/O")
}
func (v OwnedVerifier) Close() error { v.Log.Record("verifier"); return nil }

type OwnedSelector struct{ Log *CloseLog }

func (OwnedSelector) Select(context.Context, policy.SelectionInput) (policy.SelectionResult, error) {
	return policy.SelectionResult{}, errors.New("no I/O")
}
func (s OwnedSelector) Close() error { s.Log.Record("selector"); return nil }

type OwnedValidator struct{ Log *CloseLog }

func (OwnedValidator) Evaluate(context.Context, model.Principal, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, errors.New("no I/O")
}
func (v OwnedValidator) Close(context.Context) error { v.Log.Record("validator"); return nil }

type OwnedFactory struct {
	*Factory
	Log *CloseLog
}

func (f OwnedFactory) Close(context.Context) error { f.Log.Record("factory"); return nil }

// CountingStore records every unit of work opened against it.
type CountingStore struct {
	store.Store
	Durable bool
	Units   atomic.Int32
}

func (s *CountingStore) Properties() store.Properties {
	p := s.Store.Properties()
	p.Durable = s.Durable
	return p
}

func (s *CountingStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	s.Units.Add(1)
	return s.Store.View(ctx, t, fn)
}

func (s *CountingStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	s.Units.Add(1)
	return s.Store.Transact(ctx, t, fn)
}

type CountingClock struct{ Calls atomic.Int32 }

func (c *CountingClock) Now() time.Time { c.Calls.Add(1); return time.Unix(0, 0) }

type CountingIDs struct{ Calls atomic.Int32 }

func (i *CountingIDs) New(context.Context) (model.ID, error) {
	i.Calls.Add(1)
	return "", errors.New("unused")
}

// ScriptedVerifier is an IdentityVerifier that records every request and
// answers with Answer; a nil Answer fails verification.
type ScriptedVerifier struct {
	Answer func(model.Principal, model.LinkRequest) (model.Evidence, error)
	mu     sync.Mutex
	reqs   []model.LinkRequest
}

func (v *ScriptedVerifier) Verify(_ context.Context, p model.Principal, req model.LinkRequest) (model.Evidence, error) {
	v.mu.Lock()
	v.reqs = append(v.reqs, req.Clone())
	answer := v.Answer
	v.mu.Unlock()
	if answer == nil {
		return model.Evidence{}, errors.New("scripted verifier rejects")
	}
	return answer(p, req)
}

// Requests returns every request Verify received, in call order.
func (v *ScriptedVerifier) Requests() []model.LinkRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]model.LinkRequest, len(v.reqs))
	for i, r := range v.reqs {
		out[i] = r.Clone()
	}
	return out
}

// Calls counts Verify calls.
func (v *ScriptedVerifier) Calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.reqs)
}

// VersionedVerifier reports V as the version of the verifier it wraps.
type VersionedVerifier struct {
	policy.IdentityVerifier
	V string
}

func (v VersionedVerifier) Version() string { return v.V }
