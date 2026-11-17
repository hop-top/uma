package telegram

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

func TestReceiveModeConfiguration(t *testing.T) {
	ext := model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: []byte(`{"bot_id":7000001,"bot_username":"uma_bot","receive":"webhook"}`)}
	if c, issues := parseConfig(ext); len(issues) > 0 || c.Receive != ReceiveWebhook {
		t.Errorf("receive webhook: %+v %+v", c, issues)
	}
	// Empty is left out, so a configuration written before the field reads
	// the same.
	if data := string(testConfig().Extension().Data); strings.Contains(data, "receive") {
		t.Errorf("empty receive encoded: %s", data)
	}
	c := testConfig()
	c.Receive = "both"
	issues := NewFactory(Options{}).Validate(c.Extension())
	want := model.ValidationIssue{Field: "configuration.data.receive", Code: string(model.Validation), Message: `must be "poll" or "webhook"`}
	if len(issues) != 1 || issues[0] != want {
		t.Errorf("issues = %+v, want %+v", issues, want)
	}
}

// withheld is the way of receiving each mode withholds.
var withheld = map[ReceiveMode]model.Capability{
	"":             "",
	ReceivePoll:    model.CapabilityReceiveHTTP,
	ReceiveWebhook: model.CapabilityReceive,
}

func TestCapabilitiesFollowReceiveMode(t *testing.T) {
	for mode, off := range withheld {
		name := string(mode)
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			// The descriptor is the adapter type's and declares both ways in
			// every mode; the instance reports the channel's.
			declared := NewFactory(Options{}).Describe().Capabilities
			if !slices.Contains(declared, model.CapabilityReceive) || !slices.Contains(declared, model.CapabilityReceiveHTTP) {
				t.Fatalf("descriptor declares %v, want both ways of receiving", declared)
			}
			c := testConfig()
			c.Receive = mode
			st, err := open(t, c).Capabilities(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			seen := map[model.Capability]bool{}
			for _, s := range st {
				if seen[s.Name] {
					t.Errorf("%s reported twice", s.Name)
				}
				seen[s.Name] = true
				switch {
				case s.Name == off && (s.Available || s.Reason != model.Disabled):
					t.Errorf("%s = %+v, want unavailable, disabled", s.Name, s)
				case s.Name != off && (!s.Available || s.Reason != ""):
					t.Errorf("%s = %+v, want available", s.Name, s)
				}
			}
			for _, d := range declared {
				if !seen[d] {
					t.Errorf("declared %s not reported", d)
				}
			}
		})
	}
}

func TestRunUnderWebhookModeReadsNothing(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Receive = ReceiveWebhook })
	r.queue(msgUpdate(10, "one"))
	sink := &cursorSink{}
	// Bounded, so a receiver that polls after all fails instead of hanging.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := r.x.Run(ctx, sink)
	if !model.IsCode(err, model.Disabled) {
		t.Fatalf("Run = %v, want disabled", err)
	}
	if n := len(r.f.calls("acct1")); n != 0 {
		t.Errorf("%d Bot API calls, want none", n)
	}
	if got := sink.envelopes(); len(got) != 0 {
		t.Errorf("envelopes %+v", got)
	}
	if p := r.creds.purposes(); len(p) != 0 {
		t.Errorf("credentials resolved: %v", p)
	}
}

func TestRunUnderPollModeReads(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Receive = ReceivePoll })
	r.queue(msgUpdate(10, "one"))
	sink := &cursorSink{}
	if err := r.run(t, sink, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the cancellation that ended it", err)
	}
	if got := sink.envelopes(); len(got) != 1 {
		t.Errorf("envelopes %+v, want one", got)
	}
}

func TestHandlerUnderPollModeReadsNothing(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Receive = ReceivePoll })
	sink := &sinkRecorder{}
	rec := jsonPost(msgUpdate(10, "one")).send(r.x.Handler(sink))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
	if got := sink.envelopes(); len(got) != 0 {
		t.Errorf("envelopes %+v", got)
	}
	if p := r.creds.purposes(); len(p) != 0 {
		t.Errorf("credentials resolved: %v", p)
	}
}

func TestHandlerUnderWebhookModeServes(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Receive = ReceiveWebhook })
	sink := &sinkRecorder{}
	if rec := jsonPost(msgUpdate(10, "one")).send(r.x.Handler(sink)); rec.Code != http.StatusOK {
		t.Errorf("status %d, want 200", rec.Code)
	}
	if got := sink.envelopes(); len(got) != 1 {
		t.Errorf("envelopes %+v, want one", got)
	}
}

// TestEngineFollowsReceiveMode drives the real engine: the capabilities a
// host reads for a channel follow its mode, and the engine refuses the way
// of receiving the mode withholds before the adapter makes any call.
func TestEngineFollowsReceiveMode(t *testing.T) {
	ctx := t.Context()
	f := newFakeTelegram(func() time.Time { return testNow })
	f.account("acct1", testBotID, testToken)
	st := memory.New(memory.Options{Clock: portstest.NewFixedClock(testNow)})
	e, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{NewFactory(Options{PollInterval: time.Millisecond})},
		Credentials: newCredentials(),
		HTTP:        f.client(),
		Clock:       portstest.NewFixedClock(testNow),
		IDs:         portstest.NewSequenceIDs(0xc0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	refs := map[ReceiveMode]model.ChannelRef{
		"":             {Tenant: testTenant, ID: "00000ad0-0000-7000-8000-0000000000c1"},
		ReceivePoll:    {Tenant: testTenant, ID: "00000ad0-0000-7000-8000-0000000000c2"},
		ReceiveWebhook: {Tenant: testTenant, ID: "00000ad0-0000-7000-8000-0000000000c3"},
	}
	if err := st.Transact(ctx, testTenant, func(tx store.Tx) error {
		for mode, ref := range refs {
			c := testConfig()
			c.APIURL, c.Receive = f.apiURL("acct1"), mode
			if err := tx.PutChannel(ctx, model.Channel{Ref: ref, Type: Type, Configuration: c.Extension(), CredentialRef: "telegram", Revision: "ch1"}, ""); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	host, err := e.For(model.Principal{Tenant: testTenant, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: testTenant, Actor: "telegram-service"}, Call: model.Call{Correlation: "receive-mode"}}
	for mode, off := range withheld {
		ref := refs[mode]
		statuses, err := host.Configuration().Capabilities(ctx, model.Lookup[model.ChannelRef]{Call: model.Call{Correlation: "caps"}, Ref: ref})
		if err != nil {
			t.Fatalf("%q: Capabilities: %v", mode, err)
		}
		for _, s := range statuses {
			if s.Name != model.CapabilityReceive && s.Name != model.CapabilityReceiveHTTP {
				continue
			}
			if want := s.Name != off; s.Available != want {
				t.Errorf("%q: %s available = %v, want %v", mode, s.Name, s.Available, want)
			}
		}
		_, err = e.Runtime().Webhook(ctx, call, ref)
		if off == model.CapabilityReceiveHTTP {
			if !model.IsCode(err, model.Unsupported) {
				t.Errorf("%q: Webhook = %v, want unsupported", mode, err)
			}
		} else if err != nil {
			t.Errorf("%q: Webhook = %v", mode, err)
		}
	}
	// A webhook channel never polls: Receive ends at once, and Telegram is
	// never asked for updates, so a webhook set for the bot is never met
	// with a conflict.
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := e.Runtime().Receive(rctx, call, refs[ReceiveWebhook]); !model.IsCode(err, model.Unsupported) {
		t.Errorf("Receive on a webhook channel = %v, want unsupported", err)
	}
	if n := len(f.callsOf("acct1", "getUpdates")); n != 0 {
		t.Errorf("%d getUpdates calls, want none", n)
	}
}
