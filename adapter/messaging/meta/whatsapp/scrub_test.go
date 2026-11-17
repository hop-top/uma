package whatsapp_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/model"
	"hop.top/uma/testing/xrrtest"
)

var percentHex = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

func lowerHex(s string) string { return percentHex.ReplaceAllStringFunc(s, strings.ToLower) }

// phoneForms lists the spellings Meta writes a recipient in: +E.164 as
// sent, percent-encoded (%2B...) and encoded twice, and the digits alone
// (the wa_id of an answer). Only a number is expanded: the replay value
// is the placeholder itself, which has nothing to expand.
func phoneForms(recipient string) []string {
	digits := strings.TrimPrefix(recipient, "+")
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return []string{recipient}
	}
	plus := "+" + digits
	enc := url.QueryEscape(plus)
	enc2 := url.QueryEscape(enc)
	// The digits come last: they are part of every other form.
	return []string{plus, enc, lowerHex(enc), enc2, lowerHex(enc2), digits}
}

// phoneFields are the JSON fields that carry a user's number in the
// answers and webhook payloads the adapter parses: the send answer's
// contacts[].input and contacts[].wa_id, a webhook's contacts[].wa_id,
// messages[].from and messages[].context.from, and a status's
// recipient_id. display_phone_number is the business number.
var phoneFields = map[string]string{
	"input": "RECIPIENT", "wa_id": "RECIPIENT", "recipient_id": "RECIPIENT", "from": "RECIPIENT",
	"display_phone_number": "BUSINESS_PHONE",
}

// scrubPhoneFields replaces the value of every phone-bearing JSON field by
// its placeholder, whatever the value is: Meta may resolve a wa_id that
// differs from the number sent (a normalized or re-prefixed number), which
// no literal placeholder would match.
func scrubPhoneFields(contentType string, body []byte) []byte {
	if !strings.Contains(contentType, "json") {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return body
	}
	v, changed := replacePhones(v, "")
	if !changed {
		return body
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return body
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func replacePhones(v any, key string) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		changed := false
		for k, c := range t {
			if nv, ch := replacePhones(c, k); ch {
				t[k], changed = nv, true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, c := range t {
			if nv, ch := replacePhones(c, key); ch {
				t[i], changed = nv, true
			}
		}
		return t, changed
	case string:
		if p, ok := phoneFields[key]; ok && t != p {
			return p, true
		}
	case json.Number:
		if p, ok := phoneFields[key]; ok {
			return p, true
		}
	}
	return v, false
}

// recordingOptions maps the phone number ID and the recipient, in every
// form, to their placeholders and scrubs the phone-bearing fields of JSON
// bodies by name.
func recordingOptions(pnid, recipient string) []xrrtest.Option {
	opts := []xrrtest.Option{xrrtest.WithPlaceholder(pnid, "PHONE_NUMBER_ID")}
	for _, form := range phoneForms(recipient) {
		opts = append(opts, xrrtest.WithPlaceholder(form, "RECIPIENT"))
	}
	return append(opts, xrrtest.WithScrub(scrubPhoneFields))
}

// residueOf lists the values that survive in data, in any form and letter
// case. It names a value by position, never by content.
func residueOf(data []byte, values ...string) []string {
	hay := strings.ToLower(string(data))
	var found []string
	for i, v := range values {
		forms := []string{v}
		if v != "" && strings.Trim(strings.TrimPrefix(v, "+"), "0123456789") == "" {
			forms = phoneForms(v)
		} else if enc := url.QueryEscape(v); enc != v {
			forms = append(forms, enc, url.QueryEscape(enc))
		}
		for _, f := range forms {
			if f != "" && strings.Contains(hay, strings.ToLower(f)) {
				found = append(found, fmt.Sprintf("value %d survives as %d characters", i+1, len(f)))
			}
		}
	}
	return found
}

// assertNoResidue fails when any value survives in a file under dir.
func assertNoResidue(t testing.TB, dir string, values ...string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no recorded files in %s (%v)", dir, err)
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range residueOf(data, values...) {
			t.Errorf("%s: %s", filepath.Base(file), r)
		}
	}
}

// The residue check sees the recipient as +E.164, as digits alone and
// percent-encoded, whatever the letter case, and flags nothing else.
func TestResidueSeesEveryRecipientForm(t *testing.T) {
	const rcpt = "+15550123456"
	for name, body := range map[string]string{
		"e164":           `{"to":"+15550123456"}`,
		"digits":         `{"contacts":[{"input":"x","wa_id":"15550123456"}]}`,
		"encoded":        `?to=%2B15550123456`,
		"encoded lower":  `?to=%2b15550123456`,
		"double encoded": `?u=%252B15550123456`,
	} {
		if len(residueOf([]byte(body), rcpt)) == 0 {
			t.Errorf("%s: residue missed %s", name, body)
		}
	}
	for _, clean := range []string{`{"to":"RECIPIENT","wa_id":"RECIPIENT"}`, `wamid.HBgLMTY1MDM4Nzk0MzkVAgARGBI0001`, `1555012345`} {
		if got := residueOf([]byte(clean), rcpt); len(got) != 0 {
			t.Errorf("residue flagged %q: %v", clean, got)
		}
	}
	for _, v := range []string{"tok en/+", "plain-token"} {
		if len(residueOf([]byte("Bearer "+v), v)) == 0 {
			t.Errorf("residue missed value %q", v)
		}
	}
}

// Each field the adapter parses from an answer or a webhook that holds a
// user's number is replaced by name, equal to the recipient or not.
func TestScrubPhoneFieldsCoversAnswerAndWebhookShapes(t *testing.T) {
	const digits, other = "15550123456", "5215550987654"
	cases := map[string]string{
		"send answer": `{"messaging_product":"whatsapp","contacts":[{"input":"+15550123456","wa_id":"5215550987654"}],"messages":[{"id":"wamid.A"}]}`,
		"inbound webhook": `{"entry":[{"changes":[{"value":{"metadata":{"display_phone_number":"15550783881","phone_number_id":"106540352242922"},` +
			`"contacts":[{"profile":{"name":"Peer"},"wa_id":"15550123456"}],"messages":[{"from":"15550123456","id":"wamid.B","context":{"from":"15550783881","id":"wamid.A"}}]}}]}]}`,
		"status webhook": `{"entry":[{"changes":[{"value":{"statuses":[{"id":"wamid.A","status":"delivered","recipient_id":"15550123456"}]}}]}]}`,
		"numeric wa_id":  `{"contacts":[{"wa_id":15550123456}]}`,
	}
	for name, body := range cases {
		out := scrubPhoneFields("application/json", []byte(body))
		for _, leak := range []string{digits, other, "15550783881"} {
			if bytes.Contains(out, []byte(leak)) {
				t.Errorf("%s: %s survives in %s", name, leak, out)
			}
		}
		if !json.Valid(out) {
			t.Errorf("%s: scrubbed body is not JSON: %s", name, out)
		}
	}
	// What is not a phone field stays, and non-JSON bodies are untouched.
	out := scrubPhoneFields("application/json", []byte(cases["send answer"]))
	if !bytes.Contains(out, []byte(`"id":"wamid.A"`)) || !bytes.Contains(out, []byte(`"messaging_product":"whatsapp"`)) {
		t.Errorf("scrub changed other fields: %s", out)
	}
	if got := scrubPhoneFields("text/plain", []byte("wa_id 15550123456")); string(got) != "wa_id 15550123456" {
		t.Errorf("non-JSON body changed: %s", got)
	}
}

// A send recorded against a server answering the way Meta does (input as
// sent, wa_id the digits Meta resolved) keeps no real number, in a request,
// an answer or a URL, and replays with the server gone. The recipient and
// a second number the server resolves instead of it (a re-prefixed
// number) both leave no trace.
func TestRecordedScrubKeepsNoRecipientDigits(t *testing.T) {
	for name, resolved := range map[string]string{
		"wa_id equals the number sent": "15550123456",
		"wa_id resolved differently":   "5215550987654",
	} {
		t.Run(name, func(t *testing.T) {
			const recipient = "+15550123456"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					To string `json:"to"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"messaging_product":"whatsapp","contacts":[{"input":%q,"wa_id":%q}],"messages":[{"id":"wamid.HBgLMTU1NTAxMjM0NTYVAgARGBI0001"}]}`, req.To, resolved)
			}))
			defer srv.Close()
			dir := t.TempDir()
			send := func(mode xrrtest.Mode, rcpt string) adapter.Outcome {
				t.Helper()
				opts := append(recordingOptions(testPNID, rcpt), xrrtest.WithMode(mode), xrrtest.WithTransport(srv.Client().Transport))
				client := xrrtest.NewClient(t, dir, opts...)
				inst := openInstance(t, whatsapp.NewFactory(whatsapp.Options{BaseURL: srv.URL}), adapter.Dependencies{HTTP: client, Credentials: newCreds()})
				e := effect("00000000-0000-7000-8000-0000000000f3")
				e.Route.ID = rcpt
				out, err := inst.Send(t.Context(), adapter.SendRequest{Effect: e, Content: textContent("hi")})
				if err != nil || out.State != model.ProviderAccepted {
					t.Fatalf("%s: Send = %+v, %v", mode, out, err)
				}
				if mode == xrrtest.ModeRecord {
					// The number may also ride in a URL, encoded.
					resp, err := client.Get(srv.URL + "/probe?to=" + url.QueryEscape(rcpt) + "&n=" + strings.TrimPrefix(rcpt, "+"))
					if err != nil {
						t.Fatal(err)
					}
					_ = resp.Body.Close()
				}
				return out
			}
			recorded := send(xrrtest.ModeRecord, recipient)
			assertNoResidue(t, dir, testPNID, recipient, resolved)
			var all []byte
			files, _ := filepath.Glob(filepath.Join(dir, "*"))
			for _, f := range files {
				b, err := os.ReadFile(filepath.Clean(f))
				if err != nil {
					t.Fatal(err)
				}
				all = append(all, b...)
			}
			if !bytes.Contains(all, []byte("RECIPIENT")) || !bytes.Contains(all, []byte("probe?n=RECIPIENT&to=RECIPIENT")) {
				t.Errorf("cassettes hold no placeholder where the number was written:\n%s", all)
			}
			srv.Close()
			// Replay with the placeholder as the recipient, as the recorded
			// tests do with no recording variables set.
			if replayed := send(xrrtest.ModeReplay, "RECIPIENT"); replayed.References[0] != recorded.References[0] {
				t.Fatalf("replayed %+v, recorded %+v", replayed, recorded)
			}
		})
	}
}
