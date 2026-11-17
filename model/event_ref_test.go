package model

import "testing"

func TestEventRef(t *testing.T) {
	ref, err := NewEventRef("t1", "00000000-0000-7000-8000-0000000000e1")
	if err != nil || ref != (EventRef{Tenant: "t1", ID: "00000000-0000-7000-8000-0000000000e1"}) || ref.Scope() != "t1" || ref.IsZero() {
		t.Fatalf("NewEventRef = %+v, %v", ref, err)
	}
	if !(EventRef{}).IsZero() {
		t.Fatal("zero ref not zero")
	}
	for name, r := range map[string]EventRef{
		"no tenant": {ID: "00000000-0000-7000-8000-0000000000e1"},
		"no id":     {Tenant: "t1"},
	} {
		if err := r.Validate(); !IsCode(err, Validation) {
			t.Errorf("%s: Validate = %v, want Validation", name, err)
		}
		if got, err := NewEventRef(r.Tenant, r.ID); err == nil || got != (EventRef{}) {
			t.Errorf("%s: NewEventRef = %+v, %v; want the zero ref and an error", name, got, err)
		}
	}
	if err := (EventRef{Tenant: "t1"}).ValidateScope(); err != nil {
		t.Errorf("ValidateScope without ID = %v, want nil", err)
	}
}

func TestLookupValidatesEventRef(t *testing.T) {
	call := Call{Correlation: "c"}
	if err := (Lookup[EventRef]{Call: call, Ref: EventRef{Tenant: "t1", ID: "00000000-0000-7000-8000-0000000000e1"}}).Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	if err := (Lookup[EventRef]{Call: call, Ref: EventRef{Tenant: "t1"}}).Validate(); !IsCode(err, Validation) {
		t.Fatalf("Validate without ID = %v, want Validation", err)
	}
}
