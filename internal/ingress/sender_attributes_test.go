package ingress_test

import (
	"reflect"
	"strings"
	"testing"

	"hop.top/uma/model"
)

var (
	attrPhone = model.Attribute{Kind: model.AttributePhone, Value: "+15558675310", Verified: true}
	attrWAID  = model.Attribute{Kind: model.AttributeWhatsAppUserID, Value: "US.0000000000000003", Verified: true}
)

// An envelope's sender attributes are recorded on the identity ingress
// claims, sorted by kind then value, and on no contact: a contact's
// attributes are the desk's to write.
func TestSenderAttributesRecordedAtClaim(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(false), dest)
	env := h.Inbound("e1", "m1", "u1", dest)
	env.SenderAttributes = []model.Attribute{attrWAID, attrPhone}
	h.MustAccept(env)
	id, c := identityOf(h, in.Ref, "u1")
	if want := []model.Attribute{attrPhone, attrWAID}; !reflect.DeepEqual(id.Attributes, want) {
		t.Fatalf("identity attributes %+v, want %+v", id.Attributes, want)
	}
	if len(c.Attributes) != 0 {
		t.Fatalf("contact attributes %+v, want none", c.Attributes)
	}
}

// The attributes recorded at the first claim win: a later envelope with
// other attributes for the same sender changes nothing and fails nothing.
func TestStoredSenderAttributesWin(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(false), dest)
	first := h.Inbound("e1", "m1", "u1", dest)
	first.SenderAttributes = []model.Attribute{attrPhone}
	h.MustAccept(first)
	before, _ := identityOf(h, in.Ref, "u1")
	later := h.Inbound("e2", "m2", "u1", dest)
	later.SenderAttributes = []model.Attribute{attrWAID}
	h.MustAccept(later)
	after, _ := identityOf(h, in.Ref, "u1")
	if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(after.Attributes, []model.Attribute{attrPhone}) {
		t.Fatalf("identity %+v changed to %+v", before, after)
	}
}

// A sender with no attributes claims an identity with none, as before.
func TestSenderWithoutAttributes(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(false), dest)
	h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	if id, _ := identityOf(h, in.Ref, "u1"); id.Attributes != nil {
		t.Fatalf("identity attributes %+v, want none", id.Attributes)
	}
}

// A message update claims its sender with the attributes, as a new
// message does.
func TestSenderAttributesOnUpdateClaim(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(false), dest)
	h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	react := h.Inbound("e2", "m1", "reactor", dest)
	react.Kind, react.SenderAttributes = model.EnvelopeReactionAdded, []model.Attribute{attrPhone}
	react.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "+1"}}}
	h.MustAccept(react)
	if id, _ := identityOf(h, in.Ref, "reactor"); !reflect.DeepEqual(id.Attributes, []model.Attribute{attrPhone}) {
		t.Fatalf("reacting identity attributes %+v", id.Attributes)
	}
}

// Ingress refuses invalid, unnormalized or repeated attributes, too many of
// them, and attributes without a sender or on an envelope kind that claims
// none, before storing anything.
func TestSenderAttributesRejected(t *testing.T) {
	many := make([]model.Attribute, 0, model.MaxContactAttributes+1)
	for i := 0; i <= model.MaxContactAttributes; i++ {
		many = append(many, model.Attribute{Kind: model.AttributeWhatsAppUserID, Value: "US." + strings.Repeat("a", i+1)})
	}
	set := func(a ...model.Attribute) func(*harness, *model.Envelope) {
		return func(_ *harness, e *model.Envelope) { e.SenderAttributes = a }
	}
	cases := []struct {
		name string
		edit func(*harness, *model.Envelope)
		want string
	}{
		{"unknown kind", set(model.Attribute{Kind: "fax", Value: "1"}), "envelope.sender_attributes[0]"},
		{"unnormalized phone", set(model.Attribute{Kind: model.AttributePhone, Value: "+1 555 867 5310"}), "envelope.sender_attributes[0]"},
		{"prefixed user id", set(model.Attribute{Kind: model.AttributeWhatsAppUserID, Value: "whatsapp:US.0000000000000003"}), "envelope.sender_attributes[0]"},
		{"duplicate", set(attrPhone, attrPhone), "envelope.sender_attributes[1]"},
		{"too many", set(many...), "envelope.sender_attributes: exceeds"},
		{"system actor", func(_ *harness, e *model.Envelope) {
			e.Sender, e.SystemActor, e.SenderAttributes = model.ExternalRef{}, true, []model.Attribute{attrPhone}
		}, "envelope.sender_attributes: requires a sender"},
		{"status", func(_ *harness, e *model.Envelope) {
			e.Kind, e.Content, e.SenderAttributes = model.EnvelopeStatus, model.Content{}, []model.Attribute{attrPhone}
		}, "envelope.sender_attributes: the envelope kind claims no sender"},
		{"work item", func(h *harness, e *model.Envelope) {
			e.Kind, e.Content, e.SenderAttributes = model.EnvelopeWorkItem, model.Content{}, []model.Attribute{attrPhone}
			e.References = []model.ExternalRef{h.Ref(model.RefKindWorkItem, "1", "")}
			e.WorkItem = &model.WorkItemState{Subject: "s", Status: model.WorkItemOpen, Binding: model.WorkItemBindingActive}
		}, "envelope.sender_attributes: the envelope kind claims no sender"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, chatType)
			h.Configure(perContact(false), dest)
			env := h.Inbound("e1", "m1", "u1", dest)
			tc.edit(h, &env)
			before := len(h.Events())
			_, err := h.Accept(env)
			requireCode(t, err, model.Validation, tc.want)
			if len(h.Events()) != before || len(h.Contacts()) != 0 {
				t.Fatal("rejected envelope stored events or contacts")
			}
		})
	}
}
