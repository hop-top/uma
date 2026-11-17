package zendesk

import (
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func openJittered(t *testing.T, seconds int, source func() float64) *instance {
	t.Helper()
	cfg := Config{Subdomain: "acme", Auth: AuthAPIToken, Email: "a@acme.example", WebhookToleranceSeconds: 60, PollIntervalSeconds: seconds}
	opts := []Option{}
	if source != nil {
		opts = append(opts, WithJitter(source))
	}
	inst, err := NewFactory(opts...).Open(t.Context(), model.Channel{
		Ref:  model.ChannelRef{Tenant: "t", ID: "00000ad0-0000-7000-8000-0000000000aa"},
		Type: Type, Configuration: cfg.Extension(), CredentialRef: "zendesk",
	}, adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	return inst.(*Instance).x
}

// The wait after an empty page is the poll interval +-10%, following the
// injected source.
func TestPollWaitIsSpread(t *testing.T) {
	const poll = 30 * time.Second
	for _, c := range []struct {
		u    float64
		want time.Duration
	}{{0, poll * 9 / 10}, {0.5, poll}, {1, poll * 11 / 10}} {
		x := openJittered(t, 30, func() float64 { return c.u })
		if got := x.pollWait(); got != c.want {
			t.Errorf("source %v: wait %v, want %v", c.u, got, c.want)
		}
	}
}

// The spread never takes the wait under the 6 second minimum the incremental
// export allows.
func TestPollWaitKeepsTheMinimum(t *testing.T) {
	x := openJittered(t, 6, func() float64 { return 0 })
	if got := x.pollWait(); got != minPollInterval {
		t.Fatalf("wait %v, want the %v minimum", got, minPollInterval)
	}
}

// With no injected source every wait stays within +-10% and the waits are
// not all equal.
func TestPollWaitDefaultSourceVaries(t *testing.T) {
	x := openJittered(t, 30, nil)
	seen := map[time.Duration]bool{}
	for range 50 {
		d := x.pollWait()
		if d < 27*time.Second || d > 33*time.Second {
			t.Fatalf("wait %v outside +-10%% of 30s", d)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Fatalf("50 waits took only %d values", len(seen))
	}
}
