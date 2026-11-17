package conformance

import (
	"context"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Edit answers like a send, consuming the same script.
func (i *faultInstance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	p := i.p
	p.mu.Lock()
	var fn faultSend
	if len(p.script) > 0 {
		fn, p.script = p.script[0], p.script[1:]
	}
	p.edits = append(p.edits, r)
	p.calls = append(p.calls, "edit")
	p.mu.Unlock()
	if fn == nil {
		fn = applyAccepted
	}
	out, err, _ := fn(ctx, adapter.SendRequest{Effect: r.Effect, Content: r.Content})
	return out, err
}

// edited returns the edits that reached the provider.
func (p *faultProvider) edited() []adapter.EditRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]adapter.EditRequest(nil), p.edits...)
}

// edit queues an edit of m to text.
func (p *faultProc) edit(t *testing.T, opID model.OperationID, m model.Message, text string) model.OperationRef {
	t.Helper()
	a, err := p.client.Messages().Edit(p.ctx, model.EditMessage{
		Command: model.Command{Call: model.Call{Correlation: "c-" + model.CorrelationID(opID)}, Operation: opID, ExpectedRevision: m.Revision},
		Message: m.Ref, Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	return a.Operation.Ref
}
