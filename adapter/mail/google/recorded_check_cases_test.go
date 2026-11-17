package google

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestResidueCheckSeesEveryForm pins the check itself: an identity planted
// in each form a cassette can hold it must be found, and clean content
// passes.
func TestResidueCheckSeesEveryForm(t *testing.T) {
	ids := realIdentities()
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	msg := func(extra string) string { return "From: <x@outside.example>\r\nSubject: s\r\n" + extra }
	for name, content := range map[string]string{
		"plain":                   "emailAddress: " + realMailbox,
		"lower case":              "emailAddress: " + strings.ToLower(realMailbox),
		"URL escaped":             "users/" + url.PathEscape(strings.ReplaceAll(realMailbox, "@", "%40")),
		"JSON escaped":            `"snippet":"` + strings.ReplaceAll(realPeer, "@", `\u0040`) + `"`,
		"domain only":             "q=rfc822msgid:abc@" + addrDomain(realMailbox),
		"base64url raw":           `{"raw":"` + enc(msg("To: <"+realPeer+">\r\n\r\nbody")) + `"}`,
		"folded header in raw":    `{"raw":"` + enc(msg("To: a,\r\n <"+realPeer+">\r\n\r\nbody")) + `"}`,
		"address folded in raw":   `{"raw":"` + enc(msg("To: <charles@difference-\r\n engine.example>\r\n\r\nbody")) + `"}`,
		"quoted-printable in raw": `{"raw":"` + enc(msg("Content-Transfer-Encoding: quoted-printable\r\n\r\ncharles=40difference-=\r\nengine.example")) + `"}`,
		"base64 body in raw":      `{"raw":"` + enc(msg("Content-Transfer-Encoding: base64\r\n\r\n"+b64Body("write to "+realPeer+" please, and a tail so that the base64 wraps at least once over"))) + `"}`,
		"encoded word in raw":     `{"raw":"` + enc(msg("Subject: =?UTF-8?Q?x_charles=40difference-engine.example?=\r\n\r\nbody")) + `"}`,
		"nested raw in raw":       `{"raw":"` + enc(msg("Content-Type: message/rfc822\r\nContent-Transfer-Encoding: base64\r\n\r\n"+b64Body(msg("To: <"+realPeer+">\r\n\r\nx")))) + `"}`,
	} {
		if len(residueIn([]byte(content), ids)) == 0 {
			t.Errorf("%s: residue not found in %q", name, content)
		}
	}
	for name, content := range map[string]string{
		"placeholders": `{"raw":"` + enc(msg("To: <PEER@peer.invalid>\r\n\r\nMAILBOX@mailbox.invalid")) + `","emailAddress":"MAILBOX"}`,
		"other mail":   `{"raw":"` + enc(msg("To: <someone@outside.example>\r\n\r\nbody")) + `"}`,
		"empty":        "",
	} {
		if r := residueIn([]byte(content), ids); len(r) > 0 {
			t.Errorf("%s: false positive %v", name, r)
		}
	}
	if len(residueIn([]byte("charles@difference-engine.example"), nil)) != 0 {
		t.Error("no identities, no residue")
	}
}

func TestAssertNoResidueScansCassetteFiles(t *testing.T) {
	dir := t.TempDir()
	ids := realIdentities()
	clean := `request: {url: "https://host/users/MAILBOX/messages"}`
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(clean), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := &recordingTB{TB: t}
	assertNoResidue(spy, dir, ids)
	if len(spy.errors) != 0 {
		t.Fatalf("clean cassette flagged: %q", spy.errors)
	}
	leak := `response: {body: "{\"raw\":\"` + base64.RawURLEncoding.EncodeToString([]byte("To: <"+realPeer+">\r\n\r\n")) + `\"}"}`
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(leak), 0o600); err != nil {
		t.Fatal(err)
	}
	assertNoResidue(spy, dir, ids)
	if len(spy.errors) != 1 || !strings.Contains(spy.errors[0], "b.yaml") || !strings.Contains(spy.errors[0], "peer address") {
		t.Fatalf("errors = %q", spy.errors)
	}
	if strings.Contains(spy.errors[0], realPeer) {
		t.Fatal("the failure message leaks the address")
	}
}

// recordingTB collects errors instead of failing the test.
type recordingTB struct {
	testing.TB
	errors []string
}

func (r *recordingTB) Helper() {}
func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

type roundTripRecorder struct{ got []*http.Request }

func (r *roundTripRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = append(r.got, req)
	return &http.Response{StatusCode: 200, Body: http.NoBody, Request: req}, nil
}

func TestSyncBoundLimitsOnlyTheInboxSearch(t *testing.T) {
	until := testNow.Unix()
	after := testNow.Add(-time.Hour).Unix()
	base := "https://gmail.googleapis.com/gmail/v1/users/me/messages"
	for name, tc := range map[string]struct {
		method, url string
		wantQ       string // "" = unchanged
	}{
		"first sync":   {"GET", base + "?labelIds=INBOX&maxResults=100&q=after:" + itoa(after), "after:" + itoa(after) + " before:" + itoa(until)},
		"second page":  {"GET", base + "?labelIds=INBOX&maxResults=100&pageToken=p2&q=after:" + itoa(after), "after:" + itoa(after) + " before:" + itoa(until)},
		"already set":  {"GET", base + "?labelIds=INBOX&q=after:" + itoa(after) + "+before:5", ""},
		"sent search":  {"GET", base + "?labelIds=SENT&q=rfc822msgid:abc@" + recordedMessageIDDomain, ""},
		"no after":     {"GET", base + "?labelIds=INBOX&q=is:unread", ""},
		"history":      {"GET", "https://gmail.googleapis.com/gmail/v1/users/me/history?labelId=INBOX&startHistoryId=1", ""},
		"message read": {"GET", base + "/m1?format=raw", ""},
		"send":         {"POST", base + "/send", ""},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &roundTripRecorder{}
			req, err := http.NewRequest(tc.method, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			orig := req.URL.String()
			if _, err := (syncBound{next: rec, until: testNow}).RoundTrip(req); err != nil {
				t.Fatal(err)
			}
			if req.URL.String() != orig {
				t.Fatalf("caller's request changed: %s", req.URL)
			}
			sent := rec.got[0].URL
			if tc.wantQ == "" {
				if sent.String() != orig {
					t.Fatalf("request changed: %s", sent)
				}
				return
			}
			if got := sent.Query().Get("q"); got != tc.wantQ {
				t.Fatalf("q = %q, want %q", got, tc.wantQ)
			}
			if sent.Query().Get("labelIds") != "INBOX" || sent.Path != "/gmail/v1/users/me/messages" {
				t.Fatalf("other parts changed: %s", sent)
			}
		})
	}
}

func TestHistoryScopeLimitsOnlyTheHistoryRead(t *testing.T) {
	base := "https://gmail.googleapis.com/gmail/v1/users/me"
	for name, tc := range map[string]struct {
		method, url, want string // want "" = unchanged
	}{
		"history":       {"GET", base + "/history?startHistoryId=1", "SENT"},
		"already set":   {"GET", base + "/history?labelId=INBOX&startHistoryId=1", ""},
		"message list":  {"GET", base + "/messages?labelIds=INBOX", ""},
		"message read":  {"GET", base + "/messages/m1?format=raw", ""},
		"thread modify": {"POST", base + "/threads/t1/modify", ""},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &roundTripRecorder{}
			req, err := http.NewRequest(tc.method, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (historyScope{next: rec}).RoundTrip(req); err != nil {
				t.Fatal(err)
			}
			if req.URL.String() != tc.url {
				t.Fatalf("caller's request changed: %s", req.URL)
			}
			sent := rec.got[0].URL
			if tc.want == "" {
				if sent.String() != tc.url {
					t.Fatalf("request changed: %s", sent)
				}
				return
			}
			if sent.Query().Get("labelId") != tc.want || sent.Query().Get("startHistoryId") != "1" {
				t.Fatalf("request %s", sent)
			}
		})
	}
}
