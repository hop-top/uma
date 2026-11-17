package model

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// SearchTokens is the one tokenizer both stores index with and queries
// match against: runs of letters, marks and digits, simple case folding to
// the fold orbit's smallest rune, distinct in first-occurrence order,
// tokens over MaxSearchTokenBytes left out.
func TestSearchTokens(t *testing.T) {
	long := strings.Repeat("a", MaxSearchTokenBytes+1)
	edge := strings.Repeat("b", MaxSearchTokenBytes)
	for name, c := range map[string]struct {
		in   []string
		want []string
	}{
		"empty":            {[]string{""}, nil},
		"none":             {nil, nil},
		"separators only":  {[]string{" \t\n.,;:!?-_/@"}, nil},
		"words":            {[]string{"Hello, World"}, []string{"HELLO", "WORLD"}},
		"email splits":     {[]string{"Mail foo@bar.com"}, []string{"MAIL", "FOO", "BAR", "COM"}},
		"version splits":   {[]string{"re-open v2.1"}, []string{"RE", "OPEN", "V2", "1"}},
		"underscore":       {[]string{"snake_case"}, []string{"SNAKE", "CASE"}},
		"apostrophe":       {[]string{"don't"}, []string{"DON", "T"}},
		"digits":           {[]string{"order 2026-10-11"}, []string{"ORDER", "2026", "10", "11"}},
		"non-latin digits": {[]string{"رقم ٣٤"}, []string{"رقم", "٣٤"}},
		"distinct":         {[]string{"foo Foo FOO bar foo"}, []string{"FOO", "BAR"}},
		"accented fold":    {[]string{"été ÉTÉ"}, []string{"ÉTÉ"}},
		"sigma orbit":      {[]string{"ΟΔΟΣ οδοσ οδος"}, []string{"ΟΔΟΣ"}},
		"kelvin sign":      {[]string{"K k K"}, []string{"K"}},
		"long s":           {[]string{"ſ s"}, []string{"S"}},
		"combining mark":   {[]string{"naïve"}, []string{"NAÏVE"}},
		"no normalization": {[]string{"café café"}, []string{"CAFÉ", "CAFÉ"}},
		"cjk run":          {[]string{"日本語のテキスト。次"}, []string{"日本語のテキスト", "次"}},
		"emoji separates":  {[]string{"ok👍go"}, []string{"OK", "GO"}},
		"invalid utf8":     {[]string{"ab\xffcd"}, []string{"AB", "CD"}},
		"long dropped":     {[]string{"x " + long + " y"}, []string{"X", "Y"}},
		"edge kept":        {[]string{edge}, []string{strings.ToUpper(edge)}},
		"texts separate":   {[]string{"foo", "bar", "foo"}, []string{"FOO", "BAR"}},
		"texts never join": {[]string{"ab", "cd"}, []string{"AB", "CD"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := SearchTokens(c.in...); !slices.Equal(got, c.want) {
				t.Fatalf("SearchTokens(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Content.SearchTokens tokenizes text parts only: structured and
// attachment parts are never searched.
func TestContentSearchTokens(t *testing.T) {
	att := AttachmentRef{Inbox: inboxA, ID: idA}
	c := Content{Parts: []Part{
		{Kind: textPartKind, Text: "Invoice due"},
		{Kind: attachmentPartKind, Attachment: &att},
		{Kind: structuredPartKind, Text: "hidden", Structured: &Extension{Name: "x.card", Version: 1, Data: []byte(`{"t":"secret"}`)}},
		{Kind: textPartKind, Text: "invoice paid"},
	}}
	if got, want := c.SearchTokens(), []string{"INVOICE", "DUE", "PAID"}; !slices.Equal(got, want) {
		t.Fatalf("Content.SearchTokens = %q, want %q", got, want)
	}
}

// The search fields of Filter: text, half-open OccurredAt bounds and an
// attachment predicate, each validated before any query honours it.
func TestMessageSearchFilterValidate(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Nanosecond)
	yes, no := true, false
	many := make([]string, MaxSearchTokens+1)
	for i := range many {
		many[i] = "w" + strings.Repeat("x", i)
	}
	types := make([]string, MaxSearchMediaTypes+1)
	for i := range types {
		types[i] = "image/t" + strings.Repeat("x", i)
	}
	for name, c := range map[string]struct {
		f     Filter
		field string // "" = valid
	}{
		"text":                   {Filter{Text: "invoice 42"}, ""},
		"text at byte cap":       {Filter{Text: strings.Repeat("ab ", MaxSearchTextBytes/3) + "ab"[:MaxSearchTextBytes%3]}, ""},
		"text at token cap":      {Filter{Text: strings.Join(many[:MaxSearchTokens], " ")}, ""},
		"repeats count once":     {Filter{Text: strings.Repeat("foo ", MaxSearchTokens+1)}, ""},
		"text token at cap":      {Filter{Text: strings.Repeat("a", MaxSearchTokenBytes)}, ""},
		"text over byte cap":     {Filter{Text: strings.Repeat("a ", MaxSearchTextBytes/2) + "a"}, "text"},
		"text no tokens":         {Filter{Text: " -- !! "}, "text"},
		"text over token cap":    {Filter{Text: strings.Join(many, " ")}, "text"},
		"text token too long":    {Filter{Text: "ok " + strings.Repeat("a", MaxSearchTokenBytes+1)}, "text"},
		"text invalid utf8":      {Filter{Text: "ok \xff"}, "text"},
		"since":                  {Filter{Since: &t0}, ""},
		"before":                 {Filter{Before: &t0}, ""},
		"since before":           {Filter{Since: &t0, Before: &t1}, ""},
		"since equals before":    {Filter{Since: &t0, Before: &t0}, "before"},
		"since after before":     {Filter{Since: &t1, Before: &t0}, "before"},
		"zero since":             {Filter{Since: &time.Time{}}, "since"},
		"zero before":            {Filter{Before: &time.Time{}}, "before"},
		"has":                    {Filter{Attachments: &AttachmentFilter{Has: &yes}}, ""},
		"has not":                {Filter{Attachments: &AttachmentFilter{Has: &no}}, ""},
		"media types":            {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{"image/png", "application/*"}}}, ""},
		"media types has":        {Filter{Attachments: &AttachmentFilter{Has: &yes, MediaTypes: []string{"text/plain"}}}, ""},
		"media types cap":        {Filter{Attachments: &AttachmentFilter{MediaTypes: types[:MaxSearchMediaTypes]}}, ""},
		"empty attachments":      {Filter{Attachments: &AttachmentFilter{}}, "attachments"},
		"empty media type list":  {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{}}}, "attachments"},
		"media types has not":    {Filter{Attachments: &AttachmentFilter{Has: &no, MediaTypes: []string{"image/png"}}}, "attachments.media_types"},
		"media types over cap":   {Filter{Attachments: &AttachmentFilter{MediaTypes: types}}, "attachments.media_types"},
		"media type uppercase":   {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{"image/png", "Image/PNG"}}}, "attachments.media_types[1]"},
		"media type no subtype":  {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{"image"}}}, "attachments.media_types[0]"},
		"media type empty":       {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{""}}}, "attachments.media_types[0]"},
		"media type any":         {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{"*/*"}}}, "attachments.media_types[0]"},
		"media type wild type":   {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{"*/png"}}}, "attachments.media_types[0]"},
		"media type partial *":   {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{"image/p*"}}}, "attachments.media_types[0]"},
		"media type params":      {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{"text/plain;charset=utf-8"}}}, "attachments.media_types[0]"},
		"media type space":       {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{"text/plain "}}}, "attachments.media_types[0]"},
		"media type extra slash": {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{"a/b/c"}}}, "attachments.media_types[0]"},
		"media type duplicate":   {Filter{Attachments: &AttachmentFilter{MediaTypes: []string{"image/png", "image/png"}}}, "attachments.media_types[1]"},
	} {
		t.Run(name, func(t *testing.T) {
			if c.field == "" {
				requireOK(t, c.f.Validate())
				requireOK(t, ListRequest{Call: call, Filter: c.f}.Validate())
				return
			}
			requireError(t, c.f.Validate(), Validation, c.field)
			requireError(t, ListRequest{Call: call, Filter: c.f}.Validate(), Validation, "filter."+c.field)
		})
	}
}

// Clone deep-copies the search fields: no pointer or slice is shared.
func TestFilterSearchClone(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	yes := true
	f := Filter{Text: "x", Since: &t0, Before: &t1, Attachments: &AttachmentFilter{Has: &yes, MediaTypes: []string{"image/png"}}}
	c := f.Clone()
	if c.Text != "x" || c.Since == nil || !c.Since.Equal(t0) || c.Before == nil || !c.Before.Equal(t1) ||
		c.Attachments == nil || c.Attachments.Has == nil || !*c.Attachments.Has || !slices.Equal(c.Attachments.MediaTypes, f.Attachments.MediaTypes) {
		t.Fatalf("Clone dropped a search field: %+v", c)
	}
	if c.Since == f.Since || c.Before == f.Before || c.Attachments == f.Attachments || c.Attachments.Has == f.Attachments.Has {
		t.Fatal("Clone aliases a search field pointer")
	}
	*c.Since = t1
	*c.Before = t0
	*c.Attachments.Has = false
	c.Attachments.MediaTypes[0] = "text/plain"
	if !f.Since.Equal(t0) || !f.Before.Equal(t1) || !*f.Attachments.Has || f.Attachments.MediaTypes[0] != "image/png" {
		t.Fatal("mutating the clone changed the original")
	}
	if got := (Filter{}).Clone(); got.Since != nil || got.Before != nil || got.Attachments != nil {
		t.Fatalf("Clone invented search fields: %+v", got)
	}
}
