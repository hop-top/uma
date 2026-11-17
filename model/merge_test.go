package model

import (
	"fmt"
	"testing"
)

func TestMergeContactsValidate(t *testing.T) {
	into := ContactRef{Tenant: tenantA, ID: idA}
	other := ContactRef{Tenant: tenantA, ID: idB}
	ok := MergeContacts{
		Command: cmdRev, From: contact, Into: into, ExpectedIntoRevision: "r2",
		Identities: []IdentityRef{ident}, Reason: "duplicate",
	}
	with := func(f func(*MergeContacts)) MergeContacts {
		r := ok
		r.Identities = append([]IdentityRef(nil), ok.Identities...)
		f(&r)
		return r
	}
	runRequestCases(t, map[string]requestCase{
		"valid":               {ok, ""},
		"no identities":       {with(func(r *MergeContacts) { r.Identities = nil }), ""},
		"no revision":         {with(func(r *MergeContacts) { r.ExpectedRevision = "" }), "expected_revision"},
		"no into revision":    {with(func(r *MergeContacts) { r.ExpectedIntoRevision = "" }), "expected_into_revision"},
		"no from":             {with(func(r *MergeContacts) { r.From = ContactRef{} }), "from.tenant"},
		"no into":             {with(func(r *MergeContacts) { r.Into = ContactRef{} }), "into.tenant"},
		"into itself":         {with(func(r *MergeContacts) { r.Into = r.From }), "into"},
		"bad identity":        {with(func(r *MergeContacts) { r.Identities = []IdentityRef{{Inbox: inboxA}} }), "identities.0.id"},
		"repeated identity":   {with(func(r *MergeContacts) { r.Identities = []IdentityRef{ident, ident} }), "identities"},
		"free text reason":    {with(func(r *MergeContacts) { r.Reason = "same person as Ada" }), "reason"},
		"no reason":           {with(func(r *MergeContacts) { r.Reason = "" }), "reason"},
		"other contact is ok": {with(func(r *MergeContacts) { r.Into = other }), ""},
		"too many identities": {with(func(r *MergeContacts) { r.Identities = manyIdentities(MaxMergeIdentities + 1) }), "identities"},
		"exactly the maximum": {with(func(r *MergeContacts) { r.Identities = manyIdentities(MaxMergeIdentities) }), ""},
	})
	if (MergeContacts{}).RevisionRule() != RevisionRequired {
		t.Fatal("merge must require the revision of the contact merged")
	}
}

func manyIdentities(n int) []IdentityRef {
	out := make([]IdentityRef, n)
	for i := range out {
		out[i] = IdentityRef{Inbox: inboxA, ID: ID(fmt.Sprintf("00000000-0000-7000-8000-%012d", i+10))}
	}
	return out
}

func TestPreviewMergeValidate(t *testing.T) {
	into := ContactRef{Tenant: tenantA, ID: idA}
	runRequestCases(t, map[string]requestCase{
		"valid":       {PreviewMerge{Call: call, From: contact, Into: into}, ""},
		"no call":     {PreviewMerge{From: contact, Into: into}, "correlation"},
		"into itself": {PreviewMerge{Call: call, From: contact, Into: contact}, "into"},
		"no into":     {PreviewMerge{Call: call, From: contact}, "into.tenant"},
	})
}
