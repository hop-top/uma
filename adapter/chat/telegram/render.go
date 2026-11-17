package telegram

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"hop.top/uma/model"
)

// Schemas of what the adapter sends.
const (
	// MarkupSchema is text under a parse mode: the data is a JSON object
	// with parse_mode (ParseModeHTML or ParseModeMarkdownV2) and text, the
	// markup. The host owns the markup: a character in it that means markup
	// is markup, so user text composed into it goes through EscapeHTML or
	// EscapeMarkdownV2 first. Text parts beside a markup part are escaped by
	// the adapter for its mode.
	MarkupSchema  = "telegram.messages.markup"
	MarkupVersion = 1
	// KeyboardSchema is an inline keyboard: the data is the JSON object of
	// an InlineKeyboardMarkup, {"inline_keyboard": [[button, ...], ...]}
	// (https://core.telegram.org/bots/api#inlinekeyboardmarkup). A callback
	// button comes back as an interaction.
	KeyboardSchema  = "telegram.messages.keyboard"
	KeyboardVersion = 1
)

// Parse modes a markup part may name. The legacy "Markdown" mode is "a
// legacy mode, retained for backward compatibility", cannot nest entities
// and has no way to escape inside one, and is not offered.
const (
	ParseModeHTML       = "HTML"
	ParseModeMarkdownV2 = "MarkdownV2"
)

// MaxTextRunes is how much text one message may carry: "1-4096 characters
// after entities parsing" (https://core.telegram.org/bots/api#sendmessage).
// The adapter refuses plain text over it instead of letting Telegram refuse
// it; under a parse mode the length after parsing is Telegram's to judge.
const MaxTextRunes = 4096

// MaxCallbackDataBytes is the size of a callback button's data: "1-64
// bytes" (https://core.telegram.org/bots/api#inlinekeyboardbutton).
const MaxCallbackDataBytes = 64

// rendered is what Telegram is sent for a message's content.
type rendered struct {
	text      string
	parseMode string
	keyboard  json.RawMessage
}

type markupData struct {
	ParseMode string `json:"parse_mode"`
	Text      string `json:"text"`
}

func decodeStrict(data []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v) == nil && !dec.More()
}

// render maps content to Telegram's text, parse mode and keyboard.
//
// Text parts are literal. With no markup part they are sent as they are,
// under no parse mode, so no character in them means anything to Telegram;
// with one, they are escaped for its mode and joined to the markup in order.
// Parts are joined by a blank line. At most one keyboard is carried, and
// every markup part names one parse mode. Anything else is unsupported, and
// nothing is cut.
func render(c model.Content) (rendered, *model.Error) {
	var out rendered
	for _, p := range c.Parts {
		switch {
		case p.Kind == model.PartText:
		case p.Kind == model.PartStructured && p.Structured != nil && p.Structured.Name == MarkupSchema && p.Structured.Version == MarkupVersion:
			var m markupData
			if !decodeStrict(p.Structured.Data, &m) || m.Text == "" {
				return rendered{}, final(model.Validation, MarkupSchema+" must hold a parse_mode and a text")
			}
			if m.ParseMode != ParseModeHTML && m.ParseMode != ParseModeMarkdownV2 {
				return rendered{}, final(model.Unsupported, "Telegram parse mode must be HTML or MarkdownV2")
			}
			if out.parseMode != "" && out.parseMode != m.ParseMode {
				return rendered{}, final(model.Unsupported, "a Telegram message has one parse mode")
			}
			out.parseMode = m.ParseMode
		case p.Kind == model.PartStructured && p.Structured != nil && p.Structured.Name == KeyboardSchema && p.Structured.Version == KeyboardVersion:
			if out.keyboard != nil {
				return rendered{}, final(model.Unsupported, "a Telegram message carries one inline keyboard")
			}
			if err := checkKeyboard(p.Structured.Data); err != nil {
				return rendered{}, err
			}
			out.keyboard = slices.Clone(p.Structured.Data)
		default:
			return rendered{}, final(model.Unsupported, "Telegram messages carry text, markup and an inline keyboard only")
		}
	}
	var pieces []string
	for _, p := range c.Parts {
		switch {
		case p.Kind == model.PartText:
			pieces = append(pieces, escapeFor(out.parseMode, p.Text))
		case p.Structured.Name == MarkupSchema:
			var m markupData
			_ = json.Unmarshal(p.Structured.Data, &m) // checked above
			pieces = append(pieces, m.Text)
		}
	}
	out.text = strings.Join(pieces, "\n\n")
	if strings.TrimSpace(out.text) == "" {
		return rendered{}, final(model.Validation, "content has no text")
	}
	if out.parseMode == "" && utf8.RuneCountInString(out.text) > MaxTextRunes {
		return rendered{}, final(model.Validation, "text exceeds Telegram's limit of 4096 characters")
	}
	return out, nil
}

func escapeFor(parseMode, s string) string {
	switch parseMode {
	case ParseModeHTML:
		return EscapeHTML(s)
	case ParseModeMarkdownV2:
		return EscapeMarkdownV2(s)
	}
	return s
}

// buttonActions are the fields of an InlineKeyboardButton besides its text,
// icon and style, of which "exactly one ... must be used".
var buttonActions = []string{
	"url", "callback_data", "web_app", "login_url", "switch_inline_query", "switch_inline_query_current_chat",
	"switch_inline_query_chosen_chat", "copy_text", "callback_game", "pay", "disabled",
}

// checkKeyboard checks the shape of an inline keyboard and that of its
// buttons, so a malformed one is refused before a call. What a field may
// hold beyond that is Telegram's to judge.
func checkKeyboard(data []byte) *model.Error {
	bad := func(msg string) *model.Error { return final(model.Validation, KeyboardSchema+": "+msg) }
	var k struct {
		Rows [][]map[string]json.RawMessage `json:"inline_keyboard"`
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(data, &probe) != nil || json.Unmarshal(data, &k) != nil || len(k.Rows) == 0 {
		return bad("must be an inline_keyboard of button rows")
	}
	for _, row := range k.Rows {
		if len(row) == 0 {
			return bad("a row of buttons is empty")
		}
		for _, b := range row {
			var label string
			if json.Unmarshal(b["text"], &label) != nil || label == "" {
				return bad("a button has no text")
			}
			n := 0
			for _, a := range buttonActions {
				if _, ok := b[a]; ok {
					n++
				}
			}
			if n != 1 {
				return bad("a button must have exactly one of " + strings.Join(buttonActions, ", "))
			}
			if raw, ok := b["callback_data"]; ok {
				var d string
				if json.Unmarshal(raw, &d) != nil || d == "" || len(d) > MaxCallbackDataBytes {
					return bad("callback_data must be 1 to 64 bytes")
				}
			}
		}
	}
	return nil
}
