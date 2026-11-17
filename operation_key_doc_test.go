package uma_test

import (
	"reflect"
	"strings"
	"testing"

	"hop.top/uma"
	"hop.top/uma/model"
)

// noOperationKey lists the mutating methods whose godoc names no operation
// key, each with the reason. Operations.Lookup's godoc says the same.
var noOperationKey = map[string]string{
	"Events.Open":  "idempotent per consumer name; repeating recovers",
	"Events.Ack":   "token-guarded; repeating recovers",
	"Events.Close": "repeating recovers",
}

// operationKeyLine is the godoc marker of a mutating method's operation
// key, the Kind and Target Operations.Lookup takes.
const operationKeyLine = "Operation key: kind "

// Every method that accepts a command (returns a model.Acceptance) names
// its operation key in its godoc, unless it is unsupported or listed in
// noOperationKey, and the key's method appears in the Lookup godoc table.
func TestMutatingMethodsNameOperationKey(t *testing.T) {
	docs := interfaceMethodDocs(t, "uma.go")
	rows := readCoverage(t)
	lookup := docs["Operations.Lookup"]
	acceptance := reflect.TypeFor[model.Acceptance]()
	ct := reflect.TypeFor[uma.Client]()
	checked := 0
	for i := range ct.NumMethod() {
		svc := ct.Method(i)
		st := svc.Type.Out(0)
		for j := range st.NumMethod() {
			m := st.Method(j)
			if m.Type.NumOut() == 0 || m.Type.Out(0) != acceptance {
				continue
			}
			name := svc.Name + "." + m.Name
			_, exempt := noOperationKey[name]
			if exempt || rows[name].status == "unsupported" {
				if strings.Contains(docs[name], operationKeyLine) {
					t.Errorf("%s: names an operation key but is exempt or unsupported", name)
				}
				continue
			}
			checked++
			if !strings.Contains(docs[name], operationKeyLine) {
				t.Errorf("%s: godoc has no %q line", name, operationKeyLine+"...")
			}
			if !strings.Contains(lookup, svc.Name+"."+m.Name) && !strings.Contains(lookup, ", "+m.Name) {
				t.Errorf("%s: missing from the Operations.Lookup key table", name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no mutating method checked")
	}
}
