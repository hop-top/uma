package privacy_test

import (
	"context"
	"sync"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
)

// labelPolicy allows everything except label.read on the hidden labels.
type labelPolicy struct {
	mu     sync.Mutex
	hidden map[model.Ref]bool
}

func (p *labelPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return policy.Decision{Allow: a.Action != model.ActionLabelRead || !p.hidden[a.Resource]}, nil
}

func (p *labelPolicy) hide(ref model.Ref) {
	p.mu.Lock()
	p.hidden[ref] = true
	p.mu.Unlock()
}

// An export carries, after its conversations, one label line per label
// those conversations hold that the caller may read; refs alone would be
// unintelligible. Open reauthorizes the label lines like every record.
func TestExportIncludesReferencedLabels(t *testing.T) {
	pol := &labelPolicy{hidden: map[model.Ref]bool{}}
	r := enginetest.NewRig(t, withBlobs(&portstest.MemoryBlobs{}), func(c *engine.Config, _ *enginetest.OutFactory) { c.Authorizer = pol })
	labels := make([]model.LabelRef, 3)
	for i, name := range []string{"Urgent", "Billing", "Unused"} {
		a, err := r.C.Labels().Configure(r.Ctx, model.ConfigureLabel{
			Command: cmd(model.OperationID("op-label-"+name), ""), Label: model.Label{Ref: model.LabelRef{Tenant: enginetest.TenantA}, Name: name},
		})
		if err != nil {
			t.Fatalf("Configure: %v", err)
		}
		if labels[i], err = a.Operation.Result.AsLabel(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.C.Conversations().AddLabels(r.Ctx, model.ConversationLabels{
		Command: cmd("op-add", ""), Conversation: r.Conv.Ref, Labels: labels[:2],
	}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	pol.hide(labels[1].Ref())
	a, _, lines := export(t, r.C, r.Ctx, exportReq("op-export", model.Filter{Conversation: &r.Conv.Ref}))
	got := ofType[model.Label](t, lines, model.ExportLineLabel)
	if len(got) != 1 || got[0].Ref != labels[0] || got[0].Name != "Urgent" {
		t.Fatalf("label lines = %+v, want the readable referenced label only", got)
	}
	convs := ofType[model.Conversation](t, lines, model.ExportLineConversation)
	if len(convs) != 1 || len(convs[0].Labels) != 2 {
		t.Fatalf("conversation lines = %+v, want the refs of both labels", convs)
	}
	ref, _ := a.Operation.Result.AsExport()
	pol.hide(labels[0].Ref())
	_, _, err := r.C.Privacy().Open(r.Ctx, model.Lookup[model.ExportRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
	enginetest.RequireCode(t, err, model.Unauthorized, "")
}
