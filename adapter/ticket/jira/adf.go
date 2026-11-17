package jira

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"hop.top/uma/model"
)

// maxBodyRunes is the limit Jira Cloud puts on the text of a comment or
// description: 32,767 characters. It is not configurable on Cloud; the
// adapter counts the characters it sends.
const maxBodyRunes = 32767

// adfNode is a node of an Atlassian Document Format document, the body
// format of comments and descriptions in REST API v3.
type adfNode struct {
	Type    string         `json:"type"`
	Version int            `json:"version,omitempty"`
	Text    string         `json:"text,omitempty"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Content []adfNode      `json:"content,omitempty"`
}

// renderADF returns the ADF document of content: its text parts, each
// blank-line-separated block a paragraph, each newline within one a
// hardBreak. Other parts are unsupported. A text node is never empty: the
// format requires text.
func renderADF(c model.Content) (adfNode, error) {
	var texts []string
	runes := 0
	for _, p := range c.Parts {
		if p.Kind != model.PartText {
			return adfNode{}, final(model.Unsupported, "Jira comments carry text only")
		}
		texts = append(texts, p.Text)
		runes += utf8.RuneCountInString(p.Text)
	}
	if len(texts) == 0 {
		return adfNode{}, final(model.Validation, "content has no text")
	}
	if runes > maxBodyRunes {
		return adfNode{}, final(model.Validation, "comment exceeds Jira's 32767 character limit")
	}
	doc := adfNode{Type: "doc", Version: 1, Content: []adfNode{}}
	for block := range strings.SplitSeq(strings.Join(texts, "\n\n"), "\n\n") {
		var para adfNode
		para.Type = "paragraph"
		for i, line := range strings.Split(strings.Trim(block, "\n"), "\n") {
			if i > 0 {
				para.Content = append(para.Content, adfNode{Type: "hardBreak"})
			}
			if line != "" {
				para.Content = append(para.Content, adfNode{Type: "text", Text: line})
			}
		}
		if len(para.Content) > 0 {
			doc.Content = append(doc.Content, para)
		}
	}
	if len(doc.Content) == 0 {
		return adfNode{}, final(model.Validation, "content has no text")
	}
	return doc, nil
}

// bodyText returns the plain text of a body field: an ADF document
// (REST API v3, and webhooks of the same shape) or a plain string (wiki
// markup, which is read as it is). Absent or null is empty.
func bodyText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var doc adfNode
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(flatten(doc))
}

// flatten renders a node as text: block nodes are separated by a blank
// line, list items and table rows by a newline, hard breaks are newlines;
// mentions, emoji and cards use their visible text or URL; media is
// dropped.
func flatten(n adfNode) string {
	switch n.Type {
	case "text":
		return n.Text
	case "hardBreak":
		return "\n"
	case "mention", "emoji", "status", "date":
		if t, ok := n.Attrs["text"].(string); ok {
			return t
		}
		if t, ok := n.Attrs["shortName"].(string); ok {
			return t
		}
		return ""
	case "inlineCard", "blockCard", "embedCard":
		if u, ok := n.Attrs["url"].(string); ok {
			return u
		}
		return ""
	case "media", "mediaSingle", "mediaGroup", "mediaInline":
		return ""
	}
	sep := ""
	switch n.Type {
	case "doc", "blockquote", "panel", "expand":
		sep = "\n\n"
	case "nestedExpand", "table", "bulletList", "orderedList", "listItem", "tableRow", "tableCell", "tableHeader":
		sep = "\n"
	}
	var parts []string
	for _, c := range n.Content {
		if t := flatten(c); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, sep)
}

// echoProperty is the value of the EchoPropertyKey comment property.
type echoProperty struct {
	Operation model.ID `json:"operation"`
	Attempt   model.ID `json:"attempt"`
}

// propertyEntry is one entry of a comment's "properties" array.
type propertyEntry struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

func echoEntry(op model.OperationRef, attempt model.ID) propertyEntry {
	v, _ := json.Marshal(echoProperty{Operation: op.ID, Attempt: attempt})
	return propertyEntry{Key: EchoPropertyKey, Value: v}
}

// echoOf returns the operation and attempt named by the echo property among
// props.
func echoOf(props []propertyEntry) (op, attempt model.ID, ok bool) {
	for _, p := range props {
		if p.Key != EchoPropertyKey {
			continue
		}
		var e echoProperty
		if json.Unmarshal(p.Value, &e) == nil && e.Operation != "" && e.Attempt != "" {
			return e.Operation, e.Attempt, true
		}
	}
	return "", "", false
}
