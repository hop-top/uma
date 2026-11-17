package model

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func validOperationLookup() OperationLookup {
	return OperationLookup{
		Call:   Call{Correlation: "corr-1"},
		Kind:   ActionConversationTransition,
		Target: convA.Ref(),
		ID:     "op-1",
	}
}

// Validate checks the shape of the key only: Call, an action-shaped kind, a
// well-formed target and an operation ID token. Whether the kind is a
// registered action is the lookup's business (an unknown one finds nothing).
func TestOperationLookupValidate(t *testing.T) {
	requireOK(t, validOperationLookup().Validate())
	ok := map[string]func(*OperationLookup){
		"namespaced kind":   func(l *OperationLookup) { l.Kind = "com.example.ticket_merge" },
		"unregistered kind": func(l *OperationLookup) { l.Kind = "ticket.merge" },
		"tenant target":     func(l *OperationLookup) { l.Target = Ref{Tenant: tenantA, Kind: KindChannel, ID: idA} },
	}
	for name, mut := range ok {
		t.Run("ok/"+name, func(t *testing.T) {
			l := validOperationLookup()
			mut(&l)
			requireOK(t, l.Validate())
		})
	}
	bad := []struct {
		name, field string
		mut         func(*OperationLookup)
	}{
		{"missing kind", "kind", func(l *OperationLookup) { l.Kind = "" }},
		{"one-segment kind", "kind", func(l *OperationLookup) { l.Kind = "transition" }},
		{"uppercase kind", "kind", func(l *OperationLookup) { l.Kind = "Conversation.Transition" }},
		{"kind with space", "kind", func(l *OperationLookup) { l.Kind = "conversation. transition" }},
		{"reserved namespace kind", "kind", func(l *OperationLookup) { l.Kind = "uma.conversation.transition" }},
		{"versioned kind", "kind", func(l *OperationLookup) { l.Kind = "com.example.merge.v2" }},
		{"long kind", "kind", func(l *OperationLookup) { l.Kind = "com.example." + strings.Repeat("a", MaxNameLength) }},
		{"empty ID", "id", func(l *OperationLookup) { l.ID = "" }},
		{"malformed ID", "id", func(l *OperationLookup) { l.ID = " op-1" }},
		{"zero target", "target.tenant", func(l *OperationLookup) { l.Target = Ref{} }},
		{"target without kind", "target.kind", func(l *OperationLookup) { l.Target.Kind = "" }},
		{"target without ID", "target.id", func(l *OperationLookup) { l.Target.ID = "" }},
		{"tenant target with inbox", "target.inbox", func(l *OperationLookup) {
			l.Target = Ref{Tenant: tenantA, Kind: KindChannel, Inbox: idA, ID: idB}
		}},
		{"missing correlation", "correlation", func(l *OperationLookup) { l.Correlation = "" }},
	}
	for _, tc := range bad {
		t.Run("bad/"+tc.name, func(t *testing.T) {
			l := validOperationLookup()
			tc.mut(&l)
			requireError(t, l.Validate(), Validation, tc.field)
		})
	}
}

// The principal supplies tenant and caller: the request carries neither, on
// the wire or as a Go field.
func TestOperationLookupHasNoCallerField(t *testing.T) {
	data, err := json.Marshal(validOperationLookup())
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	slices.Sort(got)
	if want := []string{"correlation", "id", "kind", "target"}; !slices.Equal(got, want) {
		t.Fatalf("wire keys = %v, want %v", got, want)
	}
	forbidden := map[reflect.Type]bool{
		reflect.TypeFor[TenantID](): true, reflect.TypeFor[ActorID](): true, reflect.TypeFor[Principal](): true,
	}
	typ := reflect.TypeFor[OperationLookup]()
	for i := range typ.NumField() {
		if f := typ.Field(i); forbidden[f.Type] {
			t.Errorf("OperationLookup.%s has type %s: tenant and caller come from the principal", f.Name, f.Type)
		}
	}
}
