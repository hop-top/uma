package relay_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

func TestDescribe(t *testing.T) {
	d := relay.NewFactory(relay.Options{}).Describe()
	if d.Type != model.TypeRelayMessages || d.ContractMajor != adapter.ContractMajor || d.Maturity != "experimental" {
		t.Fatalf("descriptor: %+v", d)
	}
	// reconcile is declared for the type; a channel without a lookup
	// reports it disabled.
	want := []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityReconcile, model.CapabilityStartConversation}
	if !slices.Equal(d.Capabilities, want) {
		t.Errorf("capabilities %v, want %v", d.Capabilities, want)
	}
	// receipt is the adapter telling a message's sender it was delivered or
	// read; status updates from the relay are not it (see TestStatusRouteDeclaresNoReceipt).
	for _, c := range []model.Capability{model.CapabilityHistory, model.CapabilityReceive, model.CapabilityReceipt} {
		if slices.Contains(d.Capabilities, c) {
			t.Errorf("declares %s, which the relay contract cannot honour", c)
		}
	}
	// delivery.updated: a status update can settle an operation, which the
	// engine reports as a provider-triggered delivery.updated.
	if !slices.Equal(d.Events, []string{model.EventMessageReceived, model.EventDeliveryUpdated}) {
		t.Errorf("events %v", d.Events)
	}
	if !slices.Equal(d.IdentityNamespaces, []string{relay.NamespaceSMS, relay.NamespaceWhatsApp, relay.NamespaceIMessage}) {
		t.Errorf("namespaces %v", d.IdentityNamespaces)
	}
	// The relay's ids are the operator's own: core must not treat them as
	// message identity or echo evidence.
	if d.Exchange.MessageIDs != adapter.MessageIDsSender {
		t.Errorf("message IDs authority %q, want sender", d.Exchange.MessageIDs)
	}
	if d.ReconcileSettle != relay.ReconcileSettle {
		t.Errorf("reconcile settle %v, want %v", d.ReconcileSettle, relay.ReconcileSettle)
	}
}

// A not_found counts only once no callback can still be in flight: the
// window outlasts the request timeout.
func TestReconcileSettleOutlastsTheTimeout(t *testing.T) {
	for _, tt := range []struct {
		timeout, want time.Duration
	}{
		{0, relay.ReconcileSettle},
		{30 * time.Second, relay.ReconcileSettle},
		{5 * time.Minute, 10 * time.Minute},
	} {
		if got := relay.NewFactory(relay.Options{Timeout: tt.timeout}).Describe().ReconcileSettle; got != tt.want {
			t.Errorf("timeout %v: settle %v, want %v", tt.timeout, got, tt.want)
		}
	}
}

func TestDescribeReturnsFreshSlices(t *testing.T) {
	f := relay.NewFactory(relay.Options{})
	f.Describe().Capabilities[0] = "mutated"
	if f.Describe().Capabilities[0] != model.CapabilityReceiveHTTP {
		t.Fatal("Describe shares its capability slice")
	}
}

func TestOpenRefusals(t *testing.T) {
	f := relay.NewFactory(relay.Options{})
	ch := channelOf(baseConfig())
	ch.Type = model.TypeTwilioSms
	if _, err := f.Open(t.Context(), ch, adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
		t.Errorf("wrong type: %v", err)
	}
	ch = channelOf(relay.Config{Medium: relay.MediumSMS})
	if _, err := f.Open(t.Context(), ch, adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
		t.Errorf("no auth scheme: %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.Open(cancelled, channelOf(baseConfig()), adapter.Dependencies{}); err == nil {
		t.Error("cancelled context opened an instance")
	}
}

func TestCapabilitiesFollowConfiguration(t *testing.T) {
	f := relay.NewFactory(relay.Options{CallbackHosts: []string{"relay.example.com"}})
	withCallback := baseConfig()
	withCallback.Callback = &relay.Callback{URL: "https://relay.example.com/out"}
	withLookup := baseConfig()
	withLookup.Callback = &relay.Callback{URL: "https://relay.example.com/out", LookupURL: "https://relay.example.com/lookup"}
	status := func(i *relay.Instance) map[model.Capability]model.CapabilityStatus {
		got, err := i.Capabilities(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		out := map[model.Capability]model.CapabilityStatus{}
		for _, s := range got {
			if _, dup := out[s.Name]; dup {
				t.Fatalf("%s reported twice", s.Name)
			}
			out[s.Name] = s
		}
		return out
	}
	cases := []struct {
		name      string
		inst      *relay.Instance
		send      model.ErrorCode // reason of send; empty: available
		reconcile model.ErrorCode // reason of reconcile; empty: available
	}{
		{"no callback", open(t, f, baseConfig(), adapter.Dependencies{HTTP: http.DefaultClient}), model.Disabled, model.Disabled},
		{"callback, no client", open(t, f, withCallback, adapter.Dependencies{}), model.Unavailable, model.Disabled},
		{"callback and client", open(t, f, withCallback, adapter.Dependencies{HTTP: http.DefaultClient}), "", model.Disabled},
		{"lookup, no client", open(t, f, withLookup, adapter.Dependencies{}), model.Unavailable, model.Unavailable},
		{"lookup and client", open(t, f, withLookup, adapter.Dependencies{HTTP: http.DefaultClient}), "", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := status(tt.inst)
			if len(got) != 4 {
				t.Fatalf("reported %d capabilities", len(got))
			}
			if s := got[model.CapabilityReconcile]; s.Available != (tt.reconcile == "") || s.Reason != tt.reconcile {
				t.Errorf("reconcile %+v, want reason %q", s, tt.reconcile)
			}
			if s := got[model.CapabilityReceiveHTTP]; !s.Available || s.Reason != "" {
				t.Errorf("receive_http %+v: a receiver needs no callback", s)
			}
			for _, c := range []model.Capability{model.CapabilitySend, model.CapabilityStartConversation} {
				s := got[c]
				if s.Available != (tt.send == "") || s.Reason != tt.send {
					t.Errorf("%s %+v, want reason %q", c, s, tt.send)
				}
			}
		})
	}
}

func TestCloseIsIdempotentAndMakesCapabilitiesUnavailable(t *testing.T) {
	inst := open(t, relay.NewFactory(relay.Options{}), baseConfig(), adapter.Dependencies{})
	for range 2 {
		if err := inst.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := inst.Capabilities(t.Context())
	for _, s := range got {
		if s.Available || s.Reason != model.Unavailable {
			t.Errorf("%+v after Close", s)
		}
	}
}
