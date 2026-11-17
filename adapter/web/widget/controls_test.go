package widget_test

import (
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

const controlsJSON = `{
	"buttons": [{"action": "help", "label": "Help", "style": "primary"}],
	"selects": [{"action": "pick", "label": "Plan", "options": [{"value": "a", "label": "Free"}, {"value": "b", "label": "Pro"}]}],
	"form": {"schema": "org.example.feedback", "submit": "Send", "fields": [{"name": "email", "label": "Email", "type": "email", "required": true}]}
}`

func structured(name string, version uint32, data string) model.Part {
	return model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: name, Version: version, Data: json.RawMessage(data)}}
}

func TestControlsPassThrough(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	unknown := structured("org.example.chart", 3, `{"bars":[1,2,3]}`)
	out, err := r.inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{
		Effect: adapter.Effect{Route: route(chanA, s), Audience: model.Audience{Visibility: model.Public}},
		Content: model.Content{Parts: []model.Part{
			{Kind: model.PartText, Text: "Pick one"},
			structured(widget.ControlsSchema, widget.ControlsVersion, controlsJSON),
			unknown,
		}},
	})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	ev := r.connect(s.Token, "").next(t)
	if len(ev.Data.Parts) != 3 {
		t.Fatalf("parts = %+v, want the text, the controls and the unknown part, nothing flattened or added", ev.Data.Parts)
	}
	c := ev.Data.Parts[1]
	if c.Kind != model.PartStructured || c.Text != "" || c.Structured == nil || c.Structured.Name != widget.ControlsSchema ||
		c.Structured.Version != widget.ControlsVersion {
		t.Fatalf("controls part = %+v, want the structured controls", c)
	}
	var want, got any
	if err := json.Unmarshal([]byte(controlsJSON), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(c.Structured.Data, &got); err != nil || !jsonEqual(want, got) {
		t.Fatalf("controls data = %s, want it passed through unchanged", c.Structured.Data)
	}
	u := ev.Data.Parts[2]
	if u.Kind != model.PartStructured || u.Text != "" || u.Structured == nil || u.Structured.Name != "org.example.chart" ||
		u.Structured.Version != 3 || string(u.Structured.Data) != `{"bars":[1,2,3]}` {
		t.Fatalf("unknown part = %+v, want it delivered as the structured part it is: the widget never flattens it to text", u)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestControlsAreChecked(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	for name, data := range map[string]string{
		"not an object":           `[]`,
		"no controls":             `{}`,
		"button without action":   `{"buttons":[{"label":"Help"}]}`,
		"button without label":    `{"buttons":[{"action":"help"}]}`,
		"unknown field":           `{"buttons":[{"action":"help","label":"Help","onclick":"x"}]}`,
		"duplicate action":        `{"buttons":[{"action":"a","label":"A"}],"selects":[{"action":"a","label":"B","options":[{"value":"x","label":"X"}]}]}`,
		"select without options":  `{"selects":[{"action":"pick","label":"Plan"}]}`,
		"form schema not a key":   `{"form":{"schema":"Feedback","submit":"Send","fields":[{"name":"e","label":"E","type":"text"}]}}`,
		"form without fields":     `{"form":{"schema":"org.example.feedback","submit":"Send"}}`,
		"form field of odd type":  `{"form":{"schema":"org.example.feedback","submit":"Send","fields":[{"name":"e","label":"E","type":"file"}]}}`,
		"duplicate form field":    `{"form":{"schema":"org.example.feedback","submit":"Send","fields":[{"name":"e","label":"E","type":"text"},{"name":"e","label":"F","type":"text"}]}}`,
		"action with a blank":     `{"buttons":[{"action":"a b","label":"A"}]}`,
		"empty option value":      `{"selects":[{"action":"pick","label":"P","options":[{"value":"","label":"X"}]}]}`,
		"form without submit":     `{"form":{"schema":"org.example.feedback","fields":[{"name":"e","label":"E","type":"text"}]}}`,
		"form field without name": `{"form":{"schema":"org.example.feedback","submit":"Send","fields":[{"label":"E","type":"text"}]}}`,
	} {
		_, err := r.inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{
			Effect:  adapter.Effect{Route: route(chanA, s), Audience: model.Audience{Visibility: model.Public}},
			Content: model.Content{Parts: []model.Part{structured(widget.ControlsSchema, widget.ControlsVersion, data)}},
		})
		if model.CodeOf(err) != model.Validation {
			t.Errorf("%s: Send = %v, want validation", name, err)
		}
	}
	// A newer version of the schema is unknown to this widget and passes through.
	out, err := r.inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{
		Effect:  adapter.Effect{Route: route(chanA, s), Audience: model.Audience{Visibility: model.Public}},
		Content: model.Content{Parts: []model.Part{structured(widget.ControlsSchema, widget.ControlsVersion+1, `{"anything":true}`)}},
	})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send of a newer controls version = %+v, %v, want it delivered untouched", out, err)
	}
}

func TestDescribeDeclaresInteractions(t *testing.T) {
	f, _ := widget.New(widget.Options{Verifier: headerVerifier})
	d := f.Describe()
	for _, c := range []model.Capability{model.CapabilityRespond, model.CapabilityRespondFollowUp} {
		if !slices.Contains(d.Capabilities, c) {
			t.Errorf("capabilities = %v, want %s", d.Capabilities, c)
		}
	}
	if !slices.Contains(d.Events, model.EventInteractionReceived) {
		t.Errorf("events = %v, want %s", d.Events, model.EventInteractionReceived)
	}
}
