package model

import (
	"slices"
	"testing"
)

func flagPtr(b bool) *bool { return &b }

func TestSelectionValidate(t *testing.T) {
	cases := []struct {
		name string
		sel  Selection
		ok   bool
	}{
		{"per contact true", Selection{Mode: ModePerContact, LockToSingle: flagPtr(true), LifecycleRules: RuleSetContactLifecycleV1}, true},
		{"per contact false", Selection{Mode: ModePerContact, LockToSingle: flagPtr(false), LifecycleRules: RuleSetContactLifecycleV1}, true},
		{"per contact unset flag", Selection{Mode: ModePerContact, LifecycleRules: RuleSetContactLifecycleV1}, false},
		{"per contact no rules", Selection{Mode: ModePerContact, LockToSingle: flagPtr(true)}, false},
		{"per contact unknown rules", Selection{Mode: ModePerContact, LockToSingle: flagPtr(true), LifecycleRules: "contact-lifecycle/v0"}, false},
		{"per room flag true", Selection{Mode: ModePerRoom, LockToSingle: flagPtr(true)}, false},
		{"per room flag false", Selection{Mode: ModePerRoom, LockToSingle: flagPtr(false)}, false},
		{"per room rules", Selection{Mode: ModePerRoom, LifecycleRules: RuleSetContactLifecycleV1}, false},
		{"per thread", Selection{Mode: ModePerThread}, true},
		{"per work item", Selection{Mode: ModePerWorkItem}, true},
		{"per agent context", Selection{Mode: ModePerAgentContext}, true},
		{"unknown mode", Selection{Mode: "per_planet"}, false},
		{"empty mode", Selection{}, false},
		{"policy token", Selection{Mode: ModePerRoom, Policy: "custom"}, true},
		{"bad policy token", Selection{Mode: ModePerRoom, Policy: " custom"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.sel.Validate()
			if c.ok {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, "")
		})
	}
}

func TestEnvelopeKinds(t *testing.T) {
	for _, k := range []string{
		EnvelopeMessage, EnvelopeAutomatedMessage, EnvelopeMessageEdited, EnvelopeMessageDeleted,
		EnvelopeReactionAdded, EnvelopeReactionRemoved, EnvelopeStatus, EnvelopeWorkItem,
		EnvelopeExtensionEvent, EnvelopeConversationState,
	} {
		if !IsEnvelopeKind(k) {
			t.Errorf("IsEnvelopeKind(%q) = false", k)
		}
	}
	if IsEnvelopeKind("message.created") || IsEnvelopeKind("") {
		t.Error("undeclared kind accepted")
	}
	for k, want := range map[string]bool{
		EnvelopeMessage: false, EnvelopeAutomatedMessage: false, EnvelopeStatus: false, EnvelopeWorkItem: false,
		EnvelopeExtensionEvent: false, EnvelopeConversationState: false, EnvelopeMessageEdited: true, EnvelopeMessageDeleted: true,
		EnvelopeReactionAdded: true, EnvelopeReactionRemoved: true,
	} {
		if got := IsMessageUpdateKind(k); got != want {
			t.Errorf("IsMessageUpdateKind(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestParseMsgIDs(t *testing.T) {
	cases := map[string][]string{
		"":                                   nil,
		"<m1@a.example>":                     {"<m1@a.example>"},
		"<m0@a.example> <m1@a.example>":      {"<m0@a.example>", "<m1@a.example>"},
		"<m0@a.example>\r\n\t<m1@a.example>": {"<m0@a.example>", "<m1@a.example>"},
		"your message of Monday <m1@a.example> (Jane)": {"<m1@a.example>"},
		"<no-at> <m1@a.example>":                       {"<m1@a.example>"},
		"<@a.example> <m@> <a b@c> <m1@a.example>":     {"<m1@a.example>"},
		"<a@b@c> <m1@a.example>":                       {"<m1@a.example>"},
		"junk only":                                    nil,
		"<unterminated@a.example":                      nil,
	}
	for in, want := range cases {
		if got := ParseMsgIDs(in); !slices.Equal(got, want) {
			t.Errorf("ParseMsgIDs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmailHeadersNormalize(t *testing.T) {
	ch := ChannelRef{Tenant: tenantA, ID: idA}
	ref := func(kind, id string) ExternalRef {
		return ExternalRef{Channel: ch, Namespace: EmailHeaderNamespace, Kind: kind, ID: id}
	}
	h := EmailHeaders{
		MessageID:     " <m9@a.example> ",
		InReplyTo:     "Re: <m1@a.example> <m2@a.example>",
		References:    "<m0@a.example> <m1@a.example>",
		ListID:        "Dev list <dev.lists.example>",
		AutoSubmitted: "auto-replied",
	}
	got := h.Normalize(ch)
	if got.MessageID != "<m9@a.example>" || !got.Automated {
		t.Fatalf("normalized = %+v", got)
	}
	want := []ExternalRef{
		ref(RefKindEmailMessageID, "<m9@a.example>"),
		ref(RefKindEmailInReplyTo, "<m1@a.example>"),
		ref(RefKindEmailReferences, "<m0@a.example>"),
		ref(RefKindEmailReferences, "<m1@a.example>"),
		ref(RefKindEmailListID, "dev.lists.example"),
	}
	if !slices.Equal(got.References, want) {
		t.Fatalf("references = %+v\nwant %+v", got.References, want)
	}
	for _, r := range got.References {
		requireOK(t, r.Validate())
	}

	empty := EmailHeaders{AutoSubmitted: "no"}.Normalize(ch)
	if empty.MessageID != "" || empty.Automated || len(empty.References) != 0 {
		t.Fatalf("empty headers normalized to %+v", empty)
	}
}

func TestIsAutoSubmitted(t *testing.T) {
	for v, want := range map[string]bool{
		"": false, "no": false, "No": false, " no ": false, "no; comment": false,
		"auto-replied": true, "auto-generated": true, "Auto-Notified": true, "x-custom": true,
	} {
		if got := IsAutoSubmitted(v); got != want {
			t.Errorf("IsAutoSubmitted(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestProviderMessageRef(t *testing.T) {
	ch := ChannelRef{Tenant: tenantA, ID: idA}
	r := ProviderMessageRef(ch, "<m1@a.example>")
	requireOK(t, r.Validate())
	if r.Channel != ch || r.Kind != RefKindMessage || r.ID != "<m1@a.example>" || r.ParentID != "" {
		t.Fatalf("ProviderMessageRef = %+v", r)
	}
}

func TestProviderInteractionRef(t *testing.T) {
	ch := ChannelRef{Tenant: tenantA, ID: idA}
	r := ProviderInteractionRef(ch, "i-1")
	requireOK(t, r.Validate())
	if r.Channel != ch || r.Namespace != ProviderMessageNamespace || r.Kind != RefKindInteraction || r.ID != "i-1" || r.ParentID != "" {
		t.Fatalf("ProviderInteractionRef = %+v", r)
	}
	if r == ProviderMessageRef(ch, "i-1") {
		t.Fatal("an interaction ref equals a message ref of the same ID")
	}
}
