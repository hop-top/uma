package gitlab

import (
	"strings"
	"testing"
	"time"
)

func TestEscapeQuickActions(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                  "plain",
		"/close":                 `\/close`,
		"a\n/label ~bug\nb":      "a\n" + `\/label ~bug` + "\nb",
		"  /assign @x":           `  \/assign @x`,
		"\t/Close":               "\t" + `\/Close`,
		"a /close":               "a /close",
		"/":                      "/",
		"//comment":              "//comment",
		"/ path":                 "/ path",
		"see https://x/y":        "see https://x/y",
		"/tmp/log is a path too": `\/tmp/log is a path too`,
	} {
		if got := escapeQuickActions(in); got != want {
			t.Errorf("escapeQuickActions(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTime(t *testing.T) {
	want := time.Date(2026, 10, 11, 10, 5, 3, 0, time.UTC)
	for _, s := range []string{
		"2026-10-11T10:05:03Z", "2026-10-11T10:05:03.000Z", "2026-10-11 10:05:03 UTC",
		"2026-10-11 12:05:03 +0200", "2026-10-11T12:05:03+02:00", "2026-10-11 10:05:03",
	} {
		if got := parseTime(s); !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, want %v", s, got, want)
		}
	}
	for _, s := range []string{"", "yesterday", "2026-13-40T00:00:00Z"} {
		if got := parseTime(s); !got.IsZero() {
			t.Errorf("parseTime(%q) = %v, want zero", s, got)
		}
	}
	if got := parseTime("2026-10-11T10:05:03.250Z"); got.Nanosecond() != 250_000_000 {
		t.Errorf("fraction lost: %v", got)
	}
}

func TestVerified(t *testing.T) {
	secret := []byte("s3cret-token")
	for token, want := range map[string]bool{
		"s3cret-token": true, "": false, "s3cret-toke": false, "s3cret-token ": false, "S3CRET-TOKEN": false,
		strings.Repeat("s3cret-token", 100): false,
	} {
		if got := verified(token, secret); got != want {
			t.Errorf("verified(%q) = %v, want %v", token, got, want)
		}
	}
	if verified("", nil) || verified("x", nil) || verified("x", []byte{}) {
		t.Error("a delivery verified without a secret")
	}
}

func TestParseMarker(t *testing.T) {
	op, att := "00000000-0000-7000-8000-00000000a0a1", "00000000-0000-7000-8000-00000000b0b1"
	body := "text\n\n<!-- uma:" + op + ":" + att + " -->\n"
	o, a, rest, ok := parseMarker(body)
	if !ok || string(o) != op || string(a) != att || rest != "text" {
		t.Fatalf("parseMarker = %q %q %q %v", o, a, rest, ok)
	}
	for _, bad := range []string{"text", "<!-- uma:x -->", "<!-- uma:" + op + ":" + att + " --> trailing", "<!-- uma:" + op + ":not an id -->"} {
		if _, _, rest, ok := parseMarker(bad); ok || rest != bad {
			t.Errorf("parseMarker(%q) = %q %v, want no marker", bad, rest, ok)
		}
	}
}

func TestValidFullName(t *testing.T) {
	for s, want := range map[string]bool{
		"group/project": true, "group/sub/group/project": true, "a.b/c-d_e": true,
		"project": false, "": false, "a//b": false, "a/..": false, "a/./b": false, "a/b c": false, "a/b?x": false, "/a/b": false,
	} {
		if got := validFullName(s); got != want {
			t.Errorf("validFullName(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestBotUsername(t *testing.T) {
	for u, want := range map[string]bool{
		"project_123_bot_4ffca233d8298ea1": true, "group_9_bot_abc": true, "project_12_bot": true,
		"alice": false, "project_x_bot_1": false, "my_project_1_bot_a": false, "": false,
	} {
		if got := (userView{username: u}).bot(); got != want {
			t.Errorf("bot(%q) = %v, want %v", u, got, want)
		}
	}
}
