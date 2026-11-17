// Package mailboxes is what the mail adapters share of the provider
// mailbox list and create extensions (model.ProviderMailbox): the
// definitions they register, the check of a create's data, name matching
// and the result of a list. An adapter of another provider that lists and
// creates mailboxes the same way uses it too.
package mailboxes

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// ListDefinition is the registration of the mailbox list read of adapter
// type t. The capability that gates it is its own name.
func ListDefinition(t model.TypeKey) adapter.ExtensionDefinition {
	name := model.MailboxListName(t)
	return adapter.ExtensionDefinition{
		Name: name, Version: model.MailboxExtensionVersion, Kind: model.ExtensionKindRead,
		InputSchema:  model.Extension{Name: name + ".input", Version: model.MailboxExtensionVersion},
		ResultSchema: model.Extension{Name: name + ".result", Version: model.MailboxExtensionVersion},
		// The capability is the definition's own name, so a host sees per
		// channel whether it can list.
		RequiredCapability: model.Capability(name), AuthorizeAs: model.ActionExtensionQuery,
	}
}

// CreateDefinition is the registration of the mailbox create effect of
// adapter type t.
func CreateDefinition(t model.TypeKey) adapter.ExtensionDefinition {
	name := model.MailboxCreateName(t)
	return adapter.ExtensionDefinition{
		Name: name, Version: model.MailboxExtensionVersion, Kind: model.ExtensionKindEffect,
		InputSchema:        model.Extension{Name: name + ".input", Version: model.MailboxExtensionVersion},
		RequiredCapability: model.Capability(name), AuthorizeAs: model.ActionExtensionExecute,
	}
}

// ResultName is the result schema name of the list read of type t.
func ResultName(t model.TypeKey) string { return model.MailboxListName(t) + ".result" }

// CheckList checks a list query's fields: none are defined, so any JSON
// object (or no data) is accepted and unknown fields are ignored.
func CheckList(q model.Extension) *model.ValidationIssue {
	data := bytes.TrimSpace(q.Data)
	if len(data) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return &model.ValidationIssue{Field: "data", Code: string(model.Validation), Message: "query data must be a JSON object"}
	}
	return nil
}

// CheckCreate checks a create action's fields without I/O. kinds are the
// kinds the adapter has, the first its default; the returned action has
// its kind set. It returns the first issue, whose message is safe to show.
func CheckCreate(action model.Extension, kinds ...string) (model.CreateProviderMailbox, *model.ValidationIssue) {
	refuse := func(field, msg string) (model.CreateProviderMailbox, *model.ValidationIssue) {
		return model.CreateProviderMailbox{}, &model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
	}
	var c model.CreateProviderMailbox
	data := bytes.TrimSpace(action.Data)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return refuse("data", "action data must be a JSON object")
	}
	for _, f := range []struct {
		field string
		dst   *string
	}{{"name", &c.Name}, {"kind", &c.Kind}} {
		raw, set := fields[f.field]
		if !set || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		if err := json.Unmarshal(raw, f.dst); err != nil {
			return refuse("data."+f.field, f.field+" must be a string")
		}
	}
	if err := c.Validate(); err != nil {
		return refuse("data."+field(err), strings.TrimPrefix(safe(err), field(err)+": "))
	}
	if c.Kind == "" {
		c.Kind = kinds[0]
	}
	if !slices.Contains(kinds, c.Kind) {
		return refuse("data.kind", "this provider has no mailbox of kind "+c.Kind)
	}
	return c, nil
}

func safe(err error) string {
	if me, ok := err.(*model.Error); ok {
		return me.SafeMessage
	}
	return err.Error()
}

// field is the field a validation error names: the text before its colon.
func field(err error) string {
	f, _, _ := strings.Cut(safe(err), ": ")
	return f
}

// Find returns the mailbox of kind named name, compared without regard to
// case (Gmail labels, Exchange folders and categories are unique that way).
func Find(boxes []model.ProviderMailbox, name, kind string) (model.ProviderMailbox, bool) {
	for _, b := range boxes {
		if b.Kind == kind && strings.EqualFold(b.Name, name) {
			return b, true
		}
	}
	return model.ProviderMailbox{}, false
}

// Result returns the list read's result extension: boxes sorted by kind
// then name. A list over the structured payload limit is a permanent
// error, never a cut one.
func Result(t model.TypeKey, boxes []model.ProviderMailbox) (model.Extension, error) {
	slices.SortStableFunc(boxes, func(a, b model.ProviderMailbox) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	out, err := model.NewProviderMailboxesResult(ResultName(t), boxes)
	if err != nil {
		return model.Extension{}, model.NewError(model.Permanent, "mailbox list could not be encoded")
	}
	if len(out.Data) > model.DefaultContentLimits().MaxStructuredBytes {
		return model.Extension{}, model.NewError(model.Permanent, "mailbox list is over the result limit")
	}
	return out, nil
}
