package model_test

import (
	"errors"
	"fmt"

	"hop.top/uma/model"
)

// NormalizeEmail lowercases the domain of a bare address and keeps the local
// part byte-exact. A display name, angle brackets or surrounding space are
// refused.
func ExampleNormalizeEmail() {
	addr, err := model.NormalizeEmail("Ada.Lovelace@Example.COM")
	fmt.Println(addr, err)
	_, err = model.NormalizeEmail("Ada <ada@example.com>")
	fmt.Println(err != nil)
	// Output:
	// Ada.Lovelace@example.com <nil>
	// true
}

// An email adapter hands the raw threading headers to Normalize and reports
// the result on its envelope: the message's own ID, then the references that
// thread it, and whether the mail is automated.
func ExampleEmailHeaders_Normalize() {
	ch := model.ChannelRef{Tenant: "acme", ID: "support"}
	n := model.EmailHeaders{
		MessageID:     "<reply-2@example.com>",
		InReplyTo:     "<question-1@example.com>",
		References:    "<question-1@example.com>",
		AutoSubmitted: "auto-replied",
	}.Normalize(ch)
	fmt.Println(n.MessageID, n.Automated)
	for _, r := range n.References {
		fmt.Println(r.Kind, r.ID)
	}
	// Output:
	// <reply-2@example.com> true
	// email.message_id <reply-2@example.com>
	// email.in_reply_to <question-1@example.com>
	// email.references <question-1@example.com>
}

// Errors carry a code, a message safe to show and retry advice. A caller
// branches on the code, never on the text.
func ExampleError() {
	var err error = model.NewError(model.NotFound, "no such conversation")
	var e *model.Error
	if errors.As(err, &e) {
		fmt.Println(e.Code, e.SafeMessage)
	}
	// Output:
	// not_found no such conversation
}

// Adapter type keys are checked when an adapter registers: a maintained key
// is valid, any other key needs three or more lowercase segments.
func ExampleValidateTypeKey() {
	fmt.Println(model.ValidateTypeKey(model.TypeSmtpMail))
	fmt.Println(model.ValidateTypeKey("com.example.text") == nil)
	fmt.Println(model.ValidateTypeKey("text") != nil)
	// Output:
	// <nil>
	// true
	// true
}
