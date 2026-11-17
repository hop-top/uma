package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHeldStatesAreDeliveryStates(t *testing.T) {
	for _, s := range []DeliveryState{Held, Expired} {
		if !IsDeliveryState(s) {
			t.Errorf("IsDeliveryState(%q) = false", s)
		}
	}
	if Held != "held" || Expired != "expired" {
		t.Fatalf("held states = %q, %q", Held, Expired)
	}
}

func TestIsPluginActor(t *testing.T) {
	for a, want := range map[ActorID]bool{
		"plugin:triage": true, "plugin:": false, "plugin": false,
		"user-1": false, "Plugin:x": false, "xplugin:x": false,
	} {
		if got := a.IsPlugin(); got != want {
			t.Errorf("%q.IsPlugin() = %v, want %v", a, got, want)
		}
	}
}

// An operation that was never held carries no held block on the wire.
func TestOperationWithoutHeldBlockOmitsIt(t *testing.T) {
	b, err := json.Marshal(Operation{Ref: OperationRef{Tenant: "t", ID: "i"}, State: Queued})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["held"]; ok {
		t.Fatalf("held on the wire: %s", b)
	}
	b, _ = json.Marshal(Acceptance{})
	if string(b) != `{"operation":{"ref":{"tenant":"","id":""},"state":"","updated_at":"0001-01-01T00:00:00Z","revision":""},"durable":false,"replayed":false,"already_held":false}` {
		t.Fatalf("acceptance wire = %s", b)
	}
}

func TestHeldBlockCloneIsDeep(t *testing.T) {
	at := time.Unix(10, 0).UTC()
	op := Operation{State: Held, Held: &HeldBlock{
		Proposer: "plugin:a", Preview: &Extension{Name: "p", Version: 1, Data: []byte(`{"a":1}`)},
		TrustLabels: map[string]string{"k": "v"}, ExpiresAt: &at, DecidedAt: &at,
		EditedArgs:          &Extension{Name: "e", Version: 1, Data: []byte(`{}`)},
		Edits:               []HeldEdit{{By: "u", At: at, Before: Extension{Name: "b", Data: []byte(`1`)}, After: Extension{Name: "a", Data: []byte(`2`)}}},
		ProposedFingerprint: []byte{1, 2},
	}}
	c := op.Clone()
	if !reflect.DeepEqual(op, c) {
		t.Fatalf("clone differs: %+v vs %+v", op, c)
	}
	c.Held.Preview.Data[0] = 'X'
	c.Held.TrustLabels["k"] = "z"
	c.Held.Edits[0].Before.Data[0] = '9'
	c.Held.ProposedFingerprint[0] = 9
	*c.Held.ExpiresAt = at.Add(time.Hour)
	if op.Held.Preview.Data[0] != '{' || op.Held.TrustLabels["k"] != "v" || op.Held.Edits[0].Before.Data[0] != '1' ||
		op.Held.ProposedFingerprint[0] != 1 || !op.Held.ExpiresAt.Equal(at) {
		t.Fatalf("clone aliases the original: %+v", op.Held)
	}
}

func TestHeldDecisions(t *testing.T) {
	for _, d := range []HeldDecision{HeldApproved, HeldRejected, HeldEdited, HeldExpired} {
		if !IsHeldDecision(d) {
			t.Errorf("IsHeldDecision(%q) = false", d)
		}
	}
	if IsHeldDecision("") || IsHeldDecision("maybe") {
		t.Fatal("unknown decision accepted")
	}
}

func TestHeldIntentCloneAndOmitEmpty(t *testing.T) {
	h := HeldBlock{Intent: &Extension{Name: "i", Version: 1, Data: []byte(`{"a":1}`)}}
	c := h.Clone()
	c.Intent.Data[0] = 'X'
	if h.Intent.Data[0] != '{' {
		t.Fatal("Clone shares the intent data")
	}
	b, _ := json.Marshal(HeldBlock{})
	if strings.Contains(string(b), `"intent"`) {
		t.Fatalf("empty block carries intent: %s", b)
	}
}

func TestDecideHeldBatchValidation(t *testing.T) {
	d := DecideHeld{Call: Call{Correlation: "c"}, Operation: OperationRef{Tenant: "t", ID: "00000007-0000-7000-8000-000000000001"}, ExpectedRevision: "r"}
	if err := d.Validate(); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	d.Batch = "action-1"
	if err := d.Validate(); err != nil {
		t.Fatalf("batch: %v", err)
	}
	d.Batch = string(make([]byte, MaxTrustBatchBytes+1))
	if err := d.Validate(); err == nil {
		t.Fatal("oversized batch accepted")
	}
	d.Batch = "a\x00b"
	if err := d.Validate(); err == nil {
		t.Fatal("NUL batch accepted")
	}
}
