package slack

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
	ext := model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: []byte(`{"team_id":"T0001","bot_user_id":"U0BOT","receive":"socket_mode"}`)}
	if c, issues := parseConfig(ext); len(issues) > 0 || c.Receive != ReceiveSocketMode {
		t.Errorf("receive socket_mode: %+v %+v", c, issues)
	}
	// Empty is left out, so a configuration written before the field reads
	// the same.
	if data := string(testConfig().Extension().Data); strings.Contains(data, "receive") {
		t.Errorf("empty receive encoded: %s", data)
	}
	c := testConfig()
	c.Receive = "both"
	issues := NewFactory(Options{}).Validate(c.Extension())
	want := model.ValidationIssue{Field: "configuration.data.receive", Code: string(model.Validation), Message: `must be "events_api" or "socket_mode"`}
	if len(issues) != 1 || issues[0] != want {
		t.Errorf("issues = %+v, want %+v", issues, want)
	}
}

// withheld is the way of receiving each mode withholds.
var withheld = map[ReceiveMode]model.Capability{
	"":                "",
	ReceiveEventsAPI:  model.CapabilityReceive,
	ReceiveSocketMode: model.CapabilityReceiveHTTP,
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

func TestRunUnderEventsAPIModeConnectsNothing(t *testing.T) {
	r := newSocketRig(t, Options{}, func(c *Config) { c.Receive = ReceiveEventsAPI })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.x.Run(ctx, r.sink); !model.IsCode(err, model.Disabled) {
		t.Fatalf("Run = %v, want disabled", err)
	}
	if n := len(r.f.calls("acct1")); n != 0 {
		t.Errorf("%d Web API calls, want none", n)
	}
	if len(r.creds.requests) != 0 {
		t.Errorf("credentials resolved: %+v", r.creds.requests)
	}
}

func TestHandlerUnderSocketModeReadsNothing(t *testing.T) {
	c := testConfig()
	c.Receive = ReceiveSocketMode
	creds := newCredentials()
	x := open(t, c, func(d *adapter.Dependencies) { d.Credentials = creds })
	sink := &sinkRecorder{}
	if res := serve(t, x, sink, jsonRequest(eventPayload("Ev01", testPerson, "hi", testTS))); res.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", res.Code)
	}
	wantNone(t, sink)
	if len(creds.requests) != 0 {
		t.Errorf("credentials resolved: %+v", creds.requests)
	}
}

func TestHandlerUnderEventsAPIModeServes(t *testing.T) {
	c := testConfig()
	c.Receive = ReceiveEventsAPI
	x, sink := open(t, c), &sinkRecorder{}
	if res := serve(t, x, sink, jsonRequest(eventPayload("Ev01", testPerson, "hi", testTS))); res.Code != http.StatusOK {
		t.Errorf("status %d, want 200", res.Code)
	}
	wantOne(t, sink)
}

// TestEngineFollowsReceiveMode drives the real engine: the capabilities a
// host reads for a channel follow its mode, the engine refuses the way of
// receiving the mode withholds before the adapter makes any call, and
// Runtime.Receive on a Socket Mode channel receives over the socket.
func TestEngineFollowsReceiveMode(t *testing.T) {
	ctx := t.Context()
	f := newFakeSlack(func() time.Time { return testNow })
	t.Cleanup(f.close)
	a := f.account("acct1", testTeam, testBotUser, testToken)
	a.appToken = testAppToken
	st := memory.New(memory.Options{Clock: portstest.NewFixedClock(testNow)})
	e, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{NewFactory(Options{})},
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
		"":                {Tenant: testTenant, ID: "00000ad0-0000-7000-8000-0000000000c1"},
		ReceiveEventsAPI:  {Tenant: testTenant, ID: "00000ad0-0000-7000-8000-0000000000c2"},
		ReceiveSocketMode: {Tenant: testTenant, ID: "00000ad0-0000-7000-8000-0000000000c3"},
	}
	if err := st.Transact(ctx, testTenant, func(tx store.Tx) error {
		for mode, ref := range refs {
			c := testConfig()
			c.APIURL, c.Receive = f.apiURL("acct1"), mode
			if err := tx.PutChannel(ctx, model.Channel{Ref: ref, Type: Type, Configuration: c.Extension(), CredentialRef: "slack", Revision: "ch1"}, ""); err != nil {
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
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: testTenant, Actor: "slack-service"}, Call: model.Call{Correlation: "receive-mode"}}
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
	// A request URL channel never opens a socket: Receive ends at once.
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := e.Runtime().Receive(rctx, call, refs[ReceiveEventsAPI]); !model.IsCode(err, model.Unsupported) {
		t.Errorf("Receive on a request URL channel = %v, want unsupported", err)
	}
	a.noConn(t, 50*time.Millisecond)
	// A Socket Mode channel connects through the engine and lets go of the
	// connection when the receiver stops (the conformance suite drives
	// envelopes through it).
	sctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.Runtime().Receive(sctx, call, refs[ReceiveSocketMode]) }()
	c := a.nextConn(t)
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Receive = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Receive did not return after its context ended")
	}
	if !c.closed(time.Second) {
		t.Error("connection left open after Receive returned")
	}
}
