package google

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/model"
)

const (
	// ExtensionForward forwards one message to another address, carrying
	// the original whole: the message is attached, byte for byte, as a
	// message/rfc822 part (RFC 2046 section 5.2.1) of a new message, so its
	// headers, body and attachments all survive. Its data is
	// {"to": "<address>", "text": "<note>"}: "to" is required, one bare
	// address; "text" is an optional note written above the attachment.
	//
	// The forward is composed as follows:
	//
	//   - From the mailbox, To the address, Subject "Fwd: " and the
	//     original's subject ("Fwd: " is not repeated);
	//   - the Message-ID is uma's, derived from the operation as a send's
	//     is, and Reconcile searches Sent for it: the forwarded copy
	//     arrives under that Message-ID;
	//   - References names the original's Message-ID, and nothing else:
	//     there is no In-Reply-To and no threadId, so a forward is no reply
	//     and starts its own Gmail thread. A receiver that stores threading
	//     headers (uma's ingress does) can find the copy by the original's
	//     Message-ID. The attached part keeps the original's own headers.
	//
	// The original is the effect's Target when it names a provider message
	// of the channel, else the route's ParentID, read from Gmail
	// (users.messages.get, format=raw) at the attempt, bounded like an
	// inbound read: a message over the bound is refused. A message Gmail no
	// longer has is rejected not_found. The send is the adapter's own, with
	// its outcomes: provider_accepted, outcome_unknown when the answer is
	// lost, never retried before Reconcile. The effect is authorized as
	// model.ActionExtensionExecute. It needs ScopeReadonly and ScopeSend.
	ExtensionForward = string(Type) + ".forward"

	// maxForwardNoteBytes bounds the note.
	maxForwardNoteBytes = 16 << 10
)

// forwardDefinition is the registration of ExtensionForward. Shipping it
// is the adapter's declaration that it can forward a message as an
// attached original.
func forwardDefinition() adapter.ExtensionDefinition {
	return adapter.ExtensionDefinition{
		Name: ExtensionForward, Version: ExtensionVersion, Kind: model.ExtensionKindEffect,
		InputSchema:        model.Extension{Name: ExtensionForward + ".input", Version: ExtensionVersion},
		RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: model.ActionExtensionExecute,
	}
}

// forwardAction are the fields of an ExtensionForward action.
type forwardAction struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

// checkForward is the one set of field rules of the forward effect. It
// returns the action's fields when they are valid, otherwise the first
// issue, whose message is safe to show.
func checkForward(action model.Extension) (forwardAction, *model.ValidationIssue) {
	refuse := func(field, msg string) (forwardAction, *model.ValidationIssue) {
		return forwardAction{}, &model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(action.Data), &fields); err != nil || fields == nil {
		return refuse("data", "action data must be a JSON object")
	}
	var a forwardAction
	for _, f := range []struct {
		name string
		dst  *string
	}{{"to", &a.To}, {"text", &a.Text}} {
		raw, set := fields[f.name]
		if !set || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		if err := json.Unmarshal(raw, f.dst); err != nil {
			return refuse("data."+f.name, f.name+" must be a string")
		}
	}
	switch {
	case a.To == "":
		return refuse("data.to", "a forward needs the address to send to")
	case !bareAddress(a.To):
		return refuse("data.to", "to must be one bare address")
	case len(a.Text) > maxForwardNoteBytes:
		return refuse("data.text", "the note is too long")
	}
	return a, nil
}

// forwardEffect runs ExtensionForward. Refusals are final: a rejected
// outcome with a validation failure, before anything is sent.
func (i *instance) forwardEffect(ctx context.Context, req adapter.ExecuteRequest) (adapter.Outcome, error) {
	if req.Action.Version != ExtensionVersion {
		return rejected(model.SchemaMismatch, "unsupported action version"), nil
	}
	a, issue := checkForward(req.Action)
	if issue != nil {
		return rejected(model.Validation, issue.Message), nil
	}
	id, ok := i.targetMessage(req.Effect)
	if !ok {
		return rejected(model.Validation, "no provider message to forward: the effect names none and the route has no parent message"), nil
	}
	s := i.c.session()
	orig, err := i.rawMessage(ctx, s, id)
	switch {
	case model.IsCode(err, model.NotFound):
		return rejected(model.NotFound, "the message is gone"), nil
	case tooLarge(err):
		return rejected(model.Validation, "the message is too large to forward"), nil
	case err != nil:
		return rejectedOr(err)
	}
	from, err := i.mailboxAddress(ctx, s)
	if err != nil {
		return rejectedOr(err)
	}
	h, err := mime.ParseHeader(bytes.NewReader(orig), 0)
	if err != nil {
		return rejected(model.Validation, "the message cannot be read as mail"), nil
	}
	out := mime.Outgoing{
		From:      mime.Address{Address: from},
		To:        []mime.Address{{Address: a.To}},
		Subject:   forwardSubject(h.Subject),
		Date:      i.now(),
		MessageID: i.messageID(req.Operation, from),
		Text:      forwardText(a.Text),
		Embedded:  &mime.Embedded{Name: forwardName(h.Subject), Raw: toCRLF(orig)},
	}
	if ids := model.ParseMsgIDs(h.MessageID); len(ids) > 0 {
		out.References = ids[:1]
	}
	var raw bytes.Buffer
	if err := mime.Compose(&raw, out); err != nil {
		return rejected(model.Validation, "the forward cannot be composed"), nil
	}
	return i.submit(ctx, s, gSend{Raw: base64.RawURLEncoding.EncodeToString(raw.Bytes())}, out.MessageID)
}

// forwardSubject is the subject of a forward of subject: "Fwd: " once.
func forwardSubject(subject string) string {
	subject = strings.Join(strings.Fields(subject), " ")
	for _, p := range []string{"fwd:", "fw:"} {
		if len(subject) >= len(p) && strings.EqualFold(subject[:len(p)], p) {
			return subject
		}
	}
	return strings.TrimSpace("Fwd: " + subject)
}

// forwardText is the text part above the attachment.
func forwardText(note string) string {
	if strings.TrimSpace(note) == "" {
		return "Forwarded message attached."
	}
	return note
}

// forwardName is the attachment's file name: the original's subject.
func forwardName(subject string) string {
	name := mime.SanitizeFilename(strings.Join(strings.Fields(subject), " "))
	if name == "" {
		name = "message"
	}
	return name + ".eml"
}

// toCRLF returns b with every line ending CRLF, as a message/rfc822 part is
// carried.
func toCRLF(b []byte) []byte {
	if !bytes.Contains(b, []byte("\n")) {
		return b
	}
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
}

var _ = errors.Is
