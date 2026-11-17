package widget

import (
	"bytes"
	"encoding/json"
	"slices"
	"unicode/utf8"

	"hop.top/uma/model"
)

// The controls schema (a structured part, model.PartStructured, named
// ControlsSchema at ControlsVersion) is the adapter's own vocabulary for
// what a visitor can press, pick and fill in: buttons, selects and one
// form. The widget page renders it and the visitor's choice comes back as
// an action (ActionRequest) or a submission (SubmissionRequest). A
// structured part of any other schema, or of a later version of this one,
// is delivered unchanged and left to the page to ignore: the widget never
// turns one into text.
const (
	ControlsSchema         = string(Type) + ".controls"
	ControlsVersion uint32 = 1
)

// Limits of the controls schema.
const (
	maxControls     = 25
	maxOptions      = 100
	maxFormFields   = 50
	maxControlBytes = 256
)

// Controls is the data of a controls part. At least one of Buttons,
// Selects and Form is set; action IDs are unique across Buttons and
// Selects.
type Controls struct {
	Buttons []Button `json:"buttons,omitempty"`
	Selects []Select `json:"selects,omitempty"`
	Form    *Form    `json:"form,omitempty"`
}

// Button is a control that sends the action Action when pressed. Value is
// sent with it. Style is "primary", "secondary" or "danger", and empty for
// the page's default.
type Button struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	Value  string `json:"value,omitempty"`
	Style  string `json:"style,omitempty"`
}

// Select is a control that sends the action Action with the Value of the
// option the visitor picked.
type Select struct {
	Action  string   `json:"action"`
	Label   string   `json:"label"`
	Options []Option `json:"options"`
}

// Option is one choice of a Select.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Form is a form whose submission is a SubmissionRequest under Schema, a
// registry key at version 1. The submitted fields are an object of the
// field names to their values: strings, numbers for number fields and
// booleans for checkboxes.
type Form struct {
	Schema string      `json:"schema"`
	Submit string      `json:"submit"`
	Fields []FormField `json:"fields"`
}

// FormField is one input of a Form. Type is "text", "textarea", "email",
// "number" or "checkbox".
type FormField struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
}

// ControlsPart returns the part carrying c, after the checks Send and the
// other outbound calls make of it.
func ControlsPart(c Controls) (model.Part, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return model.Part{}, controlsError("do not encode")
	}
	p := model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: ControlsSchema, Version: ControlsVersion, Data: data}}
	return p, checkControls(*p.Structured)
}

func controlsError(why string) error {
	return model.NewError(model.Validation, "controls: "+why)
}

// checkControls checks the data of a controls part. An extension of another
// schema or version is not this widget's to judge.
func checkControls(e model.Extension) error {
	if e.Name != ControlsSchema || e.Version != ControlsVersion {
		return nil
	}
	var c Controls
	dec := json.NewDecoder(bytes.NewReader(e.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || dec.More() {
		return controlsError("data must be an object of buttons, selects and a form")
	}
	if len(c.Buttons)+len(c.Selects) > maxControls {
		return controlsError("too many buttons and selects")
	}
	if len(c.Buttons)+len(c.Selects) == 0 && c.Form == nil {
		return controlsError("nothing to show")
	}
	var actions []string
	for _, b := range c.Buttons {
		if !slices.Contains([]string{"", "primary", "secondary", "danger"}, b.Style) || !validText(b.Label) || !validToken(b.Action) ||
			len(b.Value) > maxControlBytes || !utf8.ValidString(b.Value) {
			return controlsError("a button needs an action, a label and a known style")
		}
		actions = append(actions, b.Action)
	}
	for _, s := range c.Selects {
		if !validToken(s.Action) || !validText(s.Label) || len(s.Options) == 0 || len(s.Options) > maxOptions {
			return controlsError("a select needs an action, a label and options")
		}
		for _, o := range s.Options {
			if o.Value == "" || len(o.Value) > maxControlBytes || !utf8.ValidString(o.Value) || !validText(o.Label) {
				return controlsError("an option needs a value and a label")
			}
		}
		actions = append(actions, s.Action)
	}
	for i, a := range actions {
		if slices.Index(actions, a) != i {
			return controlsError("action IDs must be distinct")
		}
	}
	if c.Form != nil {
		return checkForm(*c.Form)
	}
	return nil
}

func checkForm(f Form) error {
	if (model.Extension{Name: f.Schema, Version: 1}).Validate() != nil || !validText(f.Submit) || len(f.Fields) == 0 || len(f.Fields) > maxFormFields {
		return controlsError("a form needs a registry-key schema, a submit label and fields")
	}
	var names []string
	for _, fl := range f.Fields {
		if !validToken(fl.Name) || !validText(fl.Label) || !slices.Contains([]string{"text", "textarea", "email", "number", "checkbox"}, fl.Type) ||
			slices.Contains(names, fl.Name) {
			return controlsError("a form field needs a distinct name, a label and a known type")
		}
		names = append(names, fl.Name)
	}
	return nil
}

// validToken reports whether s is usable as an action ID or field name: a
// client ID of at most 128 bytes.
func validToken(s string) bool { return validClientID(s) && utf8.ValidString(s) }

// validText reports whether s is a non-empty label of bounded size.
func validText(s string) bool { return s != "" && len(s) <= maxControlBytes && utf8.ValidString(s) }
