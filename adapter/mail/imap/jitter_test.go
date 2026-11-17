package imap_test

import (
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
)

// jitteredFactory is steppedFactory with the jitter source answering u.
func jitteredFactory(t *testing.T, w *stepWaiter, u func() float64) *imap.Factory {
	return imap.NewFactory(imap.Options{RootCAs: certs(t).roots, Waiter: w, Jitter: u})
}

// A poll wait is the poll interval +-10%, following the injected source.
func TestPollWaitIsSpread(t *testing.T) {
	const poll = 50 * time.Millisecond
	for _, c := range []struct {
		u    float64
		want time.Duration
	}{{0, poll * 9 / 10}, {0.5, poll}, {1, poll * 11 / 10}} {
		srv := newMailServer(t, serverOptions{sec: implicitTLS, noIdle: true})
		w := &stepWaiter{}
		inst := open(t, jitteredFactory(t, w, func() float64 { return c.u }), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
		run(t, inst, newSink())
		w.idle(t)
		if got := w.requested(); len(got) == 0 || got[0] != c.want {
			t.Errorf("source %v: waits %v, want %v", c.u, got, c.want)
		}
	}
}

// An IDLE restart is the restart interval +-10%, and never more than the
// 29 minutes RFC 2177 allows.
func TestIdleRestartIsSpreadAndCapped(t *testing.T) {
	for _, c := range []struct {
		name string
		ms   int64
		u    float64
		want time.Duration
	}{
		{"short", 25 * 60_000, 0, 25 * time.Minute * 9 / 10},
		{"keep", 25 * 60_000, 0.5, 25 * time.Minute},
		{"long", 25 * 60_000, 1, 25 * time.Minute * 11 / 10},
		{"capped", 29 * 60_000, 1, imap.MaxIdleRestart},
		{"cap low", 29 * 60_000, 0, 29 * time.Minute * 9 / 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := newMailServer(t, serverOptions{sec: implicitTLS})
			cfg := config(srv, nil)
			cfg.IdleRestartMS = c.ms
			w := &stepWaiter{}
			inst := open(t, jitteredFactory(t, w, func() float64 { return c.u }), channel(channelA, cfg), adapter.Dependencies{Credentials: passwordCreds()})
			run(t, inst, newSink())
			w.idle(t)
			if got := w.requested(); len(got) == 0 || got[0] != c.want {
				t.Errorf("waits %v, want %v", got, c.want)
			}
		})
	}
}

// With no injected source the waits stay within +-10% and are not all equal.
func TestIdleRestartDefaultSourceVaries(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 20 {
		srv := newMailServer(t, serverOptions{sec: implicitTLS})
		cfg := config(srv, nil)
		cfg.IdleRestartMS = 25 * 60_000
		w := &stepWaiter{}
		inst := open(t, jitteredFactory(t, w, nil), channel(channelA, cfg), adapter.Dependencies{Credentials: passwordCreds()})
		run(t, inst, newSink())
		w.idle(t)
		d := w.requested()[0]
		if d < 25*time.Minute*9/10 || d > 25*time.Minute*11/10 {
			t.Fatalf("wait %v outside +-10%% of 25m", d)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Fatalf("20 waits took only %d values", len(seen))
	}
}
