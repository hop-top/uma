package model

import (
	"strings"
	"testing"
)

func TestNewIdentityKey(t *testing.T) {
	k, err := NewIdentityKey(inboxA, "slack.user", " T1:U1 ")
	requireOK(t, err)
	if k.SourceID != " T1:U1 " || k.Namespace != "slack.user" || k.Inbox != inboxA {
		t.Fatalf("key rewritten: %+v", k)
	}
	if k.Scope() != tenantA {
		t.Fatalf("scope = %q", k.Scope())
	}
	for _, ns := range []string{"slack.user", "irc.account", "smtp.address", "a1.b_c-d.e", "x.-y"} {
		_, err := NewIdentityKey(inboxA, ns, "id")
		requireOK(t, err)
	}
	cases := map[string]struct {
		inbox     InboxRef
		namespace string
		source    string
		field     string
	}{
		"bad inbox":           {InboxRef{Tenant: tenantA}, "slack.user", "x", "inbox.id"},
		"empty namespace":     {inboxA, "", "x", "namespace"},
		"single segment":      {inboxA, "slack", "x", "namespace"},
		"uppercase namespace": {inboxA, "Slack.user", "x", "namespace"},
		"underscore first":    {inboxA, "sl_ack.user", "x", "namespace"},
		"trailing dot":        {inboxA, "slack.user.", "x", "namespace"},
		"long namespace":      {inboxA, "a." + strings.Repeat("b", 254), "x", "namespace"},
		"empty source":        {inboxA, "slack.user", "", "source_id"},
		"long source":         {inboxA, "slack.user", strings.Repeat("s", 256), "source_id"},
		"bad utf8 source":     {inboxA, "slack.user", "a\xff", "source_id"},
		"nul source":          {inboxA, "slack.user", "a\x00", "source_id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := NewIdentityKey(tc.inbox, tc.namespace, tc.source)
			requireError(t, err, Validation, tc.field)
			if got != (IdentityKey{}) {
				t.Fatalf("constructor returned %+v with error", got)
			}
		})
	}
	_, err = NewIdentityKey(inboxA, "slack.user", strings.Repeat("s", 255))
	requireOK(t, err)
}

func TestNewExternalRef(t *testing.T) {
	ch := ChannelRef{Tenant: tenantA, ID: idA}
	r, err := NewExternalRef(ch, "slack.team", "thread", " 1700000000.000100 ", "")
	requireOK(t, err)
	if r.ID != " 1700000000.000100 " || r.Scope() != tenantA || r.IsZero() {
		t.Fatalf("external ref = %+v", r)
	}
	if !(ExternalRef{}).IsZero() {
		t.Fatal("zero ExternalRef not IsZero")
	}
	_, err = NewExternalRef(ch, "github.issues", "issue.comment", "42", "7")
	requireOK(t, err)
	_, err = NewExternalRef(ch, "slack.team", "message", strings.Repeat("i", 1024), "")
	requireOK(t, err)

	cases := map[string]struct {
		ch                         ChannelRef
		namespace, kind, id, paren string
		field                      string
	}{
		"bad channel":     {ChannelRef{Tenant: tenantA}, "slack.team", "thread", "1", "", "channel.id"},
		"empty namespace": {ch, "", "thread", "1", "", "namespace"},
		"flat namespace":  {ch, "slack", "thread", "1", "", "namespace"},
		"empty kind":      {ch, "slack.team", "", "1", "", "kind"},
		"bad kind":        {ch, "slack.team", "Thread", "1", "", "kind"},
		"kind dot first":  {ch, "slack.team", ".thread", "1", "", "kind"},
		"empty id":        {ch, "slack.team", "thread", "", "", "id"},
		"long id":         {ch, "slack.team", "thread", strings.Repeat("i", 1025), "", "id"},
		"bad utf8 id":     {ch, "slack.team", "thread", "\xff", "", "id"},
		"nul id":          {ch, "slack.team", "thread", "a\x00", "", "id"},
		"nul parent":      {ch, "slack.team", "thread", "1", "\x00", "parent_id"},
		"long parent":     {ch, "slack.team", "thread", "1", strings.Repeat("p", 1025), "parent_id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := NewExternalRef(tc.ch, tc.namespace, tc.kind, tc.id, tc.paren)
			requireError(t, err, Validation, tc.field)
			if !got.IsZero() {
				t.Fatalf("constructor returned %+v with error", got)
			}
		})
	}
}

func TestNormalizeEmail(t *testing.T) {
	valid := map[string]string{
		"Jane@Example.COM":     "Jane@example.com",
		"jane@example.com":     "jane@example.com",
		"J.Doe+tag@Sub.EX.org": "J.Doe+tag@sub.ex.org",
		"jane@BÜCHER.example":  "jane@bücher.example",
		"j@[127.0.0.1]":        "j@[127.0.0.1]",
	}
	for in, want := range valid {
		t.Run(in, func(t *testing.T) {
			got, err := NormalizeEmail(in)
			requireOK(t, err)
			if got != want {
				t.Fatalf("NormalizeEmail(%q) = %q, want %q", in, got, want)
			}
		})
	}
	for _, in := range []string{
		"", "not an email", "Jane <jane@example.com>", "<jane@example.com>",
		" jane@example.com", "jane@example.com ", "jane@example.com (work)",
		`"john doe"@example.com`, "a@b@c", "j.@x.com",
	} {
		t.Run("invalid/"+in, func(t *testing.T) {
			got, err := NormalizeEmail(in)
			requireError(t, err, Validation, "email")
			if got != "" {
				t.Fatalf("returned %q with error", got)
			}
		})
	}
}

func TestNormalizePhone(t *testing.T) {
	valid := map[string]string{
		"+1 (555) 010-0199":  "+15550100199",
		"+44.20.7946.0958":   "+442079460958",
		"+12":                "+12",
		"+123456789012345":   "+123456789012345",
		"+33 1 23 45 67 89 ": "+33123456789",
	}
	for in, want := range valid {
		t.Run(in, func(t *testing.T) {
			got, err := NormalizePhone(in)
			requireOK(t, err)
			if got != want {
				t.Fatalf("NormalizePhone(%q) = %q, want %q", in, got, want)
			}
		})
	}
	for _, in := range []string{
		"", "15550100199", "+0123", "+1", "+1234567890123456", "+1 555 x100", "++15550100199",
		"+1\t5550100199", "+１２３",
	} {
		t.Run("invalid/"+in, func(t *testing.T) {
			got, err := NormalizePhone(in)
			requireError(t, err, Validation, "phone")
			if got != "" {
				t.Fatalf("returned %q with error", got)
			}
		})
	}
}

func TestAttributeValidate(t *testing.T) {
	cases := map[string]struct {
		attr  Attribute
		field string
	}{
		"email":              {Attribute{Kind: AttributeEmail, Value: "Jane@example.com", Verified: true}, ""},
		"phone":              {Attribute{Kind: AttributePhone, Value: "+15550100199"}, ""},
		"unnormalized email": {Attribute{Kind: AttributeEmail, Value: "Jane@Example.com"}, "value"},
		"unnormalized phone": {Attribute{Kind: AttributePhone, Value: "+1 555 010 0199"}, "value"},
		"invalid email":      {Attribute{Kind: AttributeEmail, Value: "Jane <j@x>"}, "value"},
		"invalid phone":      {Attribute{Kind: AttributePhone, Value: "555"}, "value"},
		"unknown kind":       {Attribute{Kind: "fax", Value: "+15550100199"}, "kind"},
		"whatsapp user id":   {Attribute{Kind: AttributeWhatsAppUserID, Value: "CA.0000000000000002", Verified: true}, ""},
		"whatsapp parent id": {Attribute{Kind: AttributeWhatsAppUserID, Value: "US.ENT.11815799212886844830"}, ""},
		"whatsapp longest":   {Attribute{Kind: AttributeWhatsAppUserID, Value: "US." + strings.Repeat("a", 128)}, ""},
		"whatsapp too long":  {Attribute{Kind: AttributeWhatsAppUserID, Value: "US." + strings.Repeat("a", 129)}, "value"},
		"whatsapp no code":   {Attribute{Kind: AttributeWhatsAppUserID, Value: "0000000000000002"}, "value"},
		"whatsapp prefix":    {Attribute{Kind: AttributeWhatsAppUserID, Value: "whatsapp:CA.0000000000000002"}, "value"},
		"whatsapp lowercase": {Attribute{Kind: AttributeWhatsAppUserID, Value: "ca.0000000000000002"}, "value"},
		"whatsapp empty id":  {Attribute{Kind: AttributeWhatsAppUserID, Value: "CA."}, "value"},
		"whatsapp space":     {Attribute{Kind: AttributeWhatsAppUserID, Value: "CA.00 02"}, "value"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.attr.Validate()
			if tc.field == "" {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, tc.field)
		})
	}
}
