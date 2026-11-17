package runtime_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

// executingJob is a job carrying an extension action, as an extension
// effect does.
type executingJob struct {
	testJob
	action model.Extension
}

func (j executingJob) RequestedAction() model.Extension { return j.action }

// An extension effect's action travels with its reconciliation, as a work
// item act's status does; a job that carries none hands none.
func TestReconcileRequestCarriesExtensionAction(t *testing.T) {
	eff := adapter.Effect{
		Operation: model.OperationRef{Tenant: tenantA, ID: "00000003-0000-7000-8000-000000000011"},
		Attempt:   "00000003-0000-7000-8000-000000000012",
	}
	action := model.Extension{Name: "com.example.issue.update", Version: 1, Data: json.RawMessage(`{"labels":["bug"]}`)}
	at := time.Unix(1_700_000_000, 0).UTC()

	req := runtime.ReconcileRequestOf(executingJob{action: action}, eff, at)
	if req.Effect != eff || req.Capability != model.CapabilitySend || !req.AttemptedAt.Equal(at) {
		t.Fatalf("request = %+v, want the effect, the job's first capability and the attempt time", req)
	}
	if req.Action.Name != action.Name || req.Action.Version != action.Version || !bytes.Equal(req.Action.Data, action.Data) {
		t.Fatalf("request action = %+v, want %+v", req.Action, action)
	}

	plain := runtime.ReconcileRequestOf(testJob{}, eff, at)
	if plain.Action.Name != "" || plain.Action.Version != 0 || plain.Action.Data != nil {
		t.Fatalf("request of a job without an action carries %+v, want none", plain.Action)
	}
}
