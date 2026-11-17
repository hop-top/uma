package model

import (
	"strings"
	"testing"
)

func TestCreateContact(t *testing.T) {
	email := Attribute{Kind: AttributeEmail, Value: "ana@example.com"}
	phone := Attribute{Kind: AttributePhone, Value: "+14155550100"}
	many := make([]Attribute, MaxContactAttributes+1)
	for i := range many {
		many[i] = Attribute{Kind: AttributeEmail, Value: strings.Repeat("a", i+1) + "@example.com"}
	}
	runRequestCases(t, map[string]requestCase{
		"valid":              {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: "Ana", Attributes: []Attribute{email, phone}}, ""},
		"kind bot":           {CreateContact{Command: cmd, Tenant: tenantA, Kind: ContactKindBot, DisplayName: "Helper"}, ""},
		"bad kind":           {CreateContact{Command: cmd, Tenant: tenantA, Kind: "company", DisplayName: "Ana"}, "kind"},
		"revision":           {CreateContact{Command: cmdRev, Tenant: tenantA, DisplayName: "Ana"}, "expected_revision"},
		"no tenant":          {CreateContact{Command: cmd, DisplayName: "Ana"}, "tenant"},
		"no name":            {CreateContact{Command: cmd, Tenant: tenantA}, "display_name"},
		"padded name":        {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: " Ana"}, "display_name"},
		"control in name":    {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: "A\x00na"}, "display_name"},
		"long name":          {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: strings.Repeat("a", MaxContactDisplayNameRunes+1)}, "display_name"},
		"name at the limit":  {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: strings.Repeat("a", MaxContactDisplayNameRunes)}, ""},
		"no correlation":     {CreateContact{Command: noCall, Tenant: tenantA, DisplayName: "Ana"}, "correlation"},
		"too many":           {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: "Ana", Attributes: many}, "attributes"},
		"unknown kind":       {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: "Ana", Attributes: []Attribute{{Kind: "fax", Value: "1"}}}, "attributes[0]"},
		"unnormalized":       {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: "Ana", Attributes: []Attribute{{Kind: AttributeEmail, Value: "ana@EXAMPLE.com"}}}, "attributes[0]"},
		"verified":           {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: "Ana", Attributes: []Attribute{{Kind: AttributeEmail, Value: "ana@example.com", Verified: true}}}, "attributes[0].verified"},
		"duplicate":          {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: "Ana", Attributes: []Attribute{email, email}}, "attributes[1]"},
		"same value, kinds":  {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: "Ana", Attributes: []Attribute{email, phone, {Kind: AttributeEmail, Value: "ana@example.org"}}}, ""},
		"attributes at most": {CreateContact{Command: cmd, Tenant: tenantA, DisplayName: "Ana", Attributes: many[:MaxContactAttributes]}, ""},
	})
}

func TestUpdateContact(t *testing.T) {
	runRequestCases(t, map[string]requestCase{
		"valid":        {UpdateContact{Command: cmdRev, Contact: contact, DisplayName: "Ana M."}, ""},
		"no revision":  {UpdateContact{Command: cmd, Contact: contact, DisplayName: "Ana M."}, "expected_revision"},
		"no contact":   {UpdateContact{Command: cmdRev, DisplayName: "Ana M."}, "contact.tenant"},
		"no leaf":      {UpdateContact{Command: cmdRev, Contact: ContactRef{Tenant: tenantA}, DisplayName: "Ana M."}, "contact.id"},
		"no name":      {UpdateContact{Command: cmdRev, Contact: contact}, "display_name"},
		"verified":     {UpdateContact{Command: cmdRev, Contact: contact, DisplayName: "A", Attributes: []Attribute{{Kind: AttributePhone, Value: "+14155550100", Verified: true}}}, "attributes[0].verified"},
		"bad phone":    {UpdateContact{Command: cmdRev, Contact: contact, DisplayName: "A", Attributes: []Attribute{{Kind: AttributePhone, Value: "0155"}}}, "attributes[0]"},
		"no operation": {UpdateContact{Command: Command{Call: call, ExpectedRevision: "r1"}, Contact: contact, DisplayName: "A"}, "operation"},
	})
}

func TestContactCloneSharesNoAttributes(t *testing.T) {
	c := Contact{Attributes: []Attribute{{Kind: AttributeEmail, Value: "ana@example.com"}}}
	got := c.Clone()
	got.Attributes[0].Value = "changed"
	if c.Attributes[0].Value != "ana@example.com" {
		t.Fatal("Clone shares the attribute slice")
	}
	if (Contact{}).Clone().Attributes != nil {
		t.Fatal("nil attributes became non-nil")
	}
}
