package postgres

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

// BenchmarkRunWorkers drains b.N queued replies with concurrent Runtime.Run
// workers, each an engine on its own pool as a separate process runs one,
// with the default one-second poll. ns/op is wall time per reply from the
// workers' start to the last provider call. idle-waits/op counts worker
// waits per reply: a claim round that came back empty, including one that
// lost every unit it may retry to serialization failures while work was
// still queued, waits out the poll. dup-sends counts provider calls beyond
// one per reply.
func BenchmarkRunWorkers(b *testing.B) {
	for _, n := range []int{1, 4, 8, 16} {
		b.Run(fmt.Sprintf("workers=%d", n), func(b *testing.B) {
			first := benchDatabase(b)
			name := first.pool.Config().ConnConfig.Database
			r := newReplyRig(b, first)
			for i := range b.N {
				r.reply(b, i)
			}
			snd := &countingSender{want: int64(b.N), done: make(chan struct{})}
			var waits atomic.Int64
			engines := make([]uma.Engine, n)
			for w := range engines {
				st := first
				if w > 0 {
					st = benchStore(b, name)
				}
				e, err := uma.New(uma.Config{
					Store: st, Authorizer: benchAllow(), Adapters: []adapter.Factory{snd},
					Clock: benchClock{}, IDs: portstest.NewSequenceIDs(uint32(0xc0 + w)), // #nosec G115 -- w < 16
					Waiter: countingWaiter{&waits},
				})
				if err != nil {
					b.Fatal(err)
				}
				engines[w] = e
			}
			ctx, stop := context.WithCancel(b.Context())
			ran := make(chan error, n)
			b.ResetTimer()
			for w, e := range engines {
				go func() {
					ran <- e.Runtime().Run(ctx, benchCall(), uma.WorkerOptions{Owner: fmt.Sprintf("worker-%d", w), Lease: time.Minute})
				}()
			}
			select {
			case <-snd.done:
			case <-time.After(5 * time.Minute):
				b.Errorf("%d of %d replies sent after 5m", snd.sends.Load(), b.N)
			}
			b.StopTimer()
			stop()
			for range engines {
				<-ran
			}
			for _, e := range engines {
				_ = e.Close(context.Background())
			}
			b.ReportMetric(float64(waits.Load())/float64(b.N), "idle-waits/op")
			b.ReportMetric(float64(snd.sends.Load()-int64(b.N)), "dup-sends")
		})
	}
}

// countingSender is benchSender counting provider calls; done closes at
// the want-th.
type countingSender struct {
	benchSender
	sends atomic.Int64
	want  int64
	done  chan struct{}
}

func (s *countingSender) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return s, nil
}

func (s *countingSender) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if s.sends.Add(1) == s.want {
		close(s.done)
	}
	return s.benchSender.Send(ctx, r)
}

// countingWaiter counts waits and sleeps each out on a timer.
type countingWaiter struct{ n *atomic.Int64 }

func (w countingWaiter) Wait(ctx context.Context, d time.Duration) error {
	w.n.Add(1)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
