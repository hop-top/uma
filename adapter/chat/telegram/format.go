package telegram

import "strings"

// Escaping for the text parse modes of the Bot API. A message sent with a
// parse mode is parsed: a character that means markup there changes the
// message, or makes Telegram refuse it with "can't parse entities". Text
// that reaches such a message from anyone but the host's own code, a user's
// name or a quoted message, goes through these first. The adapter itself
// never sends text under a parse mode unescaped: plain text parts are sent
// with no parse mode at all, and, beside a markup part (MarkupSchema), are
// escaped for its mode.

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// EscapeHTML escapes text for the HTML parse mode: "All <, > and & symbols
// that are not a part of a tag or an HTML entity must be replaced with the
// corresponding HTML entities (< with &lt;, > with &gt; and & with &amp;)"
// (https://core.telegram.org/bots/api#html-style). A quote needs no escape
// outside a tag's attribute value, where it is &quot;.
func EscapeHTML(s string) string { return htmlEscaper.Replace(s) }

// markdownV2Reserved are the characters that "must be escaped with the
// preceding character '\'" outside every entity, with the backslash itself:
// "'\' character usually must be escaped with a preceding '\' character"
// (https://core.telegram.org/bots/api#markdownv2-style).
const markdownV2Reserved = "\\_*[]()~`>#+-=|{}.!"

// EscapeMarkdownV2 escapes text for the MarkdownV2 parse mode, outside any
// entity. Every other character, non-ASCII ones included, is itself.
func EscapeMarkdownV2(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	for _, r := range s {
		if strings.ContainsRune(markdownV2Reserved, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// EscapeMarkdownV2Code escapes text for the inside of a MarkdownV2 code or
// pre entity: "all '`' and '\' characters must be escaped".
func EscapeMarkdownV2Code(s string) string {
	return strings.NewReplacer("\\", "\\\\", "`", "\\`").Replace(s)
}

// EscapeMarkdownV2URL escapes text for the (...) part of a MarkdownV2 inline
// link or custom emoji: "all ')' and '\' must be escaped".
func EscapeMarkdownV2URL(s string) string {
	return strings.NewReplacer("\\", "\\\\", ")", "\\)").Replace(s)
}
