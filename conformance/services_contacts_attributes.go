package conformance

import (
	"fmt"
	"reflect"
	"testing"

	"hop.top/uma/model"
)

// Attributes the sender-attribute cases use: one person, WhatsApp-style,
// first with a visible number and a user ID, then hidden behind the ID,
// then on a new number with a new ID.
var (
	attrNumber1 = model.Attribute{Kind: model.AttributePhone, Value: "+15558675310", Verified: true}
	attrNumber2 = model.Attribute{Kind: model.AttributePhone, Value: "+15558675311", Verified: true}
	attrUser1   = model.Attribute{Kind: model.AttributeWhatsAppUserID, Value: "US.0000000000000001", Verified: true}
	attrUser2   = model.Attribute{Kind: model.AttributeWhatsAppUserID, Value: "US.0000000000000002", Verified: true}
)

// arriveWithAttributes is arrive with the sender's attributes on the
// envelope.
func (env *contactsEnv) arriveWithAttributes(in model.InboxRef, sender string, attrs ...model.Attribute) model.Identity {
	env.t.Helper()
	e := env.envelope(in, sender)
	e.SenderAttributes = attrs
	if err := env.accept(e); err != nil {
		env.t.Fatalf("Accept(%s): %v", e.EventID, err)
	}
	return env.byKey(in, sender)
}

// attributeAnswer is the reference verified_attribute verifier: it reads
// the identity and its anchor from the store and returns evidence naming
// a verified attribute both carry, or fails. A host verifier is free to
// be stricter; core only checks the binding it returns.
func (env *contactsEnv) attributeAnswer(anchor model.IdentityRef) func(model.Principal, model.LinkRequest) (model.Evidence, error) {
	return func(_ model.Principal, req model.LinkRequest) (model.Evidence, error) {
		own, other := env.identity(req.Identity), env.identity(anchor)
		for _, a := range own.Attributes {
			for _, b := range other.Attributes {
				if a.Verified && b.Verified && a.Kind == b.Kind && a.Value == b.Value {
					bind := Binding(req)
					bind.Anchor = &anchor
					facts, err := model.NewEvidenceFacts(model.VerifiedAttributeFacts{EvidenceBinding: bind, Attribute: string(a.Kind), Value: a.Value})
					return model.Evidence{Verifier: model.EvidenceKindVerifiedAttribute, Reference: req.Evidence.Reference, Facts: facts, Assurance: model.LinkAssuranceAttributeMatch}, err
				}
			}
		}
		return model.Evidence{}, ErrNotVerified
	}
}

// attributeLink links id to the contact of anchor with verified_attribute
// evidence from the reference verifier.
func (env *contactsEnv) attributeLink(id, anchor model.Identity, ref string) error {
	env.attribute.Script(env.attributeAnswer(anchor.Ref))
	req := env.linkReq(id, anchor.Contact, ref)
	req.Evidence.Verifier = model.EvidenceKindVerifiedAttribute
	_, err := env.link(req)
	return err
}

// contactsIngressSenderAttributes: the attributes an adapter reports on an
// envelope are stored, sorted, on the identity ingress claims and on no
// contact, read back as stored; the first claim's attributes win. They are
// the inputs of the existing verified_attribute link: identities sharing a
// verified attribute join one contact (a hidden-number identity through
// the user ID, a number-keyed one through the phone), identities sharing
// none do not, as a user who changed number and was issued a new user ID.
func contactsIngressSenderAttributes(t *testing.T, env *contactsEnv) {
	const number1, number2, hidden1, hidden2 = "+15558675310", "+15558675311", "US.0000000000000001", "US.0000000000000002"

	// Stored sorted by kind then value, on the identity only.
	num1 := env.arriveWithAttributes(env.one, number1, attrUser1, attrNumber1)
	if want := []model.Attribute{attrNumber1, attrUser1}; !reflect.DeepEqual(num1.Attributes, want) {
		t.Fatalf("identity attributes %+v, want %+v", num1.Attributes, want)
	}
	if c, err := env.contact(num1.Contact); err != nil || len(c.Attributes) != 0 {
		t.Fatalf("provisional contact %+v, %v; want no attributes", c, err)
	}

	// The first claim's attributes win, and a sender without any claims none.
	if again := env.arriveWithAttributes(env.one, number1, attrNumber2); !reflect.DeepEqual(again, num1) {
		t.Fatalf("identity %+v after a later report, want %+v", again, num1)
	}
	if bare := env.arriveWithAttributes(env.one, "bare"); bare.Attributes != nil {
		t.Fatalf("identity attributes %+v, want none", bare.Attributes)
	}

	// The number hidden: the user ID keys a new identity, which shares the
	// user ID with the number-keyed one and joins its contact.
	hid1 := env.arriveWithAttributes(env.two, hidden1, attrUser1)
	if hid1.Contact == num1.Contact {
		t.Fatal("ingress linked two identities")
	}
	if err := env.attributeLink(hid1, num1, "va-user-id"); err != nil {
		t.Fatalf("Link on the user ID: %v", err)
	}
	hid1 = env.identity(hid1.Ref)
	rec := env.records(hid1.Ref)[0]
	if hid1.Contact != num1.Contact || hid1.LinkAssurance != model.LinkAssuranceAttributeMatch || rec.Anchor == nil || *rec.Anchor != num1.Ref {
		t.Fatalf("hidden-number identity %+v, record %+v; want linked by attribute_match to %v", hid1, rec, num1.Ref)
	}

	// A new number and a new user ID share nothing with the old pair: no
	// evidence, no link, and the old contact is untouched.
	changed := env.arriveWithAttributes(env.one, number2, attrNumber2, attrUser2)
	if err := env.attributeLink(changed, num1, "va-changed"); err == nil {
		t.Fatal("identities sharing no attribute linked")
	}
	if again := env.identity(changed.Ref); again.Contact == num1.Contact || len(env.records(changed.Ref)) != 0 {
		t.Fatalf("identity %+v after a refused link, want its own contact", again)
	}

	// The new user ID, seen alone later, joins the new number's identity.
	hid2 := env.arriveWithAttributes(env.two, hidden2, attrUser2)
	if err := env.attributeLink(hid2, changed, "va-user-id-2"); err != nil {
		t.Fatalf("Link on the new user ID: %v", err)
	}
	if got := env.identity(hid2.Ref); got.Contact != changed.Contact {
		t.Fatalf("identity %+v, want contact %v", got, changed.Contact)
	}

	// A shared phone alone is enough, whatever the keys: the same number
	// seen without a user ID.
	same := env.arriveWithAttributes(env.two, fmt.Sprintf("sms-%s", number1), attrNumber1)
	if err := env.attributeLink(same, num1, "va-phone"); err != nil {
		t.Fatalf("Link on the phone: %v", err)
	}
	if got := env.identity(same.Ref); got.Contact != num1.Contact {
		t.Fatalf("identity %+v, want contact %v", got, num1.Contact)
	}
}
