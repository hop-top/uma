package postgres

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// BenchmarkConcurrentReplies accepts b.N replies through the engine
// (Messages.Reply) from the given number of callers at once, all replying
// to one conversation or each to its own. ns/op is wall time per reply;
// reruns/op counts units lost to concurrent units, which the engine
// reruns; aborts/op the server's serialization failures; lost the replies
// whose unit still lost after its last rerun, which reach the caller as
// model.Transient; p50, p95 and p99 the latency of a reply, reruns
// included.
func BenchmarkConcurrentReplies(b *testing.B) {
	for _, callers := range []int{4, 16} {
		for _, own := range []bool{false, true} {
			conversations := "one"
			if own {
				conversations = "own"
			}
			b.Run(fmt.Sprintf("callers=%d/conversations=%s", callers, conversations), func(b *testing.B) {
				name := benchCreate(b)
				cfg := server.admin.Config().Copy()
				cfg.ConnConfig.Database = name
				r := contentionRigOn(b, cfg, callers, int32(2*callers)) // #nosec G115 -- small constant
				clients := make([]uma.Client, callers)
				for i := range clients {
					clients[i] = r.client(b, fmt.Sprintf("agent-%d", i))
				}
				lat := make([]time.Duration, b.N)
				var lost atomic.Int64
				b.ResetTimer()
				var wg sync.WaitGroup
				for c := range callers {
					conv := r.convs[0]
					if own {
						conv = r.convs[c]
					}
					wg.Go(func() {
						for k := c; k < b.N; k += callers {
							began := time.Now()
							_, err := clients[c].Messages().Reply(b.Context(), model.Reply{
								Command:      contentionCommand(fmt.Sprintf("bench-reply-%d", k)),
								Conversation: conv,
								Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "reply"}}},
							})
							lat[k] = time.Since(began)
							switch {
							case model.IsCode(err, model.Transient):
								lost.Add(1)
							case err != nil:
								b.Errorf("reply %d: %v", k, err)
							}
						}
					})
				}
				wg.Wait()
				b.StopTimer()
				ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
				b.ReportMetric(float64(r.uc.lost.Load())/float64(b.N), "reruns/op")
				b.ReportMetric(float64(r.tr.total())/float64(b.N), "aborts/op")
				b.ReportMetric(float64(lost.Load()), "lost")
				b.ReportMetric(ms(percentile(lat, 0.5)), "p50-ms")
				b.ReportMetric(ms(percentile(lat, 0.95)), "p95-ms")
				b.ReportMetric(ms(percentile(lat, 0.99)), "p99-ms")
			})
		}
	}
}
