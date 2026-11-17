package postgres

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// BenchmarkAppendEvents commits b.N units of one tenant from the given
// number of callers at once, each unit appending one event about its
// caller's own conversation. A unit lost to a concurrent one reruns as the
// engine reruns a caller's unit: up to 16 times, after a full-jitter pause
// from 1ms doubling to 64ms. ns/op is wall time per unit; reruns/op counts
// reruns, lost the units still losing after the last.
func BenchmarkAppendEvents(b *testing.B) {
	for _, callers := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("callers=%d", callers), func(b *testing.B) {
			ctx := b.Context()
			st := benchDatabase(b)
			const tenant model.TenantID = "bench-append-events"
			in := model.InboxRef{Tenant: tenant, ID: benchID(0xf0, 0)}
			var next, reruns, lost atomic.Int64
			unit := func(caller int) {
				i := int(next.Add(1))
				conv := model.ConversationRef{Inbox: in, ID: benchID(0xf1, caller)}
				at := time.Now()
				ev := model.Event{
					Version: 1, ID: benchID(0xf2, i), Kind: "bench.appended", Tenant: tenant, Inbox: in.ID,
					Resource: conv.Ref(), Actor: "bench", OccurredAt: at, RecordedAt: at,
					Audience: model.Audience{Visibility: model.Internal},
				}
				pause := time.Millisecond
				for attempt := 1; ; attempt++ {
					err := st.Transact(ctx, tenant, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{ev}) })
					switch {
					case err == nil:
						return
					case !model.IsCode(err, model.Transient) || attempt == 16:
						lost.Add(1)
						return
					}
					reruns.Add(1)
					time.Sleep(rand.N(pause) + 1) // #nosec G404 -- jitter, not security
					pause = min(2*pause, 64*time.Millisecond)
				}
			}
			b.ResetTimer()
			var wg sync.WaitGroup
			for c := range callers {
				wg.Go(func() {
					for k := c; k < b.N; k += callers {
						unit(c)
					}
				})
			}
			wg.Wait()
			b.StopTimer()
			b.ReportMetric(float64(reruns.Load())/float64(b.N), "reruns/op")
			b.ReportMetric(float64(lost.Load()), "lost")
		})
	}
}
