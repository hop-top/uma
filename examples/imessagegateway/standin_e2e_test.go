package imessagegateway_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/gateway/imessage"
	"hop.top/uma/testing/xrrtest"
)

// The recorded scenario against stand-ins of both backends: recorded into a
// temporary directory, then replayed with the stand-in closed. It proves
// that a recording replays, that the cassettes and the captured webhook keep
// the recorded identifiers out, and that the scenario holds; the stand-ins
// are not evidence about the real services. The real recordings replace
// them (docs/recording/imessage.md).

// recordValues are the identifiers of the stand-in recording, different from
// the replay values so that a leak shows.
var recordValues = struct{ from, to, account, key, secret, password, token string }{ // #nosec G101 -- fixture values
	from: "+15550109001", to: "Recorder@Example.com", account: "mac-owner@example.com",
	key: "bl_record_key_value", secret: "whsec_record0123456789abcdef0123456789",
	password: "record-password-value", token: "record-webhook-token-0123456789",
}

// standInReply posts a reply webhook to the gateway, as the backend does
// when the recipient answers.
func standInReply(t *testing.T, url string, body []byte, header http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway webhook answered %d", resp.StatusCode)
	}
}

// noLeaks fails when a file under dir holds any of values.
func noLeaks(t *testing.T, dir string, values ...string) {
	t.Helper()
	n := 0
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		b, err := fs.ReadFile(root.FS(), path)
		if err != nil {
			return err
		}
		for _, v := range values {
			if v != "" && bytes.Contains(bytes.ToLower(b), bytes.ToLower([]byte(v))) {
				t.Errorf("%s holds the recorded value %q", filepath.Base(path), v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("nothing recorded")
	}
}

func TestStandInRoundTripBlooio(t *testing.T) {
	sb := newStandInBlooio(t)
	host := strings.TrimPrefix(sb.srv.URL, "http://")
	dir := t.TempDir()
	cassettes, fixture := filepath.Join(dir, "cassettes"), filepath.Join(dir, "webhooks", "reply.json")
	extra := []xrrtest.Option{xrrtest.WithPlaceholder(host, "PROVIDER_HOST")}
	rv := recordValues
	to := handle(rv.to)

	record := blooioSpec(sb.srv.URL+"/v4", rv.key, rv.secret, [2]string{rv.from, to}, [2]string{replayBlooioFrom, replayRecipient})
	runE2E(t, record, e2eRun{
		mode: xrrtest.ModeRecord, cassettes: cassettes, fixture: fixture, extra: extra,
		reply: func(t *testing.T, webhookURL string) {
			body := strings.NewReplacer("alice@example.com", rv.to, "+15550100002", rv.from).Replace(blooioReceived)
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			standInReply(t, webhookURL, []byte(body), http.Header{imessage.BlooioSignatureHeader: {"t=" + ts + ",v1=" + imessage.Sign([]byte(rv.secret), ts, []byte(body))}})
		},
	})
	if n := len(sb.reqs); n != 1 {
		t.Fatalf("the stand-in took %d sends, want 1", n)
	}
	noLeaks(t, dir, rv.from, digits(rv.from), rv.to, rv.key, rv.secret, "org_test_real")

	sb.srv.Close()
	replay := blooioSpec(sb.srv.URL+"/v4", replayBlooioKey, replayBlooioSecret,
		[2]string{replayBlooioFrom, replayRecipient}, [2]string{replayBlooioFrom, replayRecipient})
	runE2E(t, replay, e2eRun{mode: xrrtest.ModeReplay, cassettes: cassettes, fixture: fixture, extra: extra})
}

func TestStandInRoundTripBlueBubbles(t *testing.T) {
	sb := newStandInBlueBubbles(t)
	dir := t.TempDir()
	cassettes, fixture := filepath.Join(dir, "cassettes"), filepath.Join(dir, "webhooks", "reply.json")
	rv := recordValues
	to := handle(rv.to)

	record := bbSpec(sb.srv.URL, rv.password, rv.token, [2]string{rv.account, to}, [2]string{replayBBAccount, replayRecipient})
	runE2E(t, record, e2eRun{
		mode: xrrtest.ModeRecord, cassettes: cassettes, fixture: fixture,
		reply: func(t *testing.T, webhookURL string) {
			var e map[string]any
			must(t, json.Unmarshal([]byte(bbNewMessage), &e))
			d := e["data"].(map[string]any)
			d["handle"].(map[string]any)["address"] = to
			d["chats"] = []any{map[string]any{"guid": "iMessage;-;" + to, "chatIdentifier": to, "displayName": "Recorder Person"}}
			body, _ := json.Marshal(e)
			standInReply(t, webhookURL, body, nil)
		},
	})
	if n := len(sb.reqs); n != 1 {
		t.Fatalf("the stand-in took %d sends, want 1", n)
	}
	noLeaks(t, dir, rv.account, rv.to, rv.password, rv.token, "Recorder Person", strings.TrimPrefix(sb.srv.URL, "http://"))

	sb.srv.Close()
	replay := bbSpec(sb.srv.URL, replayBBPassword, replayWebhookToken,
		[2]string{replayBBAccount, replayRecipient}, [2]string{replayBBAccount, replayRecipient})
	runE2E(t, replay, e2eRun{mode: xrrtest.ModeReplay, cassettes: cassettes, fixture: fixture})
}
