package ingress_test

import (
	"fmt"
	"testing"
)

// BenchmarkIngestGrowth measures one inbound message on the memory store
// when the tenant already holds stored messages. Unlike BenchmarkIngestCommit,
// whose ns/op is a mean over a store that grows while it runs, the store is
// filled to a pinned size first (untimed, through Sink.Accept), so each
// ns/op is the marginal cost at that size. Use a small -benchtime=Nx: the
// measured messages are committed, so the store grows by N during the run,
// and the size in the name is the size at the start.
//
// first_contact gives every message a new sender, so each one adds a
// contact, an identity and a conversation. existing_conversation sends
// every message from one sender into one conversation, which grows to the
// pinned size.
func BenchmarkIngestGrowth(b *testing.B) {
	for _, tc := range []struct {
		name      string
		newSender bool
	}{
		{"first_contact", true},
		{"existing_conversation", false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for _, n := range []int{100, 1_000, 10_000} {
				b.Run(fmt.Sprintf("stored=%d", n), func(b *testing.B) {
					snk, ch := ingestBench(b)
					ctx := b.Context()
					from := func(i int) string {
						if tc.newSender {
							return fmt.Sprintf("u-%d", i)
						}
						return "u-0"
					}
					for i := range n {
						if _, err := snk.Accept(ctx, benchIngress(ch, i, from(i))); err != nil {
							b.Fatalf("fill %d: %v", i, err)
						}
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := range b.N {
						if _, err := snk.Accept(ctx, benchIngress(ch, n+i, from(n+i))); err != nil {
							b.Fatalf("Accept %d: %v", n+i, err)
						}
					}
				})
			}
		})
	}
}
