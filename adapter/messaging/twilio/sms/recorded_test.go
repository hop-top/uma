package sms_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/adapter/messaging/twilio/sms"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// cassettes holds recorded Messages API exchanges, one directory per
// scenario: replay does not see credentials, so exchanges differing only by
// credential must not share a directory.
const cassettes = "testdata/cassettes/twilio"

// Recording runs against a live Twilio account named by the environment:
//
//	XRR_MODE=record TWILIO_ACCOUNT_SID=AC... TWILIO_AUTH_TOKEN=... \
//	TWILIO_FROM=+1... go test ./sms -run 'TestRecorded'
//
// TWILIO_FROM is a number the account owns and TWILIO_TO a phone you own.
// The account SID and every number are replaced by placeholders before anything is written; review
// testdata/cassettes before committing. Replay (the default) needs no
// credentials.
func liveAccount() recordedAccount {
	return recordedAccount{
		sid:   envOr("TWILIO_ACCOUNT_SID", "AC00000000000000000000000000000000"),
		token: envOr("TWILIO_AUTH_TOKEN", "replay-token"),
		from:  envOr("TWILIO_FROM", senderNumber),
		to:    envOr("TWILIO_TO", "+15558675310"),
	}
}

// recordedClock is the clock of recorded instances: before any recording,
// so a reconcile listing never excludes a message by age.
var recordedClock = portstest.NewFixedClock(time.Unix(1_760_000_000, 0))

// TestRecordedSendAndFetch sends one SMS from TWILIO_FROM to TWILIO_TO and
// fetches it back. It sends a real message, so recording it also needs
// TWILIO_TO, a phone you own; replay skips until its cassettes exist.
func TestRecordedSendAndFetch(t *testing.T) {
	dir := filepath.Join(cassettes, "send-and-fetch")
	mode := os.Getenv(xrrtest.EnvMode)
	if mode == "" || mode == string(xrrtest.ModeReplay) {
		if files, _ := filepath.Glob(filepath.Join(dir, "*.yaml")); len(files) == 0 {
			t.Skip("no recorded send-and-fetch cassettes yet")
		}
	}
	a := liveAccount()
	sendAndFetch(t, a.client(t, dir), "", a)
}

// TestRecordedReconcileFindsSent sends one SMS, then reconciles that
// attempt as if the answer had been lost: the Messages list holds the
// message the send created, so reconcile finds it and reports it accepted
// (or delivered, if the list is already that far) with the same SID, and nothing is sent again. Recording sends one real
// message and waits for the list to show it (see sms.ReconcileSettle);
// replay skips until its cassettes exist.
func TestRecordedReconcileFindsSent(t *testing.T) {
	dir := filepath.Join(cassettes, "reconcile-sent")
	mode := os.Getenv(xrrtest.EnvMode)
	if mode == "" || mode == string(xrrtest.ModeReplay) {
		if files, _ := filepath.Glob(filepath.Join(dir, "*.yaml")); len(files) == 0 {
			t.Skip("no recorded reconcile-sent cassettes yet")
		}
	}
	a := liveAccount()
	inst := a.open(t, a.client(t, dir))
	attempted := attemptTime(t, dir)
	sent, err := inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{Effect: a.effect(), Content: recordedText})
	if err != nil || sent.State != model.ProviderAccepted || len(sent.References) != 1 {
		t.Fatalf("Send = %+v, %v; want provider_accepted with the message", sent, err)
	}
	if mode == string(xrrtest.ModeRecord) {
		waitListed(t, a, attempted)
	}
	rec, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: a.effect(), Content: recordedText, AttemptedAt: attempted})
	// The list shows the message's state by then: still held by Twilio, or
	// already delivered.
	held := rec.State == model.ProviderAccepted || rec.State == model.Delivered
	if err != nil || !held || rec.Failure != nil || len(rec.References) != 1 || rec.References[0].ID != sent.References[0].ID {
		t.Fatalf("Reconcile = %+v (failure %v), %v; want the sent message %v, accepted or delivered", rec, rec.Failure, err, sent.References)
	}
}

// TestRecordedAnswers replays Twilio answers recorded without delivering a
// message: a create Twilio refuses, reconcile listings, an unknown message
// and refused credentials.
func TestRecordedAnswers(t *testing.T) {
	ctx := t.Context()
	// Twilio refuses a message to its own sender (error 21266), so nobody
	// is messaged. The refusal is final, yet Twilio keeps the refused
	// message as failed (and bills it): reconciling the attempt finds that
	// message and settles it rejected, never resendable. The Messages list
	// shows the message only some time after the refusal (see
	// sms.ReconcileSettle): recording waits for it before the recorded
	// listing.
	t.Run("RejectedIsFinal", func(t *testing.T) {
		a := liveAccount()
		a.to = a.from
		dir := filepath.Join(cassettes, "rejected")
		inst := a.open(t, a.client(t, dir))
		attempted := attemptTime(t, dir)
		out, err := inst.(adapter.Sender).Send(ctx, adapter.SendRequest{Effect: a.effect(), Content: recordedText})
		if err != nil || out.State != model.Rejected || out.Failure == nil ||
			out.Failure.Code != model.Permanent || out.Failure.Retry != model.NoRetry || len(out.References) != 0 {
			t.Fatalf("Send = %+v (failure %v), %v; want rejected, permanent, never retried", out, out.Failure, err)
		}
		if !strings.Contains(out.Failure.SafeMessage, "error 21266") {
			t.Fatalf("failure %q does not name Twilio's error code", out.Failure.SafeMessage)
		}
		if os.Getenv(xrrtest.EnvMode) == string(xrrtest.ModeRecord) {
			waitListed(t, a, attempted)
		}
		rec, err := inst.(adapter.Reconciler).Reconcile(ctx, adapter.ReconcileRequest{Effect: a.effect(), Content: recordedText, AttemptedAt: attempted})
		if err != nil || rec.State != model.Rejected || rec.Failure == nil || rec.Failure.Code != model.Permanent ||
			rec.Failure.Retry != model.NoRetry || len(rec.References) != 1 || !strings.Contains(rec.Failure.SafeMessage, "error 21266") {
			t.Fatalf("Reconcile = %+v (failure %v), %v; want the refused message, rejected for good", rec, rec.Failure, err)
		}
	})
	// A listing holding no message the attempt created: Twilio has none,
	// so a new attempt is safe. The route is a number nobody messaged.
	t.Run("ReconcileFindsNone", func(t *testing.T) {
		a := liveAccount()
		r := a.open(t, a.client(t, filepath.Join(cassettes, "reconcile-none"))).(adapter.Reconciler)
		out, err := r.Reconcile(ctx, adapter.ReconcileRequest{Effect: a.effect(), Content: recordedText, AttemptedAt: recordedClock.Now()})
		if err != nil || out.State != model.Failed || out.Failure == nil || out.Failure.Retry != model.RetrySameOperation || len(out.References) != 0 {
			t.Fatalf("Reconcile = %+v (failure %v), %v; want failed, safe to retry", out, out.Failure, err)
		}
	})
	t.Run("FetchUnknownMessage", func(t *testing.T) {
		a := liveAccount()
		c := a.rest(t, a.client(t, filepath.Join(cassettes, "fetch-unknown")))
		_, err := c.FetchMessage(ctx, a.sid, ports.Secret{Bytes: []byte(a.token)}, "SM00000000000000000000000000000000")
		var ae *twilio.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusNotFound || ae.Code != 20404 {
			t.Fatalf("FetchMessage error = %v, want HTTP 404, error 20404", err)
		}
	})
	// A wrong auth token earns 401 (error 20003): reconcile reports it as
	// unauthorized, never retried.
	t.Run("RefusedCredentials", func(t *testing.T) {
		a := liveAccount()
		a.token = "not-the-auth-token"
		r := a.open(t, a.client(t, filepath.Join(cassettes, "refused-credentials"))).(adapter.Reconciler)
		_, err := r.Reconcile(ctx, adapter.ReconcileRequest{Effect: a.effect(), Content: recordedText, AttemptedAt: recordedClock.Now()})
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.Unauthorized || me.Retry != model.NoRetry {
			t.Fatalf("Reconcile error = %v, want unauthorized, never retried", err)
		}
	})
}

// waitListed polls the live Messages list, unrecorded, until it shows the
// message a's attempt at attempted created, and logs how long that took.
func waitListed(t *testing.T, a recordedAccount, attempted time.Time) {
	t.Helper()
	c := a.rest(t, http.DefaultClient)
	start := time.Now()
	for time.Since(start) < 5*time.Minute {
		_, found, err := c.FindSent(t.Context(), a.sid, ports.Secret{Bytes: []byte(a.token)}, twilio.SentMessage{
			To: a.to, From: a.from, Body: "recorded test message", Since: attempted,
		})
		if err != nil {
			t.Fatalf("live listing: %v", err)
		}
		if found {
			t.Logf("the Messages list showed the message after %v", time.Since(start).Round(time.Second))
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("the Messages list never showed the message")
}

// attemptTime is when a recorded attempt started. Recording takes the wall
// clock, truncated to Twilio's one-second resolution, and keeps it next to
// the cassettes; replay reads it back, so a listing excludes the same
// earlier messages in both modes.
func attemptTime(t *testing.T, dir string) time.Time {
	t.Helper()
	file := filepath.Join(dir, "attempted_at")
	if os.Getenv(xrrtest.EnvMode) == string(xrrtest.ModeRecord) {
		now := time.Now().UTC().Truncate(time.Second)
		if err := os.WriteFile(file, []byte(now.Format(time.RFC3339)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return now
	}
	b, err := os.ReadFile(file) // #nosec G304 -- test data
	if err != nil {
		t.Fatalf("attempt time: %v; re-record with %s=record", err, xrrtest.EnvMode)
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return at
}

var recordedText = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "recorded test message"}}}

// A cassette recorded against the local Messages API replays without it,
// and keeps the credentials and placeholdered identifiers out: the
// recorded tests above take real cassettes the same way.
func TestCassetteRoundTrip(t *testing.T) {
	fake := newFakeTwilio(t)
	dir := t.TempDir()
	a := recordedAccount{sid: hookAccount, token: "record-token", from: senderNumber, to: "+15558675310"}
	recorded := sendAndFetch(t, a.client(t, dir, xrrtest.WithMode(xrrtest.ModeRecord), xrrtest.WithPlaceholder(fake.srv.URL[len("http://"):], "PROVIDER_HOST")), fake.srv.URL, a)
	if n := len(fake.calls(hookAccount)); n != 2 {
		t.Fatalf("record mode reached the API %d times, want 2", n)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) == 0 {
		t.Fatal("no cassette written")
	}
	for _, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- test temp dir
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{a.token, a.sid, a.from, a.to, url.QueryEscape(a.from), url.QueryEscape(a.to), "Authorization"} {
			if bytes.Contains(b, []byte(secret)) {
				t.Errorf("%s contains %q", filepath.Base(f), secret)
			}
		}
	}

	fake.srv.Close()
	replayed := sendAndFetch(t, a.client(t, dir, xrrtest.WithMode(xrrtest.ModeReplay), xrrtest.WithPlaceholder(fake.srv.URL[len("http://"):], "PROVIDER_HOST")), fake.srv.URL, a)
	if replayed.SID != recorded.SID || replayed.To != a.to {
		t.Fatalf("replay = %+v, recorded %+v", replayed, recorded)
	}
}

// A recorded Messages list holds the messages the scenario sent and no
// other message of the account's history.
func TestScrubForeignMessages(t *testing.T) {
	page := `{"end":3,"messages":[{"body":"recorded test message","sid":"SM1"},` +
		`{"body":"an earlier private message","sid":"SM2"},{"body":"Recorded test reply","sid":"SM3"}],"page":0}`
	var got struct {
		End      int `json:"end"`
		Page     int `json:"page"`
		Messages []struct {
			SID string `json:"sid"`
		} `json:"messages"`
	}
	out := scrubForeignMessages("application/json", []byte(page))
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("scrubbed page %s: %v", out, err)
	}
	if len(got.Messages) != 2 || got.Messages[0].SID != "SM1" || got.Messages[1].SID != "SM3" || got.End != 1 || bytes.Contains(out, []byte("private")) {
		t.Fatalf("scrubbed page = %s, want only SM1 and SM3, end 1", out)
	}
	for _, other := range []string{`{"message":"not found","status":404}`, `not json`} {
		if got := scrubForeignMessages("", []byte(other)); string(got) != other {
			t.Errorf("scrub changed %q to %q", other, got)
		}
	}
}

type recordedAccount struct{ sid, token, from, to string }

// client records to and replays from dir. Numbers are placeholdered in
// both their plain and their query-escaped forms, since listings carry
// them in query strings, and the partly masked numbers of Twilio's error
// messages are scrubbed.
func (a recordedAccount) client(t *testing.T, dir string, opts ...xrrtest.Option) *http.Client {
	return xrrtest.NewClient(t, dir, append([]xrrtest.Option{
		xrrtest.WithPlaceholder(a.sid, "ACCOUNT_SID"),
		xrrtest.WithPlaceholder(a.from, "FROM_NUMBER"),
		xrrtest.WithPlaceholder(a.to, "TO_NUMBER"),
		xrrtest.WithPlaceholder(url.QueryEscape(a.from), "FROM_NUMBER_QUERY"),
		xrrtest.WithPlaceholder(url.QueryEscape(a.to), "TO_NUMBER_QUERY"),
		xrrtest.WithScrub(scrubMaskedNumbers),
		xrrtest.WithScrub(scrubForeignMessages),
	}, opts...)...)
}

// scrubForeignMessages keeps only the messages of the recorded scenarios
// from a Messages list page, the test message and the recorder's reply:
// the list covers the account's whole history to the recipient (and, for
// history, a whole day of the replying phone's messages), whose other
// messages are not ours to publish.
func scrubForeignMessages(_ string, body []byte) []byte {
	var page map[string]json.RawMessage
	if json.Unmarshal(body, &page) != nil || page["messages"] == nil {
		return body
	}
	var all []json.RawMessage
	if json.Unmarshal(page["messages"], &all) != nil {
		return body
	}
	kept := make([]json.RawMessage, 0, len(all))
	for _, raw := range all {
		var m struct {
			Body string `json:"body"`
		}
		if json.Unmarshal(raw, &m) == nil && (m.Body == recordedText.Parts[0].Text || isRecordedReply(m.Body)) {
			kept = append(kept, raw)
		}
	}
	page["messages"], _ = json.Marshal(kept)
	page["end"], _ = json.Marshal(max(len(kept)-1, 0))
	out, err := json.Marshal(page)
	if err != nil {
		return body
	}
	return out
}

// maskedNumber is a phone number Twilio masks in error messages, such as
// +1555867XXXX: its leading digits are still the account's.
var maskedNumber = regexp.MustCompile(`\+?[0-9]+X{2,}`)

func scrubMaskedNumbers(_ string, body []byte) []byte {
	return maskedNumber.ReplaceAll(body, []byte("MASKED_NUMBER"))
}

// open opens a twilio.sms instance of a's account on client, against the
// Twilio API (empty baseURL) unless one is given.
func (a recordedAccount) open(t *testing.T, client *http.Client, baseURL ...string) adapter.Instance {
	t.Helper()
	opts := sms.Options{}
	if len(baseURL) > 0 {
		opts.BaseURL = baseURL[0]
	}
	inst, err := sms.NewFactory(opts).Open(t.Context(), model.Channel{
		Ref: recordedChannel, Type: model.TypeTwilioSms, Configuration: sms.Config{AccountSID: a.sid, From: a.from}.Extension(),
		CredentialRef: "twilio", Revision: "ch1",
	}, adapter.Dependencies{HTTP: client, Credentials: staticCreds{ports.Secret{Bytes: []byte(a.token)}}, Clock: recordedClock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
}

func (a recordedAccount) rest(t *testing.T, client *http.Client, baseURL ...string) *twilio.Client {
	t.Helper()
	cfg := twilio.ClientConfig{HTTP: client}
	if len(baseURL) > 0 {
		cfg.BaseURL = baseURL[0]
	}
	c, err := twilio.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var recordedChannel = model.ChannelRef{Tenant: "recorded", ID: "00000000-0000-7000-8000-0000000005a0"}

// effect is a public effect on the route to a.to.
func (a recordedAccount) effect() adapter.Effect {
	return adapter.Effect{
		Operation: model.OperationRef{Tenant: recordedChannel.Tenant, ID: "00000000-0000-7000-8000-0000000005b1"},
		Attempt:   "00000000-0000-7000-8000-0000000005c1",
		Route:     model.ExternalRef{Channel: recordedChannel, Namespace: sms.Namespace, Kind: sms.RouteKind, ID: a.to},
		Audience:  model.Audience{Visibility: model.Public},
	}
}

// sendAndFetch sends one message through the adapter, then fetches it by
// the SID the send reported.
func sendAndFetch(t *testing.T, client *http.Client, baseURL string, a recordedAccount) twilio.Message {
	t.Helper()
	out, err := a.open(t, client, baseURL).(adapter.Sender).Send(t.Context(), adapter.SendRequest{Effect: a.effect(), Content: recordedText})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	m, err := a.rest(t, client, baseURL).FetchMessage(t.Context(), a.sid, ports.Secret{Bytes: []byte(a.token)}, out.References[0].ID)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	if _, ok := twilio.DeliveryState(m.Status); !ok || m.SID != out.References[0].ID {
		t.Fatalf("fetched %+v", m)
	}
	return m
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

type staticCreds struct{ c ports.Credential }

func (s staticCreds) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return s.c, nil
}
