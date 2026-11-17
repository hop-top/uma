package model

import (
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// Part kinds. Part.Kind is a closed set; unknown kinds are rejected.
const (
	textPartKind       = "text"
	attachmentPartKind = "attachment"
	structuredPartKind = "structured"
)

// ContentLimits bounds content accepted at entry. A zero field takes the
// DefaultContentLimits value.
type ContentLimits struct {
	MaxParts           int
	MaxTextBytes       int
	MaxStructuredBytes int
}

// DefaultContentLimits returns the v0 entry limits: 64 parts, 1 MiB per text
// part, 256 KiB per structured payload.
func DefaultContentLimits() ContentLimits {
	return ContentLimits{MaxParts: 64, MaxTextBytes: 1 << 20, MaxStructuredBytes: 256 << 10}
}

func (l ContentLimits) withDefaults() ContentLimits {
	d := DefaultContentLimits()
	if l.MaxParts <= 0 {
		l.MaxParts = d.MaxParts
	}
	if l.MaxTextBytes <= 0 {
		l.MaxTextBytes = d.MaxTextBytes
	}
	if l.MaxStructuredBytes <= 0 {
		l.MaxStructuredBytes = d.MaxStructuredBytes
	}
	return l
}

// Validate checks c against DefaultContentLimits; see ValidateWith.
func (c Content) Validate() error { return c.ValidateWith(ContentLimits{}) }

// ValidateWith checks that c has 1 to MaxParts parts and that every part's
// kind agrees with its body: a text part carries only non-empty UTF-8 text, an
// attachment part only a valid attachment ref, a structured part only a valid
// versioned extension. Structured parts of unknown schemas are accepted and
// retained; use Executable before rendering or executing content.
func (c Content) ValidateWith(l ContentLimits) error { return c.validate("", l.withDefaults(), nil) }

// ValidateIn is Validate that also requires every attachment part to belong
// to inbox.
func (c Content) ValidateIn(inbox InboxRef) error {
	return c.validate("", DefaultContentLimits(), &inbox)
}

// validate checks c; when inbox is set, attachment parts must belong to it.
func (c Content) validate(path string, l ContentLimits, inbox *InboxRef) error {
	parts := fieldPath(path, "parts")
	switch {
	case len(c.Parts) == 0:
		return invalidField(parts, "required")
	case len(c.Parts) > l.MaxParts:
		return invalidField(parts, "exceeds part limit")
	}
	for i, p := range c.Parts {
		at := parts + "[" + strconv.Itoa(i) + "]"
		if err := p.validate(at, l); err != nil {
			return err
		}
		if inbox != nil && p.Attachment != nil && !p.Attachment.In(*inbox) {
			return invalidField(fieldPath(at, "attachment"), "must belong to the target inbox")
		}
	}
	return nil
}

func (p Part) validate(path string, l ContentLimits) error {
	switch p.Kind {
	case textPartKind:
		if p.Attachment != nil || p.Structured != nil {
			return invalidField(path, "text part must carry text only")
		}
		field := fieldPath(path, "text")
		switch {
		case p.Text == "":
			return invalidField(field, "required")
		case len(p.Text) > l.MaxTextBytes:
			return invalidField(field, "exceeds text limit")
		case !utf8.ValidString(p.Text):
			return invalidField(field, "must be valid UTF-8")
		}
		return nil
	case attachmentPartKind:
		if p.Text != "" || p.Structured != nil {
			return invalidField(path, "attachment part must carry an attachment only")
		}
		if p.Attachment == nil {
			return invalidField(fieldPath(path, "attachment"), "required")
		}
		return p.Attachment.validate(fieldPath(path, "attachment"), false)
	case structuredPartKind:
		if p.Text != "" || p.Attachment != nil {
			return invalidField(path, "structured part must carry a structured extension only")
		}
		if p.Structured == nil {
			return invalidField(fieldPath(path, "structured"), "required")
		}
		return p.Structured.validate(fieldPath(path, "structured"), l.MaxStructuredBytes)
	case "":
		return invalidField(fieldPath(path, "kind"), "required")
	}
	return invalidField(fieldPath(path, "kind"), "must be text, attachment or structured")
}

// SchemaSet reports which versioned structured schemas a host can render or
// execute.
type SchemaSet interface {
	Supports(name string, version uint32) bool
}

// UnknownParts returns the indexes of structured parts whose schema known does
// not support; a nil SchemaSet supports none.
func (c Content) UnknownParts(known SchemaSet) []int {
	var out []int
	for i, p := range c.Parts {
		if p.Kind != structuredPartKind || p.Structured == nil {
			continue
		}
		if known == nil || !known.Supports(p.Structured.Name, p.Structured.Version) {
			out = append(out, i)
		}
	}
	return out
}

// Executable reports whether every structured part has a supported schema.
// Content with unknown structured parts may be retained under restrictions but
// must not be rendered or executed as supported content.
func (c Content) Executable(known SchemaSet) bool { return len(c.UnknownParts(known)) == 0 }

// Validate checks a versioned extension: a registry-key shaped name, version
// 1 or later, and, when present, valid JSON data within the default structured
// payload limit.
func (e Extension) Validate() error {
	return e.validate("", DefaultContentLimits().MaxStructuredBytes)
}

func (e Extension) validate(path string, maxData int) error {
	name := fieldPath(path, "name")
	switch {
	case e.Name == "":
		return invalidField(name, "required")
	case !isRegistryKey(e.Name, 2):
		return invalidField(name, "must be lowercase dot-separated segments")
	case e.Version == 0:
		return invalidField(fieldPath(path, "version"), "must be 1 or later")
	case len(e.Data) > maxData:
		return invalidField(fieldPath(path, "data"), "exceeds payload limit")
	case len(e.Data) > 0 && !json.Valid(e.Data):
		return invalidField(fieldPath(path, "data"), "must be valid JSON")
	}
	return nil
}

// validateOptional checks e only when any field is set.
func (e Extension) validateOptional(path string) error {
	if e.Name == "" && e.Version == 0 && len(e.Data) == 0 {
		return nil
	}
	return e.validate(path, DefaultContentLimits().MaxStructuredBytes)
}

// Validate requires one of Public, Internal or Restricted; empty and unknown
// visibilities are invalid.
func (v Visibility) Validate() error { return v.validate("visibility") }

func (v Visibility) validate(field string) error {
	switch v {
	case Public, Internal, Restricted:
		return nil
	case "":
		return invalidField(field, "required")
	}
	return invalidField(field, "must be public, internal or restricted")
}

// Validate checks the visibility and, when set, the policy reference token.
func (a Audience) Validate() error { return a.validate("") }

func (a Audience) validate(path string) error {
	return firstError(
		a.Visibility.validate(fieldPath(path, "visibility")),
		checkOptionalToken(fieldPath(path, "policy_ref"), a.PolicyRef),
	)
}
