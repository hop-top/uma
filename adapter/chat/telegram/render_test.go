package telegram

import (
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func markup(mode, text string) model.Part {
	data, _ := json.Marshal(map[string]string{"parse_mode": mode, "text": text})
	return model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: MarkupSchema, Version: MarkupVersion, Data: data}}
}

func keyboard(raw string) model.Part {
	return model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: KeyboardSchema, Version: KeyboardVersion, Data: []byte(raw)}}
}

func textPart(s string) model.Part { return model.Part{Kind: model.PartText, Text: s} }

func content(parts ...model.Part) model.Content { return model.Content{Parts: parts} }

func (r *rig) lastSend(t *testing.T) map[string]any {
	t.Helper()
	calls := r.callsOf("sendMessage")
	if len(calls) == 0 {
		t.Fatal("no sendMessage call")
	}
	return calls[len(calls)-1].args()
}

// Text parts are literal: with no markup part they are sent under no parse
// mode, so a character that is markup in Telegram's modes is only text.
func TestPlainTextIsNeverParsed(t *testing.T) {
	r := newRig(t)
	for name, in := range map[string]string{
		"all ASCII": allASCII(),
		"markdown":  "*bold* _italic_ [x](http://y) `code` ```pre``` ~s~ ||spoiler|| > quote",
		"html":      `<b>bold</b> <a href="tg://user?id=1">x</a> &amp; &lt;`,
		"a mention": "hi [you](tg://user?id=123456789)",
		"backslash": `C:\path\to\file`,
		"non ASCII": "héllo 😀 日本語",
	} {
		t.Run(name, func(t *testing.T) {
			accept(t, r, r.room(testGroup), text(in))
			a := r.lastSend(t)
			if a["text"] != in {
				t.Errorf("text %q, want %q as it is", a["text"], in)
			}
			if _, ok := a["parse_mode"]; ok {
				t.Errorf("parse_mode %v on plain text", a["parse_mode"])
			}
			if _, ok := a["entities"]; ok {
				t.Error("entities on plain text")
			}
		})
	}
}

// User text beside a markup part is escaped for its mode, never
// interpolated as it is: whatever the text holds, it reads back as the same
// text and no markup of its own survives.
func TestTextBesideMarkupIsEscaped(t *testing.T) {
	r := newRig(t)
	user := "Mallory <script>*pwn*</script> & [x](tg://user?id=1) _a_.b! ~c~ `d` \\e"
	for _, tc := range []struct {
		mode      string
		markup    string
		unescape  func(string) (string, bool)
		wantFirst string
	}{
		{ParseModeHTML, "<b>from the host</b>", unescapeHTML, EscapeHTML(user)},
		{ParseModeMarkdownV2, "*from the host*", unescapeMarkdownV2, EscapeMarkdownV2(user)},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			// The user's text first, then after the markup: both orders keep
			// their place.
			accept(t, r, r.room(testGroup), content(textPart(user), markup(tc.mode, tc.markup)))
			a := r.lastSend(t)
			if a["parse_mode"] != tc.mode || a["text"] != tc.wantFirst+"\n\n"+tc.markup {
				t.Fatalf("args %v, want %s: the escaped text, a blank line, the markup", a, tc.mode)
			}
			head := strings.TrimSuffix(a["text"].(string), "\n\n"+tc.markup)
			if got, ok := tc.unescape(head); !ok || got != user {
				t.Errorf("the text reads back as %q (clean %v), want %q", got, ok, user)
			}
			accept(t, r, r.room(testGroup), content(markup(tc.mode, tc.markup), textPart(user)))
			if a := r.lastSend(t); a["text"] != tc.markup+"\n\n"+tc.wantFirst {
				t.Errorf("text %q, want the markup then the escaped text", a["text"])
			}
		})
	}
	// Markup alone is the host's, sent as written under its mode.
	accept(t, r, r.room(testGroup), content(markup(ParseModeHTML, "<i>only</i>")))
	if a := r.lastSend(t); a["text"] != "<i>only</i>" || a["parse_mode"] != "HTML" {
		t.Errorf("args %v", a)
	}
	// Two markup parts of one mode join in order.
	accept(t, r, r.room(testGroup), content(markup(ParseModeMarkdownV2, "*a*"), markup(ParseModeMarkdownV2, "_b_")))
	if a := r.lastSend(t); a["text"] != "*a*\n\n_b_" {
		t.Errorf("args %v", a)
	}
}

func TestMarkupIsChecked(t *testing.T) {
	r := newRig(t)
	for name, tc := range map[string]struct {
		c    model.Content
		code model.ErrorCode
	}{
		"two modes":       {content(markup(ParseModeHTML, "<b>a</b>"), markup(ParseModeMarkdownV2, "*b*")), model.Unsupported},
		"legacy Markdown": {content(markup("Markdown", "*a*")), model.Unsupported},
		"no mode":         {content(markup("", "*a*")), model.Unsupported},
		"no text":         {content(markup(ParseModeHTML, "")), model.Validation},
		"unknown field": {content(model.Part{Kind: model.PartStructured, Structured: &model.Extension{
			Name: MarkupSchema, Version: MarkupVersion, Data: []byte(`{"parse_mode":"HTML","text":"x","entities":[]}`)}}), model.Validation},
		"not json": {content(model.Part{Kind: model.PartStructured, Structured: &model.Extension{
			Name: MarkupSchema, Version: MarkupVersion, Data: []byte(`<b>`)}}), model.Validation},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := r.send(t, r.room(testGroup), tc.c)
			wantRejected(t, out, err, tc.code)
			if n := len(r.callsOf("sendMessage")); n != 0 {
				t.Fatalf("%d calls reached Telegram", n)
			}
		})
	}
}

func TestKeyboard(t *testing.T) {
	r := newRig(t)
	good := `{"inline_keyboard":[[{"text":"Approve","callback_data":"approve:42"},{"text":"Docs","url":"https://example.com"}],[{"text":"Copy","copy_text":{"text":"x"}}]]}`
	accept(t, r, r.room(testGroup), content(textPart("Approve?"), keyboard(good)))
	a := r.lastSend(t)
	if a["text"] != "Approve?" {
		t.Errorf("text %v", a["text"])
	}
	got, _ := json.Marshal(a["reply_markup"])
	var want, have any
	_ = json.Unmarshal([]byte(good), &want)
	_ = json.Unmarshal(got, &have)
	if jsonOf(want) != jsonOf(have) {
		t.Errorf("reply_markup %s, want %s as given", got, good)
	}
	// A callback button's data is "1-64 bytes": 64 fit, 65 do not, and bytes
	// are not characters.
	b64 := `{"inline_keyboard":[[{"text":"x","callback_data":"` + strings.Repeat("a", 64) + `"}]]}`
	accept(t, r, r.room(testGroup), content(textPart("x"), keyboard(b64)))
	for name, raw := range map[string]string{
		"65 bytes":          `{"inline_keyboard":[[{"text":"x","callback_data":"` + strings.Repeat("a", 65) + `"}]]}`,
		"33 two-byte runes": `{"inline_keyboard":[[{"text":"x","callback_data":"` + strings.Repeat("é", 33) + `"}]]}`,
		"empty data":        `{"inline_keyboard":[[{"text":"x","callback_data":""}]]}`,
		"no buttons":        `{"inline_keyboard":[]}`,
		"empty row":         `{"inline_keyboard":[[]]}`,
		"no text":           `{"inline_keyboard":[[{"callback_data":"a"}]]}`,
		"no action":         `{"inline_keyboard":[[{"text":"x"}]]}`,
		"two actions":       `{"inline_keyboard":[[{"text":"x","callback_data":"a","url":"https://e.x"}]]}`,
		"not an object":     `[[{"text":"x","callback_data":"a"}]]`,
		"not json":          `inline`,
	} {
		t.Run(name, func(t *testing.T) {
			before := len(r.callsOf("sendMessage"))
			out, err := r.send(t, r.room(testGroup), content(textPart("x"), keyboard(raw)))
			wantRejected(t, out, err, model.Validation)
			if len(r.callsOf("sendMessage")) != before {
				t.Fatal("a call reached Telegram")
			}
		})
	}
	out, err := r.send(t, r.room(testGroup), content(textPart("x"), keyboard(good), keyboard(good)))
	wantRejected(t, out, err, model.Unsupported)
	// A keyboard is not content: with no text there is nothing to send.
	out, err = r.send(t, r.room(testGroup), content(keyboard(good)))
	wantRejected(t, out, err, model.Validation)
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

var _ = adapter.Outcome{}
