package microsoft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"hop.top/uma/testing/xrrtest"
)

// valueForm is one spelling of a recorded value.
type valueForm struct{ kind, text string }

var percentHex = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// formsOf lists the spellings Graph writes a value in: as given; with "@"
// and other reserved characters percent-encoded, as in the mailbox of an
// @odata.context or a paging link (users('alice%40example.com')); encoded
// twice (%2540), as a link embedded in another link's query is; and the
// lower-case hex of both. The adapter itself puts the address in a path
// with url.PathEscape (which keeps "@") and never encodes one twice; the
// encoded forms come from Graph's answers.
func formsOf(v string) []valueForm {
	if v == "" {
		return nil
	}
	enc := url.QueryEscape(v)
	enc2 := url.QueryEscape(enc)
	low := func(s string) string { return percentHex.ReplaceAllStringFunc(s, strings.ToLower) }
	var out []valueForm
	seen := map[string]bool{}
	for _, f := range []valueForm{{"literal", v}, {"percent-encoded", enc}, {"double-encoded", enc2}, {"percent-encoded lower-case", low(enc)}, {"double-encoded lower-case", low(enc2)}} {
		if !seen[f.text] {
			seen[f.text] = true
			out = append(out, f)
		}
	}
	return out
}

// valueForms is formsOf's spellings only.
func valueForms(v string) []string {
	var out []string
	for _, f := range formsOf(v) {
		out = append(out, f.text)
	}
	return out
}

// residue lists the values that survive in data, in any form and any
// letter case (addresses are case-insensitive, and Graph may change case or
// the hex digits of an escape). It names a value by position, never by
// content, so a failing recording does not print the secret it leaked.
func residue(data []byte, values ...string) []string {
	hay := strings.ToLower(string(data))
	var found []string
	for i, v := range values {
		for _, f := range formsOf(v) {
			if strings.Contains(hay, strings.ToLower(f.text)) {
				found = append(found, fmt.Sprintf("value %d survives %s", i+1, f.kind))
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
		for _, r := range residue(data, values...) {
			t.Errorf("%s: %s", filepath.Base(file), r)
		}
	}
}

// The residue check sees every spelling of a value, so a recording that
// leaves one behind fails.
func TestResidueSeesEncodedForms(t *testing.T) {
	const addr = "alice.real@contoso-real.test"
	leaks := map[string]string{
		"literal":            `{"from":{"address":"alice.real@contoso-real.test"}}`,
		"percent-encoded":    `{"@odata.context":"https://graph.microsoft.com/v1.0/$metadata#users('alice.real%40contoso-real.test')/messages/$entity"}`,
		"lower-case hex":     `users/alice.real%40contoso-real.test`,
		"upper-case address": `users('ALICE.REAL%40Contoso-Real.test')`,
		"double-encoded":     `{"@odata.nextLink":"https://h/v1.0/x?$filter=alice.real%2540contoso-real.test"}`,
		"double lower-case":  `x?q=alice.real%2540contoso-real.test`,
	}
	for name, body := range leaks {
		if got := residue([]byte(body), addr); len(got) == 0 {
			t.Errorf("%s: residue missed %s", name, body)
		}
	}
	for _, clean := range []string{`users('MAILBOX')/messages`, `{"from":"PEER"}`, `alice.real contoso-real.test`} {
		if got := residue([]byte(clean), addr); len(got) != 0 {
			t.Errorf("residue flagged %q: %v", clean, got)
		}
	}
	if got := residue([]byte(`users('alice.real%40contoso-real.test')`), addr); strings.Contains(strings.Join(got, ";"), "alice") {
		t.Errorf("residue names the value: %v", got)
	}
}

// encodingAnswers turns Graph's answers into the shapes real Graph writes:
// an @odata.context naming the mailbox with "@" percent-encoded, paging
// links with the mailbox encoded in the path, and a delta link embedding
// the encoded mailbox in its query, encoded again. The fake keeps "@" literal.
type encodingAnswers struct {
	next    http.RoundTripper
	mailbox string
}

func (e encodingAnswers) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := e.next.RoundTrip(req)
	if err != nil || !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return resp, err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if json.Unmarshal(b, &obj) == nil && obj != nil && obj["error"] == nil {
		enc := url.QueryEscape(e.mailbox)
		obj["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#users('" + enc + "')/mailFolders('inbox')/messages(id)"
		for _, k := range []string{"@odata.nextLink", "@odata.deltaLink"} {
			if s, ok := obj[k].(string); ok && s != "" {
				s = strings.ReplaceAll(s, "/users/"+e.mailbox+"/", "/users/"+enc+"/")
				if k == "@odata.deltaLink" {
					s += "&$filter=" + url.QueryEscape("from/emailAddress/address eq '"+enc+"'")
				}
				obj[k] = s
			}
		}
		b, _ = json.Marshal(obj)
	}
	resp.Body, resp.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	return resp, nil
}

// Graph answers with the mailbox percent-encoded in @odata.context and in
// paging links. A recording over answers shaped that way keeps no real
// address in any form, and replays, following the same links.
func TestRecordedScrubCoversEncodedAddresses(t *testing.T) {
	const mailbox, peer = "alice.real@contoso-real.test", "bob.real@fabrikam-real.test"
	g := newFakeGraph(t)
	g.mailbox(mailbox, testChan)
	received := time.Date(2026, 10, 11, 0, 30, 0, 0, time.UTC)
	for _, id := range []string{"in-1", "in-2", "in-3"} {
		g.deliver(mailbox, gMessage{
			ID: id, InternetMessageID: "<" + id + "@fabrikam-real.test>", ReceivedDateTime: received, Subject: "hello",
			From:         &gRecipient{EmailAddress: gEmailAddress{Address: peer, Name: "Peer"}},
			ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: mailbox}}},
			Body:         &gItemBody{ContentType: "text", Content: "hi"},
		})
	}
	dir := t.TempDir()
	pageOf := func(o *Options) { o.PageSize = 1 }
	record := append(recordingOptions(mailbox, peer), xrrtest.WithMode(xrrtest.ModeRecord),
		xrrtest.WithTransport(encodingAnswers{next: g.srv.Client().Transport, mailbox: mailbox}))
	recordedScenario(t, xrrtest.NewClient(t, dir, record...), g.endpoint(), mailbox, peer, "tok-1", pageOf)
	assertNoResidue(t, dir, mailbox, peer, "tok-1")

	// The scenario really met the encoded spelling: the cassettes hold the
	// placeholder where Graph wrote the encoded address.
	var all []byte
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range files {
		b, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	for _, want := range []*regexp.Regexp{regexp.MustCompile(`\$metadata#users\(''?MAILBOX''?\)`), regexp.MustCompile(`"@odata.nextLink":"[^"]*/users/MAILBOX/`)} {
		if !want.Match(all) {
			t.Errorf("cassettes lack %s: the encoded answers were not recorded", want)
		}
	}

	// Replay follows the restored links and finds every recorded request.
	g.srv.Close()
	replay := append(recordingOptions(mailbox, peer), xrrtest.WithMode(xrrtest.ModeReplay))
	recordedScenario(t, xrrtest.NewClient(t, dir, replay...), g.endpoint(), mailbox, peer, "tok-1", pageOf)
}
