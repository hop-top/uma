package whatsapp_test

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/adapter/messaging/twilio/whatsapp"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// cassettes holds recorded Messages API exchanges.
const cassettes = "testdata/cassettes/twilio"

// recordedRecipient stands in for the recipient the cassettes were
// recorded to: a WhatsApp user ID (CA.<digits>), which is how Twilio
// names a person who wrote to a WhatsApp sender in place of a phone
// number. Twilio sends to it as it does to a number. A phone number is
// escaped in the form body and an ID is not, so replay needs the same
// kind of recipient the recording used.
const recordedRecipient = "CA.0000000000000001"

// TestRecordedSendAndFetch sends one WhatsApp message, free-form or a
// template, and fetches it back through recorded exchanges. Replay (the
// default) needs no credentials and skips until cassettes exist. To
// record against the Twilio WhatsApp Sandbox, after TWILIO_TO joined it
// and messaged it within 24 hours (free-form) or with an approved Content
// Template SID (template):
//
//	XRR_MODE=record TWILIO_ACCOUNT_SID=AC... TWILIO_AUTH_TOKEN=... \
//	TWILIO_FROM=+14155238886 TWILIO_TO=+1... [TWILIO_CONTENT_SID=HX...] \
//	go test ./whatsapp -run TestRecordedSendAndFetch
//
// The account SID and both numbers are replaced by placeholders before
// anything is written; review testdata/cassettes before committing.
func TestRecordedSendAndFetch(t *testing.T) {
	mode := os.Getenv(xrrtest.EnvMode)
	if mode == "" || mode == string(xrrtest.ModeReplay) {
		if files, _ := filepath.Glob(filepath.Join(cassettes, "*.yaml")); len(files) == 0 {
			t.Skip("no recorded Twilio WhatsApp cassettes yet")
		}
	}
	a := recordedAccount{
		sid:   envOr("TWILIO_ACCOUNT_SID", "AC00000000000000000000000000000000"),
		token: envOr("TWILIO_AUTH_TOKEN", "replay-token"),
		from:  envOr("TWILIO_FROM", senderNumber),
		to:    envOr("TWILIO_TO", recordedRecipient),
	}
	part := model.Part{Kind: model.PartText, Text: "recorded test message"}
	if sid := os.Getenv("TWILIO_CONTENT_SID"); sid != "" {
		part = whatsapp.Template{ContentSID: sid}.Part()
	}
	sendAndFetch(t, a.client(t, cassettes), "", a, part)
}

// A cassette recorded against the local Messages API replays without it,
// and keeps the credentials and placeholdered identifiers out: the
// recorded test above takes real cassettes the same way.
func TestCassetteRoundTrip(t *testing.T) {
	fake := newFakeTwilio(t)
	dir := t.TempDir()
	a := recordedAccount{sid: hookAccount, token: "record-token", from: senderNumber, to: userNumber}
	host := xrrtest.WithPlaceholder(fake.srv.URL[len("http://"):], "PROVIDER_HOST")
	recorded := sendAndFetch(t, a.client(t, dir, xrrtest.WithMode(xrrtest.ModeRecord), host), fake.srv.URL, a, helloTemplate.Part())
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
	replayed := sendAndFetch(t, a.client(t, dir, xrrtest.WithMode(xrrtest.ModeReplay), host), fake.srv.URL, a, helloTemplate.Part())
	if replayed.SID != recorded.SID || replayed.To != "whatsapp:"+a.to {
		t.Fatalf("replay = %+v, recorded %+v", replayed, recorded)
	}
}

type recordedAccount struct{ sid, token, from, to string }

// client records to and replays from dir. Numbers are placeholdered in
// both their plain and their query-escaped forms: the form-encoded send
// body and the listings carry them as %2B1....
func (a recordedAccount) client(t *testing.T, dir string, opts ...xrrtest.Option) *http.Client {
	return xrrtest.NewClient(t, dir, append([]xrrtest.Option{
		xrrtest.WithPlaceholder(a.sid, "ACCOUNT_SID"),
		xrrtest.WithPlaceholder(a.from, "FROM_NUMBER"),
		xrrtest.WithPlaceholder(a.to, "TO_NUMBER"),
		xrrtest.WithPlaceholder(url.QueryEscape(a.from), "FROM_NUMBER_QUERY"),
		xrrtest.WithPlaceholder(url.QueryEscape(a.to), "TO_NUMBER_QUERY"),
	}, opts...)...)
}

// sendAndFetch sends one message through the adapter, then fetches it by
// the SID the send reported.
func sendAndFetch(t *testing.T, client *http.Client, baseURL string, a recordedAccount, part model.Part) twilio.Message {
	t.Helper()
	ch := model.ChannelRef{Tenant: "recorded", ID: "00000000-0000-7000-8000-0000000007e0"}
	creds := ports.Secret{Bytes: []byte(a.token)}
	inst, err := whatsapp.NewFactory(whatsapp.Options{BaseURL: baseURL}).Open(t.Context(), model.Channel{
		Ref: ch, Type: model.TypeTwilioWhatsapp, Configuration: whatsapp.Config{AccountSID: a.sid, From: a.from}.Extension(),
		CredentialRef: "twilio", Revision: "ch1",
	}, adapter.Dependencies{HTTP: client, Credentials: staticCreds{creds}, Clock: portstest.NewFixedClock(time.Unix(1_760_000_000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	out, err := inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{
		Effect: adapter.Effect{
			Operation: model.OperationRef{Tenant: ch.Tenant, ID: "00000000-0000-7000-8000-0000000007e1"},
			Attempt:   "00000000-0000-7000-8000-0000000007e2",
			Route:     model.ExternalRef{Channel: ch, Namespace: whatsapp.Namespace, Kind: whatsapp.RouteKind, ID: a.to},
			Audience:  model.Audience{Visibility: model.Public},
		},
		Content: model.Content{Parts: []model.Part{part}},
	})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	rest, err := twilio.NewClient(twilio.ClientConfig{HTTP: client, BaseURL: baseURL})
	if err != nil {
		t.Fatal(err)
	}
	m, err := rest.FetchMessage(t.Context(), a.sid, creds, out.References[0].ID)
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
