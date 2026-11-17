package model

import (
	"encoding/json"
	"testing"
)

func TestConversationStatusAndModes(t *testing.T) {
	for _, s := range []string{ConversationOpen, ConversationPending, ConversationResolved, ConversationClosed} {
		if !IsConversationStatus(s) {
			t.Errorf("%q not a conversation status", s)
		}
	}
	for _, s := range []string{"", "snoozed", "Open"} {
		if IsConversationStatus(s) {
			t.Errorf("%q reported as conversation status", s)
		}
	}
	modes := []string{ModePerContact, ModePerRoom, ModePerThread, ModePerWorkItem, ModePerAgentContext}
	want := []string{"per_contact", "per_room", "per_thread", "per_work_item", "per_agent_context"}
	for i, m := range modes {
		if m != want[i] || !IsSelectionMode(m) {
			t.Errorf("mode %q invalid", m)
		}
	}
	if IsSelectionMode("") || IsSelectionMode("per_inbox") {
		t.Error("unknown mode reported valid")
	}
}

func TestLinkAssuranceOrder(t *testing.T) {
	order := []LinkAssurance{
		LinkAssuranceNone, LinkAssuranceAsserted, LinkAssuranceImported,
		LinkAssuranceAttributeMatch, LinkAssuranceHostAuthenticated, LinkAssuranceProven,
	}
	wire := []string{"none", "asserted", "imported", "attribute_match", "host_authenticated", "proven"}
	for i, a := range order {
		if string(a) != wire[i] {
			t.Fatalf("assurance %d = %q, want %q", i, a, wire[i])
		}
		requireOK(t, a.Validate())
		if a.Rank() != i {
			t.Fatalf("%q rank = %d, want %d", a, a.Rank(), i)
		}
		if i > 0 && !order[i-1].Below(a) {
			t.Fatalf("%q not below %q", order[i-1], a)
		}
		if a.Below(a) {
			t.Fatalf("%q below itself", a)
		}
	}
	unknown := LinkAssurance("certain")
	requireError(t, unknown.Validate(), Validation, "assurance")
	requireError(t, LinkAssurance("").Validate(), Validation, "assurance")
	if unknown.Rank() != -1 || unknown.Below(LinkAssuranceNone) || LinkAssuranceProven.Below(unknown) {
		t.Fatal("unknown assurance must rank below nothing and above nothing")
	}
}

func TestLinkClassAndContactKinds(t *testing.T) {
	for _, c := range []LinkClass{LinkClassLink, LinkClassRelink, LinkClassUnlink, LinkClassMerge} {
		requireOK(t, c.Validate())
	}
	requireError(t, LinkClass("swap").Validate(), Validation, "class")
	for _, k := range []string{ContactKindPerson, ContactKindBot, ContactKindAgent} {
		if !IsContactKind(k) {
			t.Errorf("%q not a contact kind", k)
		}
	}
	if IsContactKind("") || IsContactKind("org") {
		t.Error("unknown contact kind reported valid")
	}
	if IsContactKind(AuthorKindSystem) {
		t.Error("the system author kind reported as a contact kind")
	}
}

func TestEventValidate(t *testing.T) {
	res := msgA.Ref()
	valid := Event{
		Version: 1, ID: idD, Kind: "message.received", Tenant: tenantA, Inbox: inboxA.ID,
		Resource: res, Audience: Audience{Visibility: Public},
	}
	requireOK(t, valid.Validate())
	withCause := valid
	withCause.Causation = idB
	requireOK(t, withCause.Validate())

	tenantRef := ChannelRef{Tenant: tenantA, ID: idB}.Ref()
	tenantEvent := valid
	tenantEvent.Resource, tenantEvent.Inbox = tenantRef, ""
	requireOK(t, tenantEvent.Validate())

	mutate := func(f func(*Event)) Event { e := valid; f(&e); return e }
	cases := map[string]struct {
		event Event
		field string
	}{
		"bad id":          {mutate(func(e *Event) { e.ID = badID }), "id"},
		"no kind":         {mutate(func(e *Event) { e.Kind = "" }), "kind"},
		"bad resource":    {mutate(func(e *Event) { e.Resource.Kind = "" }), "resource.kind"},
		"tenant mismatch": {mutate(func(e *Event) { e.Tenant = tenantB }), "tenant"},
		"inbox mismatch":  {mutate(func(e *Event) { e.Inbox = idB }), "inbox"},
		"inbox dropped":   {mutate(func(e *Event) { e.Inbox = "" }), "inbox"},
		"inbox on tenant": {mutate(func(e *Event) { e.Resource = tenantRef }), "inbox"},
		"bad causation":   {mutate(func(e *Event) { e.Causation = "x" }), "causation"},
		"no audience":     {mutate(func(e *Event) { e.Audience = Audience{} }), "audience.visibility"},
		"bad correlation": {mutate(func(e *Event) { e.Correlation = "\n" }), "correlation"},
		"bad payload":     {mutate(func(e *Event) { e.Payload = Extension{Name: "a.b"} }), "payload.version"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) { requireError(t, tc.event.Validate(), Validation, tc.field) })
	}
}

func TestOperationTypedResults(t *testing.T) {
	r := msgA.Ref()
	op := Operation{Result: &r, ResultMessage: &msgA}
	if op.ResultMessage.Ref() != *op.Result {
		t.Fatal("typed result disagrees with generic result")
	}
	s := StreamRef{Conversation: convA, ID: idD}
	sr := s.Ref()
	op = Operation{Result: &sr, ResultStream: &s}
	if op.ResultStream.Ref() != *op.Result {
		t.Fatal("typed stream result disagrees with generic result")
	}
}

// A link record carries the actor's audit reason and its redaction state on
// the wire, beside the policy provenance.
func TestIdentityLinkReasonAndRedacted(t *testing.T) {
	b, err := json.Marshal(IdentityLink{Reason: "wrong_person", Redacted: true, ReasonCode: "policy-7"})
	requireOK(t, err)
	var fields map[string]json.RawMessage
	requireOK(t, json.Unmarshal(b, &fields))
	for key, want := range map[string]string{"reason": `"wrong_person"`, "redacted": `true`, "reason_code": `"policy-7"`} {
		if got := string(fields[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
}
