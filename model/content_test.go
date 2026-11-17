package model

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func textPart(s string) Part { return Part{Kind: "text", Text: s} }

func structuredPart(name string, version uint32, data string) Part {
	return Part{Kind: "structured", Structured: &Extension{Name: name, Version: version, Data: json.RawMessage(data)}}
}

func TestVisibilityAndAudience(t *testing.T) {
	for _, v := range []Visibility{Public, Internal, Restricted} {
		requireOK(t, v.Validate())
		requireOK(t, Audience{Visibility: v}.Validate())
	}
	requireError(t, Visibility("").Validate(), Validation, "visibility")
	requireError(t, Visibility("secret").Validate(), Validation, "visibility")
	requireError(t, Audience{}.Validate(), Validation, "visibility")
	requireOK(t, Audience{Visibility: Restricted, PolicyRef: "desk.supervisors"}.Validate())
	requireError(t, Audience{Visibility: Restricted, PolicyRef: " x"}.Validate(), Validation, "policy_ref")
}

func TestExtensionValidate(t *testing.T) {
	cases := map[string]struct {
		ext   Extension
		field string
	}{
		"valid":          {Extension{Name: "org.example.card", Version: 1, Data: json.RawMessage(`{"a":1}`)}, ""},
		"no data":        {Extension{Name: "org.example.card", Version: 2}, ""},
		"missing name":   {Extension{Version: 1}, "name"},
		"bad name":       {Extension{Name: "Org.Example", Version: 1}, "name"},
		"zero version":   {Extension{Name: "org.example.card"}, "version"},
		"invalid json":   {Extension{Name: "org.example.card", Version: 1, Data: json.RawMessage(`{`)}, "data"},
		"oversized data": {Extension{Name: "org.example.card", Version: 1, Data: json.RawMessage(`"` + strings.Repeat("x", 256<<10) + `"`)}, "data"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.ext.Validate()
			if tc.field == "" {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, tc.field)
		})
	}
}

func TestContentValidate(t *testing.T) {
	att := &AttachmentRef{Inbox: inboxA, ID: idC}
	cases := map[string]struct {
		content Content
		field   string
	}{
		"text":                 {Content{Parts: []Part{textPart("hello")}}, ""},
		"attachment":           {Content{Parts: []Part{{Kind: "attachment", Attachment: att}}}, ""},
		"structured":           {Content{Parts: []Part{structuredPart("org.example.card", 1, `{}`)}}, ""},
		"unknown structured":   {Content{Parts: []Part{structuredPart("com.vendor.unknown", 9, `[1]`)}}, ""},
		"mixed":                {Content{Parts: []Part{textPart("a"), {Kind: "attachment", Attachment: att}}}, ""},
		"no parts":             {Content{}, "parts"},
		"empty kind":           {Content{Parts: []Part{{Text: "x"}}}, "parts[0].kind"},
		"unknown kind":         {Content{Parts: []Part{{Kind: "html", Text: "<b>"}}}, "parts[0].kind"},
		"empty text":           {Content{Parts: []Part{textPart("")}}, "parts[0].text"},
		"invalid utf8":         {Content{Parts: []Part{textPart("a\xff")}}, "parts[0].text"},
		"oversized text":       {Content{Parts: []Part{textPart(strings.Repeat("x", (1<<20)+1))}}, "parts[0].text"},
		"text with attachment": {Content{Parts: []Part{{Kind: "text", Text: "x", Attachment: att}}}, "parts[0]"},
		"text with structured": {Content{Parts: []Part{{Kind: "text", Text: "x", Structured: &Extension{}}}}, "parts[0]"},
		"attachment missing":   {Content{Parts: []Part{{Kind: "attachment"}}}, "parts[0].attachment"},
		"attachment with text": {Content{Parts: []Part{{Kind: "attachment", Text: "x", Attachment: att}}}, "parts[0]"},
		"attachment bad ref":   {Content{Parts: []Part{{Kind: "attachment", Attachment: &AttachmentRef{Inbox: inboxA}}}}, "parts[0].attachment.id"},
		"structured missing":   {Content{Parts: []Part{{Kind: "structured"}}}, "parts[0].structured"},
		"structured with text": {Content{Parts: []Part{{Kind: "structured", Text: "x", Structured: &Extension{Name: "a.b", Version: 1}}}}, "parts[0]"},
		"structured bad json":  {Content{Parts: []Part{structuredPart("org.example.card", 1, `nope`)}}, "parts[0].structured.data"},
		"second part invalid":  {Content{Parts: []Part{textPart("ok"), textPart("")}}, "parts[1].text"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.content.Validate()
			if tc.field == "" {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, tc.field)
		})
	}
}

func TestContentLimits(t *testing.T) {
	parts := make([]Part, 3)
	for i := range parts {
		parts[i] = textPart("abcd")
	}
	c := Content{Parts: parts}
	requireOK(t, c.ValidateWith(ContentLimits{}))
	requireError(t, c.ValidateWith(ContentLimits{MaxParts: 2}), Validation, "parts")
	requireError(t, c.ValidateWith(ContentLimits{MaxTextBytes: 3}), Validation, "parts[0].text")
	requireOK(t, c.ValidateWith(ContentLimits{MaxParts: 3, MaxTextBytes: 4}))

	s := Content{Parts: []Part{structuredPart("org.example.card", 1, `"abcdef"`)}}
	requireError(t, s.ValidateWith(ContentLimits{MaxStructuredBytes: 4}), Validation, "parts[0].structured.data")

	d := DefaultContentLimits()
	if d.MaxParts <= 0 || d.MaxTextBytes <= 0 || d.MaxStructuredBytes <= 0 {
		t.Fatalf("default limits not positive: %+v", d)
	}
	many := Content{Parts: make([]Part, d.MaxParts+1)}
	for i := range many.Parts {
		many.Parts[i] = textPart("x")
	}
	requireError(t, many.Validate(), Validation, "parts")
}

type schemaSet map[string]bool

func (s schemaSet) Supports(name string, version uint32) bool {
	return s[name+"/"+strconv.FormatUint(uint64(version), 10)]
}

func TestUnknownStructuredPartsAreRetainedButNotExecutable(t *testing.T) {
	c := Content{Parts: []Part{
		textPart("hi"),
		structuredPart("org.example.card", 1, `{}`),
		structuredPart("org.example.card", 2, `{}`),
		{Kind: "attachment", Attachment: &AttachmentRef{Inbox: inboxA, ID: idC}},
	}}
	requireOK(t, c.Validate())

	known := schemaSet{"org.example.card/1": true}
	if got := c.UnknownParts(known); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("UnknownParts = %v, want [2]", got)
	}
	if c.Executable(known) {
		t.Fatal("content with an unknown structured part reported executable")
	}
	if got := c.UnknownParts(nil); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("UnknownParts(nil) = %v, want [1 2]", got)
	}
	both := schemaSet{"org.example.card/1": true, "org.example.card/2": true}
	if !c.Executable(both) || c.UnknownParts(both) != nil {
		t.Fatal("fully known content reported non-executable")
	}
	plain := Content{Parts: []Part{textPart("x")}}
	if !plain.Executable(nil) {
		t.Fatal("text-only content reported non-executable")
	}
}
