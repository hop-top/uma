package conformance

import (
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
)

// redeliveringProvider reports inbound results as a receiver that handed
// an earlier event on again before the one just delivered.
type redeliveringProvider struct {
	Provider
	mu      sync.Mutex
	results []InboundResult
}

func (p *redeliveringProvider) Inbound(model.ChannelRef) []InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]InboundResult(nil), p.results...)
}

func (p *redeliveringProvider) add(r InboundResult) {
	p.mu.Lock()
	p.results = append(p.results, r)
	p.mu.Unlock()
}

// A redelivery of an earlier event is no result of the event just
// delivered: awaitInbound waits for the delivered event itself, so a case
// never stops the receiver before that event reached the sink.
func TestAwaitInboundIgnoresRedeliveries(t *testing.T) {
	p := &redeliveringProvider{results: []InboundResult{{EventID: "evt-1", Acked: true}}}
	e := &adapterEnv{provider: p}
	before := len(p.Inbound(adapterChanA))
	p.add(InboundResult{EventID: "evt-1", Acked: true})
	go func() {
		time.Sleep(20 * time.Millisecond)
		p.add(InboundResult{EventID: "evt-2", Acked: true})
	}()
	got := e.awaitInbound(t, adapterChanA, before, []InboundEvent{{EventID: "evt-2"}})
	if len(got) != 1 || got[0].EventID != "evt-2" {
		t.Fatalf("results = %+v, want the delivered event's", got)
	}
}
