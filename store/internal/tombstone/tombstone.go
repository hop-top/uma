// Package tombstone holds the record rewrites store.Tx.Redact and
// store.Tx.RedactLinks perform, so every maintained backend applies the
// same rules.
package tombstone

import (
	"slices"
	"strings"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Message returns m as Redact stores it under rev: content and every
// metadata entry outside model.CoreNamespace cleared, Redacted set, and
// references, attribution, audience and times kept.
func Message(m model.Message, rev model.Revision) model.Message {
	m = m.Clone()
	m.Content = model.Content{}
	m.Metadata = slices.DeleteFunc(m.Metadata, func(x model.Extension) bool {
		return !strings.HasPrefix(x.Name, model.CoreNamespace+".")
	})
	if len(m.Metadata) == 0 {
		m.Metadata = nil
	}
	m.Redacted = true
	m.Revision = rev
	return m
}

// Attachment returns r as Redact stores it under rev and the blob key it
// no longer binds, if any: state store.AttachmentRedacted, metadata other
// than the audience cleared, blob and provider locators dropped, and the
// staging operation and stager kept.
func Attachment(r store.AttachmentRecord, rev model.Revision) (store.AttachmentRecord, *ports.BlobKey) {
	blob := r.Blob
	r.Attachment.Metadata = model.AttachmentMetadata{Audience: r.Attachment.Metadata.Audience}
	r.Attachment.Revision = rev
	r.Blob, r.Provider = nil, nil
	r.State = store.AttachmentRedacted
	return r, blob
}

// Interaction returns r as Redact stores it under rev: field data,
// arguments and the response token cleared, Redacted set, and references,
// actor, kind, identifiers, response state and the token expiry kept.
func Interaction(r store.InteractionRecord, rev model.Revision) store.InteractionRecord {
	r = r.Clone()
	r.Interaction.Fields.Data = nil
	r.Interaction.Arguments = ""
	r.Interaction.Redacted = true
	r.Interaction.Revision = rev
	r.ResponseToken = ""
	return r
}

// Ingress returns r with its envelope content, raw payload data and
// interaction field data and arguments cleared; identity, deduplication,
// receipt and routing are kept.
func Ingress(r store.IngressRecord) store.IngressRecord {
	r = r.Clone()
	env := &r.Input.Envelope
	env.Content = model.Content{}
	env.Payload.Data = nil
	if env.Interaction != nil {
		env.Interaction.Fields.Data = nil
		env.Interaction.Arguments = ""
	}
	return r
}

// Operation returns r with its intent data cleared; key, fingerprint and
// state are kept.
func Operation(r store.OperationRecord) store.OperationRecord {
	r = r.Clone()
	r.Command.Data = nil
	return r
}

// Link returns rec as RedactLinks stores it: evidence facts cleared and
// Redacted set, every other field, the evidence verifier and reference
// included, kept. It reports false, and returns rec unchanged, when rec is
// a tombstone already.
func Link(rec store.LinkRecord) (store.LinkRecord, bool) {
	if rec.Redacted {
		return rec, false
	}
	rec = rec.Clone()
	rec.Evidence.Facts = model.Extension{}
	rec.Redacted = true
	return rec, true
}

// ProviderMessageIDs returns the provider message IDs among m's references,
// with the channel each belongs to.
func ProviderMessageIDs(m model.Message) []model.ExternalRef {
	var out []model.ExternalRef
	for _, r := range m.External {
		if r.Namespace == model.ProviderMessageNamespace && r.Kind == model.RefKindMessage {
			out = append(out, r)
		}
	}
	return out
}

// Attachments returns the distinct attachment references of m's parts.
func Attachments(m model.Message) []model.AttachmentRef {
	var out []model.AttachmentRef
	for _, p := range m.Content.Parts {
		if p.Attachment != nil && !slices.Contains(out, *p.Attachment) {
			out = append(out, *p.Attachment)
		}
	}
	return out
}

// Check validates the shape of a Redact request: exactly one target, an
// expected revision and a new revision that differs from it.
func Check(req store.Redaction) error {
	targets := 0
	for _, set := range []bool{req.Message != nil, req.Interaction != nil, req.Attachment != nil} {
		if set {
			targets++
		}
	}
	switch {
	case targets != 1:
		return model.NewError(model.Validation, "redaction: exactly one of message, interaction and attachment required")
	case req.Expected == "":
		return model.NewError(model.Validation, "expected: required")
	case req.Revision == "":
		return model.NewError(model.Validation, "revision: required")
	case req.Revision == req.Expected:
		return model.NewError(model.Validation, "revision: must differ from the expected revision")
	}
	return nil
}
