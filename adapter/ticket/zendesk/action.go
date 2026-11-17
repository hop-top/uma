package zendesk

import (
	"encoding/json"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// ValidateAction checks the fields of an extension effect without I/O
// (adapter.ActionValidator): the rules Execute applies at the attempt, so
// an action Zendesk would refuse is a model.Validation error at the call
// with nothing stored. An action of another name or version is not judged
// here; core refuses it by its registration and Execute by its name.
func (f *Factory) ValidateAction(aud model.Audience, action model.Extension) []model.ValidationIssue {
	if _, issue := checkAction(aud, action); issue != nil {
		return []model.ValidationIssue{*issue}
	}
	return nil
}

var _ adapter.ActionValidator = (*Factory)(nil)

// checkAction is the one set of field rules of the extension effects. It
// returns the action's fields when they are valid, otherwise the first
// issue, whose message is safe to show.
func checkAction(aud model.Audience, action model.Extension) (map[string]json.RawMessage, *model.ValidationIssue) {
	refuse := func(field, msg string) (map[string]json.RawMessage, *model.ValidationIssue) {
		return nil, &model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
	}
	if action.Version != ExtensionVersion || (action.Name != ExtensionInternalNote && action.Name != ExtensionUpdate) {
		return nil, nil
	}
	fields, err := object(action.Data)
	if err != nil {
		return refuse("data", "action data must be a JSON object")
	}
	switch action.Name {
	case ExtensionInternalNote:
		if aud.Visibility == model.Public {
			return refuse("audience", "an internal note never has a public audience")
		}
		if _, set := fields["public"]; set {
			return refuse("data.public", "an internal note sets no public flag")
		}
		if !hasText(fields["body"]) && !hasText(fields["html_body"]) {
			return refuse("data.body", "an internal note needs a body")
		}
	case ExtensionUpdate:
		for _, k := range []string{"comment", "metadata", "safe_update", "updated_stamp"} {
			if _, set := fields[k]; set {
				return refuse("data."+k, "a ticket update sets no "+k)
			}
		}
		if len(fields) == 0 {
			return refuse("data", "a ticket update sets no field")
		}
	}
	return fields, nil
}
