package imessagegateway_test

import (
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/gateway/imessage"
	"hop.top/uma/testing/xrrtest"
)

// Recorded exchanges and the captured reply webhook, per backend. See
// docs/recording/imessage.md to record them.
const (
	blooioCassettes = "testdata/cassettes/blooio"
	blooioFixture   = "testdata/webhooks/blooio/reply.json"
	bbCassettes     = "testdata/cassettes/bluebubbles"
	bbFixture       = "testdata/webhooks/bluebubbles/reply.json"
)

// Replay values: what stands for each recorded identifier when the
// recording variables are unset. Reserved numbers and example.com only.
const (
	replayRecipient     = "+15550100001"
	replayBlooioFrom    = "+15550100002"
	replayBlooioKey     = "bl_replay_key"                          // #nosec G101 -- replay value
	replayBlooioSecret  = "whsec_replay0123456789abcdef0123456789" // #nosec G101 -- replay value
	replayBBURL         = "http://bluebubbles.example.com:1234"
	replayBBPassword    = "replay-password" // #nosec G101 -- replay value
	replayBBAccount     = "uma-test@example.com"
	replayWebhookToken  = "replay-webhook-token-0123456789" // #nosec G101 -- replay value
	placeholderTo       = "RECIPIENT_HANDLE"
	placeholderToDigits = "RECIPIENTDIGITS"
)

// recording reads the recording variables: each must be set in record
// mode, and takes its replay value otherwise.
type recording struct {
	t    *testing.T
	mode xrrtest.Mode
}

func newRecording(t *testing.T, cassettes, backend string) recording {
	t.Helper()
	mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode))
	if mode == "" {
		mode = xrrtest.ModeReplay
	}
	if _, err := os.Stat(cassettes); err != nil && mode != xrrtest.ModeRecord {
		t.Skip("no recorded " + backend + " cassettes; record with XRR_MODE=record (docs/recording/imessage.md)")
	}
	return recording{t: t, mode: mode}
}

func (r recording) env(name, replay string) string {
	r.t.Helper()
	if v := os.Getenv(name); v != "" && r.mode == xrrtest.ModeRecord {
		return v
	}
	if r.mode == xrrtest.ModeRecord {
		r.t.Fatalf("recording needs %s", name)
	}
	return replay
}

// handle is a recipient as uma keeps it: an email address lowercased.
func handle(s string) string {
	if strings.Contains(s, "@") {
		return strings.ToLower(s)
	}
	return s
}

func digits(s string) string { return strings.TrimPrefix(s, "+") }

// personReply asks the person holding the recipient's device to answer.
func personReply(backend string) func(*testing.T, string) {
	return func(t *testing.T, webhookURL string) {
		t.Helper()
		t.Logf("reply %q to the message from %s now; waiting up to %s for its webhook at %s",
			e2eReply, backend, replyWait, webhookURL)
	}
}

// blooioSpec is the scenario over Blooio. real are the values the run
// uses, replay what the cassettes and the fixture hold instead.
func blooioSpec(baseURL, apiKey, secret string, real, replay [2]string) e2eSpec {
	from, to := real[0], real[1]
	return e2eSpec{
		name: "blooio", recipient: to, address: from,
		backend: func(c *http.Client) imessage.Backend {
			be, err := imessage.NewBlooio(imessage.BlooioOptions{BaseURL: baseURL, APIKey: apiKey, From: from, WebhookSecret: []byte(secret), Client: c})
			if err != nil {
				panic(err)
			}
			return be
		},
		signReplay: func(body []byte) http.Header {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			return http.Header{imessage.BlooioSignatureHeader: {"t=" + ts + ",v1=" + imessage.Sign([]byte(secret), ts, body)}}
		},
		placeholders: []xrrtest.Option{
			xrrtest.WithPlaceholder(from, "SENDER_NUMBER"),
			xrrtest.WithPlaceholder(to, placeholderTo),
			xrrtest.WithPlaceholder(digits(from), "SENDERDIGITS"),
			xrrtest.WithPlaceholder(digits(to), placeholderToDigits),
		},
		fixtureValues: [][2]string{
			{from, replay[0]}, {to, replay[1]}, {digits(from), digits(replay[0])}, {digits(to), digits(replay[1])},
		},
	}
}

// bbSpec is the scenario over BlueBubbles. real and replay are the account
// and the recipient.
func bbSpec(serverURL, password, token string, real, replay [2]string) e2eSpec {
	account, to := real[0], real[1]
	u, _ := url.Parse(serverURL)
	return e2eSpec{
		name: "bluebubbles", recipient: to, address: account, webhookToken: token,
		backend: func(c *http.Client) imessage.Backend {
			be, err := imessage.NewBlueBubbles(imessage.BlueBubblesOptions{URL: serverURL, Password: password, Account: account, Client: c})
			if err != nil {
				panic(err)
			}
			return be
		},
		placeholders: []xrrtest.Option{
			xrrtest.WithPlaceholder(password, "BLUEBUBBLES_PASSWORD"),
			xrrtest.WithPlaceholder(u.Host, "BLUEBUBBLES_HOST"),
			xrrtest.WithPlaceholder(account, "BLUEBUBBLES_ACCOUNT"),
			xrrtest.WithPlaceholder(to, placeholderTo),
			xrrtest.WithPlaceholder(digits(to), placeholderToDigits),
		},
		fixtureValues: [][2]string{{account, replay[0]}, {to, replay[1]}, {digits(to), digits(replay[1])}},
	}
}

// TestRecordedBlooio runs the end-to-end scenario through the gateway over
// Blooio: one message to IMESSAGE_TO, the repeated callback and the lookup,
// and the recipient's reply, which the person holding the recipient's
// device sends while the test waits. It skips until cassettes exist. To
// record (docs/recording/imessage.md):
//
//	BLOOIO_API_KEY=... BLOOIO_FROM=+1... BLOOIO_WEBHOOK_SECRET=whsec_... IMESSAGE_TO=+1... \
//	  IMESSAGE_GATEWAY_LISTEN=127.0.0.1:8787 XRR_MODE=record go test -count=1 -v -run TestRecordedBlooio
//
// with Blooio's webhook pointed at <public URL of the listen address>/webhook.
func TestRecordedBlooio(t *testing.T) {
	r := newRecording(t, blooioCassettes, "Blooio")
	from := r.env("BLOOIO_FROM", replayBlooioFrom)
	to := handle(r.env("IMESSAGE_TO", replayRecipient))
	key := r.env("BLOOIO_API_KEY", replayBlooioKey)
	secret := r.env("BLOOIO_WEBHOOK_SECRET", replayBlooioSecret)
	listen := ""
	if r.mode == xrrtest.ModeRecord {
		listen = r.env("IMESSAGE_GATEWAY_LISTEN", "")
	}
	spec := blooioSpec(imessage.BlooioBaseURL, key, secret, [2]string{from, to}, [2]string{replayBlooioFrom, replayRecipient})
	runE2E(t, spec, e2eRun{
		mode: r.mode, cassettes: blooioCassettes, fixture: blooioFixture, listen: listen,
		reply: personReply("Blooio"),
	})
}

// TestRecordedBlueBubbles runs the scenario over BlueBubbles Server on the
// owner's Mac: AppleScript send (no Private API) to IMESSAGE_TO, then the
// recipient's reply. It skips until cassettes exist. To record
// (docs/recording/imessage.md):
//
//	BLUEBUBBLES_URL=http://<mac>:1234 BLUEBUBBLES_PASSWORD=... BLUEBUBBLES_ACCOUNT=<apple id> \
//	  BLUEBUBBLES_WEBHOOK_TOKEN=<random> IMESSAGE_TO=+1... IMESSAGE_GATEWAY_LISTEN=<addr:port> \
//	  XRR_MODE=record go test -count=1 -v -run TestRecordedBlueBubbles
//
// with a BlueBubbles webhook (new messages) at http://<listen>/webhook/<token>.
func TestRecordedBlueBubbles(t *testing.T) {
	r := newRecording(t, bbCassettes, "BlueBubbles")
	serverURL := r.env("BLUEBUBBLES_URL", replayBBURL)
	password := r.env("BLUEBUBBLES_PASSWORD", replayBBPassword)
	account := handle(r.env("BLUEBUBBLES_ACCOUNT", replayBBAccount))
	token := r.env("BLUEBUBBLES_WEBHOOK_TOKEN", replayWebhookToken)
	to := handle(r.env("IMESSAGE_TO", replayRecipient))
	listen := ""
	if r.mode == xrrtest.ModeRecord {
		listen = r.env("IMESSAGE_GATEWAY_LISTEN", "")
	}
	spec := bbSpec(serverURL, password, token, [2]string{account, to}, [2]string{replayBBAccount, replayRecipient})
	runE2E(t, spec, e2eRun{
		mode: r.mode, cassettes: bbCassettes, fixture: bbFixture, listen: listen,
		reply: personReply("BlueBubbles"),
	})
}
