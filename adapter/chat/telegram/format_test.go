package telegram

import (
	"strings"
	"testing"
)

// unescapeMarkdownV2 reads text as Telegram's MarkdownV2 does for text
// outside every entity: "Any character with code between 1 and 126
// inclusively can be escaped anywhere with a preceding '\' character, in
// which case it is treated as an ordinary character and not a part of the
// markup", and "In all other places characters '_', '*', '[', ']', '(',
// ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!' must be
// escaped with the preceding character '\'"
// (https://core.telegram.org/bots/api#markdownv2-style). It reports false
// for text with a reserved character left bare: Telegram would refuse it or
// read markup into it.
func unescapeMarkdownV2(s string) (string, bool) {
	const reserved = "_*[]()~`>#+-=|{}.!"
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == '\\' && i+1 < len(rs) && rs[i+1] >= 1 && rs[i+1] <= 126:
			i++
			b.WriteRune(rs[i])
		case r == '\\' || strings.ContainsRune(reserved, r):
			return "", false
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), true
}

// unescapeHTML reads text as Telegram's HTML mode does outside every tag:
// "All <, > and & symbols that are not a part of a tag or an HTML entity
// must be replaced with the corresponding HTML entities", and "The API
// currently supports only the following named HTML entities: &lt;, &gt;,
// &amp; and &quot;" (https://core.telegram.org/bots/api#html-style). It
// reports false for a bare "<", ">" or "&".
func unescapeHTML(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<', '>':
			return "", false
		case '&':
			var ok bool
			for ent, lit := range map[string]string{"&lt;": "<", "&gt;": ">", "&amp;": "&", "&quot;": `"`} {
				if strings.HasPrefix(s[i:], ent) {
					b.WriteString(lit)
					i += len(ent) - 1
					ok = true
					break
				}
			}
			if !ok {
				return "", false
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), true
}

func allASCII() string {
	var b strings.Builder
	for c := rune(1); c <= 126; c++ {
		b.WriteRune(c)
	}
	return b.String()
}

func TestEscapeMarkdownV2(t *testing.T) {
	for name, in := range map[string]string{
		"every ASCII character": allASCII(),
		"reserved":              "_*[]()~`>#+-=|{}.!\\",
		"a sentence":            "Price: 5.99 (was 7-10!) #deal [link](http://x.y) *not bold* _nor italic_ ~x~ ||spoiler|| `code` > quote",
		"already escaped":       `\*not\* a\\b`,
		"non ASCII":             "héllo 😀 日本語 — “quoted”",
		"empty":                 "",
		"a backslash last":      `path\`,
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := unescapeMarkdownV2(EscapeMarkdownV2(in))
			if !ok || got != in {
				t.Fatalf("EscapeMarkdownV2(%q) = %q reads back as %q (clean %v)", in, EscapeMarkdownV2(in), got, ok)
			}
		})
	}
	if got := EscapeMarkdownV2("plain text 123"); got != "plain text 123" {
		t.Errorf("plain text changed: %q", got)
	}
	if got := EscapeMarkdownV2("a.b"); got != `a\.b` {
		t.Errorf("got %q", got)
	}
}

func TestEscapeMarkdownV2InsideCodeAndLinks(t *testing.T) {
	// "Inside pre and code entities, all '`' and '\' characters must be
	// escaped with a preceding '\' character."
	if got := EscapeMarkdownV2Code("a`b\\c.d*e"); got != "a\\`b\\\\c.d*e" {
		t.Errorf("code: %q", got)
	}
	// "Inside the (...) part of the inline link and custom emoji
	// definition, all ')' and '\' must be escaped with a preceding '\'
	// character."
	if got := EscapeMarkdownV2URL("http://x.y/a_(b)\\c"); got != "http://x.y/a_(b\\)\\\\c" {
		t.Errorf("url: %q", got)
	}
}

func TestEscapeHTML(t *testing.T) {
	for name, in := range map[string]string{
		"markup":      `<script>alert("x")</script>`,
		"an entity":   "&lt; is not <b>",
		"ampersand":   "fish & chips &amp; more",
		"plain":       "nothing special",
		"non ASCII":   "héllo 😀",
		"all ASCII":   allASCII(),
		"angle quote": "a > b < c",
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := unescapeHTML(EscapeHTML(in))
			if !ok || got != in {
				t.Fatalf("EscapeHTML(%q) = %q reads back as %q (clean %v)", in, EscapeHTML(in), got, ok)
			}
		})
	}
	if got := EscapeHTML("a<b>&c"); got != "a&lt;b&gt;&amp;c" {
		t.Errorf("got %q", got)
	}
}
