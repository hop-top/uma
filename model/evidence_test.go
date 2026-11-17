package model

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestMaxLinkAssuranceByKind(t *testing.T) {
	want := map[string]LinkAssurance{
		EvidenceKindAttestation:       LinkAssuranceAsserted,
		EvidenceKindImport:            LinkAssuranceImported,
		EvidenceKindVerifiedAttribute: LinkAssuranceAttributeMatch,
		EvidenceKindHostSession:       LinkAssuranceHostAuthenticated,
		EvidenceKindChallenge:         LinkAssuranceProven,
	}
	kinds := EvidenceKinds()
	if len(kinds) != len(want) {
		t.Fatalf("EvidenceKinds = %v, want the five core kinds", kinds)
	}
	for _, k := range kinds {
		got, ok := MaxLinkAssurance(k)
		if !ok || got != want[k] {
			t.Errorf("MaxLinkAssurance(%q) = %q, %v; want %q", k, got, ok, want[k])
		}
		if !IsEvidenceKind(k) {
			t.Errorf("%q not an evidence kind", k)
		}
		if s, ok := EvidenceSchema(k); !ok || s != CoreNamespace+".evidence."+k {
			t.Errorf("EvidenceSchema(%q) = %q, %v", k, s, ok)
		}
	}
	for _, k := range []string{"", "Attestation", "oauth", "uma.evidence.attestation"} {
		if a, ok := MaxLinkAssurance(k); ok || a != "" {
			t.Errorf("MaxLinkAssurance(%q) = %q, %v; want no cap", k, a, ok)
		}
		if IsEvidenceKind(k) {
			t.Errorf("%q reported as an evidence kind", k)
		}
		if _, ok := EvidenceSchema(k); ok {
			t.Errorf("EvidenceSchema(%q) reported a schema", k)
		}
	}
	kinds[0] = "mutated"
	if EvidenceKinds()[0] != EvidenceKindAttestation {
		t.Fatal("EvidenceKinds result aliases package state")
	}
}

var (
	evTarget  = ContactRef{Tenant: tenantA, ID: idA}
	evAnchor  = IdentityRef{Inbox: inboxB, ID: idB}
	evNow     = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	evExpires = evNow.Add(time.Minute)
)

const evActor ActorID = "agent-7"

func evRequest(intent LinkClass, kind string) LinkRequest {
	r := LinkRequest{
		Intent: intent, Identity: ident, Current: contact, ExpectedRevision: "r1",
		Evidence: Evidence{Verifier: kind, Reference: "ref-1"},
	}
	if intent == LinkClassUnlink {
		r.Reason = "wrong_person"
	} else {
		target := evTarget
		r.Target = &target
	}
	return r
}

// evFacts returns facts of kind bound to req.
func evFacts(req LinkRequest, kind string) EvidenceFacts {
	b := EvidenceBinding{Intent: req.Intent, Identity: req.Identity, Current: req.Current}
	if req.Target != nil {
		target := *req.Target
		b.Target = &target
	}
	anchored := kind == EvidenceKindVerifiedAttribute || kind == EvidenceKindHostSession || kind == EvidenceKindChallenge
	if anchored && req.Intent != LinkClassUnlink {
		anchor := evAnchor
		b.Anchor = &anchor
	}
	switch kind {
	case EvidenceKindAttestation:
		code := "verified_by_phone"
		if req.Intent == LinkClassUnlink {
			code = req.Reason
		}
		return AttestationFacts{EvidenceBinding: b, Actor: evActor, ReasonCode: code}
	case EvidenceKindImport:
		key := IdentityKey{Inbox: req.Identity.Inbox, Namespace: "smtp.address", SourceID: "jane@example.org"}
		return ImportFacts{EvidenceBinding: b, SourceSystem: "crm", SourceRecord: "c-42", IdentityKey: key}
	case EvidenceKindVerifiedAttribute:
		return VerifiedAttributeFacts{EvidenceBinding: b, Attribute: "email", Value: "jane@example.org"}
	case EvidenceKindHostSession:
		return HostSessionFacts{EvidenceBinding: b, SessionSubject: "sub-9"}
	case EvidenceKindChallenge:
		expires := evExpires
		b.ExpiresAt = &expires
		return ChallengeFacts{EvidenceBinding: b, ChallengeRef: "chal-1"}
	}
	panic("unknown kind " + kind)
}

func evReturned(t *testing.T, req LinkRequest, kind string, a LinkAssurance, facts EvidenceFacts) Evidence {
	t.Helper()
	x, err := NewEvidenceFacts(facts)
	requireOK(t, err)
	return Evidence{Verifier: kind, Reference: req.Evidence.Reference, Facts: x, Assurance: a}
}

func TestEvidenceFactsRoundTrip(t *testing.T) {
	for _, kind := range EvidenceKinds() {
		for _, intent := range []LinkClass{LinkClassLink, LinkClassUnlink} {
			req := evRequest(intent, kind)
			facts := evFacts(req, kind)
			if facts.EvidenceKind() != kind {
				t.Fatalf("%T kind = %q, want %q", facts, facts.EvidenceKind(), kind)
			}
			ev := evReturned(t, req, kind, LinkAssuranceNone, facts)
			if want, _ := EvidenceSchema(kind); ev.Facts.Name != want || ev.Facts.Version != EvidenceFactsVersion1 {
				t.Fatalf("%s facts = %s v%d", kind, ev.Facts.Name, ev.Facts.Version)
			}
			got, err := ev.DecodeFacts()
			requireOK(t, err)
			if !reflect.DeepEqual(got, facts) {
				t.Fatalf("%s decoded %#v, want %#v", kind, got, facts)
			}
			if !reflect.DeepEqual(got.Binding(), facts.Binding()) {
				t.Fatalf("%s binding differs", kind)
			}
		}
	}
	_, err := NewEvidenceFacts(nil)
	requireError(t, err, Validation, "facts")
}

// Every core kind accepts verifier output bound to the request, for every
// intent, at any declared assurance up to its cap; link and relink need an
// assurance above none.
func TestEvidenceReturnAccepts(t *testing.T) {
	for _, kind := range EvidenceKinds() {
		maxA, _ := MaxLinkAssurance(kind)
		for _, intent := range []LinkClass{LinkClassLink, LinkClassRelink, LinkClassUnlink} {
			req := evRequest(intent, kind)
			for _, a := range linkAssuranceOrder {
				ev := evReturned(t, req, kind, a, evFacts(req, kind))
				err := ev.ValidateReturn(req, evActor, evNow)
				switch {
				case a.Below(LinkAssuranceAsserted) && intent != LinkClassUnlink:
					requireError(t, err, Validation, "evidence.assurance")
				case maxA.Below(a):
					requireError(t, err, Validation, "evidence.assurance")
				default:
					if err != nil {
						t.Fatalf("%s %s %s: %v", kind, intent, a, err)
					}
				}
			}
		}
	}
}

func TestEvidenceReturnValidation(t *testing.T) {
	type tc struct {
		kind   string
		intent LinkClass
		edit   func(*Evidence, *EvidenceFacts)
		field  string
	}
	set := func(f func(*EvidenceBinding)) func(*Evidence, *EvidenceFacts) {
		return func(_ *Evidence, facts *EvidenceFacts) { *facts = rebind(*facts, f) }
	}
	other := ContactRef{Tenant: tenantA, ID: idC}
	otherIdent := IdentityRef{Inbox: inboxA, ID: idD}
	past := evNow.Add(-time.Second)
	cases := map[string]tc{
		"key differs from request": {EvidenceKindAttestation, LinkClassLink, func(e *Evidence, _ *EvidenceFacts) {
			e.Verifier = EvidenceKindChallenge
		}, "evidence.verifier"},
		"facts of another kind": {EvidenceKindChallenge, LinkClassLink, func(e *Evidence, f *EvidenceFacts) {
			*f = evFacts(evRequest(LinkClassLink, EvidenceKindAttestation), EvidenceKindAttestation)
		}, "evidence.facts.name"},
		"wrong facts version": {EvidenceKindImport, LinkClassLink, func(e *Evidence, _ *EvidenceFacts) {
			e.Facts.Version = 2
		}, "evidence.facts.version"},
		"facts missing": {EvidenceKindImport, LinkClassLink, func(e *Evidence, _ *EvidenceFacts) {
			e.Facts = Extension{}
		}, "evidence.facts.name"},
		"facts not an object": {EvidenceKindImport, LinkClassLink, func(e *Evidence, _ *EvidenceFacts) {
			e.Facts.Data = json.RawMessage(`[1]`)
		}, "evidence.facts.data"},
		"facts unknown field": {EvidenceKindImport, LinkClassLink, func(e *Evidence, _ *EvidenceFacts) {
			e.Facts.Data = json.RawMessage(`{"intent":"link","extra":1}`)
		}, "evidence.facts.data"},
		"facts trailing data": {EvidenceKindImport, LinkClassLink, func(e *Evidence, _ *EvidenceFacts) {
			e.Facts.Data = append(e.Facts.Data, []byte(` {}`)...)
		}, "evidence.facts.data"},
		"intent mismatch": {EvidenceKindHostSession, LinkClassRelink, set(func(b *EvidenceBinding) {
			b.Intent = LinkClassLink
		}), "evidence.facts.intent"},
		"unbound identity": {EvidenceKindAttestation, LinkClassLink, set(func(b *EvidenceBinding) {
			b.Identity = otherIdent
		}), "evidence.facts.identity"},
		"unbound current": {EvidenceKindAttestation, LinkClassUnlink, set(func(b *EvidenceBinding) {
			b.Current = other
		}), "evidence.facts.current"},
		"unbound target": {EvidenceKindImport, LinkClassLink, set(func(b *EvidenceBinding) {
			b.Target = &other
		}), "evidence.facts.target"},
		"target missing": {EvidenceKindImport, LinkClassRelink, set(func(b *EvidenceBinding) {
			b.Target = nil
		}), "evidence.facts.target"},
		"target on unlink": {EvidenceKindAttestation, LinkClassUnlink, set(func(b *EvidenceBinding) {
			b.Target = &other
		}), "evidence.facts.target"},
		"anchor missing": {EvidenceKindVerifiedAttribute, LinkClassLink, set(func(b *EvidenceBinding) {
			b.Anchor = nil
		}), "evidence.facts.anchor"},
		"anchor is the identity": {EvidenceKindHostSession, LinkClassLink, set(func(b *EvidenceBinding) {
			b.Anchor = &ident
		}), "evidence.facts.anchor"},
		"anchor malformed": {EvidenceKindChallenge, LinkClassLink, set(func(b *EvidenceBinding) {
			b.Anchor = &IdentityRef{Inbox: inboxA}
		}), "evidence.facts.anchor.id"},
		"anchor in another tenant": {EvidenceKindChallenge, LinkClassLink, set(func(b *EvidenceBinding) {
			b.Anchor = &IdentityRef{Inbox: InboxRef{Tenant: tenantB, ID: idA}, ID: idB}
		}), "evidence.facts.anchor"},
		"anchor on unanchored kind": {EvidenceKindAttestation, LinkClassLink, set(func(b *EvidenceBinding) {
			b.Anchor = &evAnchor
		}), "evidence.facts.anchor"},
		"anchor on unlink": {EvidenceKindChallenge, LinkClassUnlink, set(func(b *EvidenceBinding) {
			b.Anchor = &evAnchor
		}), "evidence.facts.anchor"},
		"expired": {EvidenceKindAttestation, LinkClassLink, set(func(b *EvidenceBinding) {
			b.ExpiresAt = &past
		}), "evidence.facts.expires_at"},
		"expires now": {EvidenceKindChallenge, LinkClassLink, set(func(b *EvidenceBinding) {
			now := evNow
			b.ExpiresAt = &now
		}), "evidence.facts.expires_at"},
		"challenge without expiry": {EvidenceKindChallenge, LinkClassLink, set(func(b *EvidenceBinding) {
			b.ExpiresAt = nil
		}), "evidence.facts.expires_at"},
		"challenge without reference": {EvidenceKindChallenge, LinkClassLink, func(e *Evidence, _ *EvidenceFacts) {
			e.Reference = ""
		}, "evidence.reference"},
		"malformed reference": {EvidenceKindImport, LinkClassLink, func(e *Evidence, _ *EvidenceFacts) {
			e.Reference = " ref"
		}, "evidence.reference"},
		"no assurance": {EvidenceKindAttestation, LinkClassUnlink, func(e *Evidence, _ *EvidenceFacts) {
			e.Assurance = ""
		}, "evidence.assurance"},
		"unknown assurance": {EvidenceKindAttestation, LinkClassLink, func(e *Evidence, _ *EvidenceFacts) {
			e.Assurance = "certain"
		}, "evidence.assurance"},
		"assurance above cap": {EvidenceKindAttestation, LinkClassUnlink, func(e *Evidence, _ *EvidenceFacts) {
			e.Assurance = LinkAssuranceImported
		}, "evidence.assurance"},
		"attestation by another actor": {EvidenceKindAttestation, LinkClassLink, func(_ *Evidence, f *EvidenceFacts) {
			a := (*f).(AttestationFacts)
			a.Actor = "someone-else"
			*f = a
		}, "evidence.facts.actor"},
		"attestation free text reason": {EvidenceKindAttestation, LinkClassLink, func(_ *Evidence, f *EvidenceFacts) {
			a := (*f).(AttestationFacts)
			a.ReasonCode = "they said so"
			*f = a
		}, "evidence.facts.reason_code"},
		"attestation reason not the unlink reason": {EvidenceKindAttestation, LinkClassUnlink, func(_ *Evidence, f *EvidenceFacts) {
			a := (*f).(AttestationFacts)
			a.ReasonCode = "duplicate"
			*f = a
		}, "evidence.facts.reason_code"},
		"import without source system": {EvidenceKindImport, LinkClassLink, func(_ *Evidence, f *EvidenceFacts) {
			i := (*f).(ImportFacts)
			i.SourceSystem = ""
			*f = i
		}, "evidence.facts.source_system"},
		"import without source record": {EvidenceKindImport, LinkClassLink, func(_ *Evidence, f *EvidenceFacts) {
			i := (*f).(ImportFacts)
			i.SourceRecord = ""
			*f = i
		}, "evidence.facts.source_record"},
		"import key in another inbox": {EvidenceKindImport, LinkClassLink, func(_ *Evidence, f *EvidenceFacts) {
			i := (*f).(ImportFacts)
			i.IdentityKey.Inbox = inboxB
			*f = i
		}, "evidence.facts.identity_key.inbox"},
		"import key malformed": {EvidenceKindImport, LinkClassLink, func(_ *Evidence, f *EvidenceFacts) {
			i := (*f).(ImportFacts)
			i.IdentityKey.SourceID = ""
			*f = i
		}, "evidence.facts.identity_key.source_id"},
		"attribute without value": {EvidenceKindVerifiedAttribute, LinkClassLink, func(_ *Evidence, f *EvidenceFacts) {
			v := (*f).(VerifiedAttributeFacts)
			v.Value = ""
			*f = v
		}, "evidence.facts.value"},
		"attribute without type": {EvidenceKindVerifiedAttribute, LinkClassUnlink, func(_ *Evidence, f *EvidenceFacts) {
			v := (*f).(VerifiedAttributeFacts)
			v.Attribute = ""
			*f = v
		}, "evidence.facts.attribute"},
		"session without subject": {EvidenceKindHostSession, LinkClassLink, func(_ *Evidence, f *EvidenceFacts) {
			h := (*f).(HostSessionFacts)
			h.SessionSubject = ""
			*f = h
		}, "evidence.facts.session_subject"},
		"challenge without ref": {EvidenceKindChallenge, LinkClassLink, func(_ *Evidence, f *EvidenceFacts) {
			c := (*f).(ChallengeFacts)
			c.ChallengeRef = ""
			*f = c
		}, "evidence.facts.challenge_ref"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			req := evRequest(c.intent, c.kind)
			facts := evFacts(req, c.kind)
			maxA, _ := MaxLinkAssurance(c.kind)
			ev := evReturned(t, req, c.kind, maxA, facts)
			before := ev
			c.edit(&ev, &facts)
			if !reflect.DeepEqual(facts, evFacts(req, c.kind)) {
				x, err := NewEvidenceFacts(facts)
				requireOK(t, err)
				ev.Facts.Name, ev.Facts.Data = x.Name, x.Data
			}
			if reflect.DeepEqual(ev, before) {
				t.Fatal("edit changed nothing")
			}
			requireError(t, ev.ValidateReturn(req, evActor, evNow), Validation, c.field)
		})
	}
}

// An unknown request key has no defined cap: the return is refused even
// when it echoes the key.
func TestEvidenceReturnRejectsUnknownKind(t *testing.T) {
	req := evRequest(LinkClassLink, "oauth")
	ev := Evidence{Verifier: "oauth", Facts: Extension{Name: "uma.evidence.oauth", Version: 1, Data: json.RawMessage(`{}`)},
		Assurance: LinkAssuranceAsserted}
	requireError(t, ev.ValidateReturn(req, evActor, evNow), Validation, "evidence.verifier")
	_, err := ev.DecodeFacts()
	requireError(t, err, Validation, "verifier")
}

// rebind applies f to the binding of facts and returns the edited facts.
func rebind(facts EvidenceFacts, f func(*EvidenceBinding)) EvidenceFacts {
	switch v := facts.(type) {
	case AttestationFacts:
		f(&v.EvidenceBinding)
		return v
	case ImportFacts:
		f(&v.EvidenceBinding)
		return v
	case VerifiedAttributeFacts:
		f(&v.EvidenceBinding)
		return v
	case HostSessionFacts:
		f(&v.EvidenceBinding)
		return v
	case ChallengeFacts:
		f(&v.EvidenceBinding)
		return v
	}
	panic("unknown facts")
}
