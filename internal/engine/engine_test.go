package engine_test

import (
	"runtime"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store/memory"
)

func TestNewAcceptsMinimalConfig(t *testing.T) {
	e := enginetest.MustNew(t, enginetest.ValidConfig())
	if got := e.Describe(); len(got) != 0 {
		t.Fatalf("Describe = %v, want no adapters", got)
	}
}

func TestNewRequiresInputs(t *testing.T) {
	cases := []struct {
		field string
		clear func(*engine.Config)
	}{
		{"Config.Store", func(c *engine.Config) { c.Store = nil }},
		{"Config.Authorizer", func(c *engine.Config) { c.Authorizer = nil }},
		{"Config.Clock", func(c *engine.Config) { c.Clock = nil }},
		{"Config.IDs", func(c *engine.Config) { c.IDs = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			cfg := enginetest.ValidConfig()
			tc.clear(&cfg)
			e, err := engine.New(cfg)
			if e != nil {
				t.Fatal("New returned an engine with an error")
			}
			enginetest.RequireCode(t, err, model.Validation, tc.field+": required")
		})
	}
}

// Optional dependencies: absence is not a composition error. Operations
// that need them fail when used.
func TestNewOptionalInputs(t *testing.T) {
	cfg := enginetest.ValidConfig()
	cfg.Credentials, cfg.HTTP, cfg.Blobs, cfg.Observer = nil, nil, nil, nil
	cfg.IdentityVerifiers, cfg.Selectors, cfg.InteractionValidator = nil, nil, nil
	enginetest.MustNew(t, cfg)
}

func TestNewRequireDurable(t *testing.T) {
	cfg := enginetest.ValidConfig()
	cfg.RequireDurable = true
	_, err := engine.New(cfg)
	enginetest.RequireCode(t, err, model.Validation, "Config.Store: durable store required")

	cfg.Store = &enginetest.CountingStore{Store: memory.New(memory.Options{}), Durable: true}
	enginetest.MustNew(t, cfg)

	cfg.RequireDurable = false
	cfg.Store = memory.New(memory.Options{})
	enginetest.MustNew(t, cfg)
}

func TestNewWebhookBodyLimit(t *testing.T) {
	cfg := enginetest.ValidConfig()
	cfg.WebhookBodyLimit = -1
	_, err := engine.New(cfg)
	enginetest.RequireCode(t, err, model.Validation, "Config.WebhookBodyLimit: must not be negative")

	cfg.WebhookBodyLimit = 0
	if got := enginetest.MustNew(t, cfg).WebhookBodyLimit(); got != engine.DefaultWebhookBodyLimit {
		t.Fatalf("zero limit = %d, want the default %d", got, engine.DefaultWebhookBodyLimit)
	}
	cfg.WebhookBodyLimit = 512
	if got := enginetest.MustNew(t, cfg).WebhookBodyLimit(); got != 512 {
		t.Fatalf("limit = %d, want 512", got)
	}
}

func TestNewRejectsMalformedPolicyMaps(t *testing.T) {
	cases := []struct {
		name, want string
		set        func(*engine.Config)
	}{
		{"nil verifier", "Config.IdentityVerifiers: nil value", func(c *engine.Config) {
			c.IdentityVerifiers = map[string]policy.IdentityVerifier{"attestation": nil}
		}},
		{"empty verifier key", "Config.IdentityVerifiers: empty key", func(c *engine.Config) {
			c.IdentityVerifiers = map[string]policy.IdentityVerifier{"": enginetest.OwnedVerifier{}}
		}},
		{"nil selector", "Config.Selectors: nil value", func(c *engine.Config) {
			c.Selectors = map[string]policy.ConversationSelector{"default": nil}
		}},
		{"empty selector key", "Config.Selectors: empty key", func(c *engine.Config) {
			c.Selectors = map[string]policy.ConversationSelector{"": enginetest.OwnedSelector{}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := enginetest.ValidConfig()
			tc.set(&cfg)
			_, err := engine.New(cfg)
			enginetest.RequireCode(t, err, model.Validation, tc.want)
		})
	}
}

// New validates composition only: it opens no instance, runs no unit of
// work, consults no policy, reads no clock or ID source and leaves no
// goroutine behind.
func TestNewPerformsNoIOAndStartsNoGoroutines(t *testing.T) {
	st := &enginetest.CountingStore{Store: memory.New(memory.Options{})}
	auth := &enginetest.Allow{}
	clock, ids := &enginetest.CountingClock{}, &enginetest.CountingIDs{}
	f := enginetest.NewFactory(enginetest.ReceiveDescriptor(enginetest.ThirdPartyKey))
	cfg := engine.Config{
		Store: st, Authorizer: auth, Clock: clock, IDs: ids,
		Adapters: []adapter.Factory{f},
	}
	before := runtime.NumGoroutine()
	e := enginetest.MustNew(t, cfg)
	_ = e.Describe()
	_ = e.ValidateChannel(enginetest.ThirdPartyKey, f.Desc.ConfigurationSchema)
	if _, err := e.For(enginetest.PrincipalA()); err != nil {
		t.Fatal(err)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines %d -> %d", before, after)
	}
	if n := f.Opens.Load(); n != 0 {
		t.Errorf("Factory.Open called %d times", n)
	}
	if n := f.Describes.Load(); n != 1 {
		t.Errorf("Factory.Describe called %d times, want 1", n)
	}
	if n := st.Units.Load(); n != 0 {
		t.Errorf("store units opened: %d", n)
	}
	if n := auth.Calls.Load(); n != 0 {
		t.Errorf("authorizer calls: %d", n)
	}
	if clock.Calls.Load() != 0 || ids.Calls.Load() != 0 {
		t.Errorf("clock %d, ids %d calls", clock.Calls.Load(), ids.Calls.Load())
	}
}

// An unset work policy takes the documented defaults; an explicit one is
// validated by New, every field positive.
func TestNewWorkPolicy(t *testing.T) {
	base := enginetest.ValidConfig()
	e := enginetest.MustNew(t, base)
	if got := e.Work(); got != (engine.WorkPolicy{MaxClaims: engine.DefaultMaxClaims, MaxInFlight: engine.DefaultMaxInFlight}) {
		t.Fatalf("default work policy = %+v", got)
	}
	if engine.DefaultMaxClaims != 10 || engine.DefaultMaxInFlight != 16 {
		t.Fatalf("defaults = %d, %d; want 10 and 16", engine.DefaultMaxClaims, engine.DefaultMaxInFlight)
	}
	explicit := base
	explicit.Work = &engine.WorkPolicy{MaxClaims: 3, MaxInFlight: 4}
	if got := enginetest.MustNew(t, explicit).Work(); got != *explicit.Work {
		t.Fatalf("explicit work policy = %+v, want %+v", got, *explicit.Work)
	}
	for _, c := range []struct {
		p     engine.WorkPolicy
		field string
	}{
		{engine.WorkPolicy{MaxInFlight: 4}, "Config.Work.MaxClaims"},
		{engine.WorkPolicy{MaxClaims: -1, MaxInFlight: 4}, "Config.Work.MaxClaims"},
		{engine.WorkPolicy{MaxClaims: 3}, "Config.Work.MaxInFlight"},
		{engine.WorkPolicy{MaxClaims: 3, MaxInFlight: -2}, "Config.Work.MaxInFlight"},
	} {
		cfg := base
		cfg.Work = &c.p
		_, err := engine.New(cfg)
		enginetest.RequireCode(t, err, model.Validation, c.field+": must be positive")
	}
}

// A verifier registration key is an evidence kind: only the five core kinds
// have a defined maximum assurance, so any other key is a composition error.
func TestNewRejectsUnknownVerifierKind(t *testing.T) {
	for _, key := range []string{"oauth", "Attestation", "uma.evidence.attestation", "com.example.sso"} {
		cfg := enginetest.ValidConfig()
		cfg.IdentityVerifiers = map[string]policy.IdentityVerifier{key: enginetest.OwnedVerifier{}}
		_, err := engine.New(cfg)
		enginetest.RequireCode(t, err, model.Validation, "Config.IdentityVerifiers: key is not an evidence kind")
	}
	cfg := enginetest.ValidConfig()
	cfg.IdentityVerifiers = map[string]policy.IdentityVerifier{}
	for _, k := range model.EvidenceKinds() {
		cfg.IdentityVerifiers[k] = enginetest.OwnedVerifier{}
	}
	e := enginetest.MustNew(t, cfg)
	for _, k := range model.EvidenceKinds() {
		if _, err := e.IdentityVerifier(k); err != nil {
			t.Errorf("verifier %q not registered: %v", k, err)
		}
	}
}

// Held opts local writes into holding; the zero value opts in none.
func TestNewHeldPolicy(t *testing.T) {
	base := enginetest.ValidConfig()
	if got := enginetest.MustNew(t, base).Held(); got != (engine.HeldPolicy{}) {
		t.Fatalf("default held policy = %+v, want none", got)
	}
	base.Held = engine.HeldPolicy{LabelDelete: true}
	if got := enginetest.MustNew(t, base).Held(); got != (engine.HeldPolicy{LabelDelete: true}) {
		t.Fatalf("held policy = %+v", got)
	}
}
