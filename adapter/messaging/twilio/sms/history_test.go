package sms_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/sms"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

const peerNumber = "+15558675310"

var historyRoute = model.ExternalRef{Channel: hookChannel, Namespace: sms.Namespace, Kind: sms.RouteKind, ID: peerNumber}

// openHistory opens a twilio.sms instance of hookChannel with cfg on fake
// (no HTTP client when fake is nil).
func openHistory(t *testing.T, fake *fakeTwilio, cfg sms.Config) adapter.Instance {
	t.Helper()
	opts := sms.Options{}
	deps := adapter.Dependencies{Credentials: &tokenHost{}, Clock: portstest.NewFixedClock(time.Unix(1_760_000_000, 0))}
	if fake != nil {
		deps.HTTP, opts.BaseURL = fake.client(), fake.srv.URL
	}
	inst, err := sms.NewFactory(opts).Open(t.Context(), model.Channel{
		Ref: hookChannel, Type: model.TypeTwilioSms, Configuration: cfg.Extension(), CredentialRef: "twilio", Revision: "ch1",
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
}

func history(t *testing.T, inst adapter.Instance, req adapter.HistoryRequest) (adapter.HistoryPage, error) {
	t.Helper()
	hr, ok := inst.(adapter.HistoryReader)
	if !ok {
		t.Fatal("instance is no adapter.HistoryReader")
	}
	return hr.History(t.Context(), req)
}

// A message read from history is the envelope the webhook reports for the
// same message, at the time Twilio received it, so the engine imports a
// message once whichever way it arrived.
func TestHistoryEnvelopeMatchesWebhook(t *testing.T) {
	fake := newFakeTwilio(t)
	inst := openHistory(t, fake, hookConfig())
	sink := &recordingSink{}
	body := fixture(t, "incoming_sms.form")
	if rec := post(inst.(adapter.HTTPReceiver).Handler(sink), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK || len(sink.got) != 1 {
		t.Fatalf("webhook = %d, %d accepted", rec.Code, len(sink.got))
	}
	at := time.Date(2026, 10, 11, 9, 30, 5, 0, time.UTC)
	fake.holdInbound(hookAccount, fakeMessage{
		SID: "SM10000000000000000000000000000001", From: peerNumber, To: senderNumber, Body: "Hello from the other side", Sent: at,
	})
	page, err := history(t, inst, adapter.HistoryRequest{Since: at.Add(-time.Hour), Limit: 10})
	if err != nil || len(page.Envelopes) != 1 || page.Next != "" || page.Gap {
		t.Fatalf("History = %+v, %v; want the one message", page, err)
	}
	want := sink.got[0].Envelope
	want.OccurredAt = at
	if got := page.Envelopes[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("history envelope\n%+v\nwant the webhook's\n%+v", got, want)
	}
}

// An MMS from history carries the media summary the webhook reports, its
// count only: the listing names no content types, and history stages no
// media.
func TestHistoryMediaSummary(t *testing.T) {
	fake := newFakeTwilio(t)
	at := time.Date(2026, 10, 11, 9, 30, 5, 0, time.UTC)
	fake.holdInbound(hookAccount, fakeMessage{SID: "MM10000000000000000000000000000002", From: peerNumber, To: senderNumber, Sent: at, Media: 2})
	page, err := history(t, openHistory(t, fake, hookConfig()), adapter.HistoryRequest{Route: &historyRoute, Since: at, Limit: 10})
	if err != nil || len(page.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v", page, err)
	}
	parts := page.Envelopes[0].Content.Parts
	var m sms.Media
	if len(parts) != 1 || parts[0].Structured == nil || parts[0].Structured.Name != sms.PayloadMedia ||
		json.Unmarshal(parts[0].Structured.Data, &m) != nil || m.Count != 2 || len(m.ContentTypes) != 0 {
		t.Fatalf("content %+v, want the media summary of 2", parts)
	}
}

// The channel's own sends are outbound messages: history never reports
// them as received, so they cannot come back as new inbound messages.
func TestHistoryLeavesOwnSendsOut(t *testing.T) {
	fake := newFakeTwilio(t)
	inst := openHistory(t, fake, hookConfig())
	since := time.Now().Add(-time.Minute)
	sendReq := adapter.SendRequest{
		Effect:  adapter.Effect{Operation: sendOp, Attempt: "00000000-0000-7000-8000-0000000004c1", Route: historyRoute, Audience: model.Audience{Visibility: model.Public}},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "our reply"}}},
	}
	sent, err := inst.(adapter.Sender).Send(t.Context(), sendReq)
	if err != nil || len(sent.References) != 1 {
		t.Fatalf("Send = %+v, %v", sent, err)
	}
	fake.holdInbound(hookAccount, fakeMessage{SID: "SM10000000000000000000000000000003", From: peerNumber, To: senderNumber, Body: "hi", Sent: time.Now()})
	for _, route := range []*model.ExternalRef{&historyRoute, nil} {
		page, err := history(t, inst, adapter.HistoryRequest{Route: route, Since: since, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, env := range page.Envelopes {
			ids = append(ids, env.MessageID)
		}
		if !slices.Equal(ids, []string{"SM10000000000000000000000000000003"}) {
			t.Fatalf("route %v: history = %q, want only the received message, never the send %s", route, ids, sent.References[0].ID)
		}
	}
}

// Refusals come before any listing: a request the reader cannot serve
// reaches no API.
func TestHistoryRefusesBeforeAnyCall(t *testing.T) {
	since := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	otherChannel := historyRoute
	otherChannel.Channel = sendChannel
	otherKind := historyRoute
	otherKind.Kind = "email"
	service := hookConfig()
	service.From, service.MessagingServiceSID = "", "MG00000000000000000000000000000001"
	for _, c := range []struct {
		name string
		cfg  sms.Config
		req  adapter.HistoryRequest
		code model.ErrorCode
	}{
		{"no limit", hookConfig(), adapter.HistoryRequest{Since: since}, model.Validation},
		{"cursor never issued", hookConfig(), adapter.HistoryRequest{Since: since, Limit: 10, Cursor: "conformance-unknown-cursor"}, model.Validation},
		{"next page URI as cursor", hookConfig(), adapter.HistoryRequest{Since: since, Limit: 10, Cursor: "/2010-04-01/Accounts/" + hookAccount + "/Messages.json?Page=1"}, model.Validation},
		{"route of another channel", hookConfig(), adapter.HistoryRequest{Route: &otherChannel, Since: since, Limit: 10}, model.Validation},
		{"route of another kind", hookConfig(), adapter.HistoryRequest{Route: &otherKind, Since: since, Limit: 10}, model.Validation},
		{"messaging service without a number", service, adapter.HistoryRequest{Since: since, Limit: 10}, model.Unsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeTwilio(t)
			_, err := history(t, openHistory(t, fake, c.cfg), c.req)
			if model.CodeOf(err) != c.code {
				t.Fatalf("History = %v, want %s", err, c.code)
			}
			if n := len(fake.calls(hookAccount)); n != 0 {
				t.Fatalf("%d API calls, want none", n)
			}
		})
	}
	if _, err := history(t, openHistory(t, nil, hookConfig()), adapter.HistoryRequest{Since: since, Limit: 10}); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("History without an HTTP client = %v, want unavailable", err)
	}
	inst := openHistory(t, newFakeTwilio(t), hookConfig())
	_ = inst.Close(t.Context())
	if _, err := history(t, inst, adapter.HistoryRequest{Since: since, Limit: 10}); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("History on a closed instance = %v, want unavailable", err)
	}
}

// history is available on a channel with a number of its own; a
// Messaging Service alone names no number whose inbound messages to list.
func TestHistoryCapabilityStatus(t *testing.T) {
	service := hookConfig()
	service.From, service.MessagingServiceSID = "", "MG00000000000000000000000000000001"
	for _, c := range []struct {
		cfg  sms.Config
		want model.CapabilityStatus
	}{
		{hookConfig(), model.CapabilityStatus{Name: model.CapabilityHistory, Available: true}},
		{service, model.CapabilityStatus{Name: model.CapabilityHistory, Reason: model.Unsupported}},
	} {
		statuses, err := openHistory(t, nil, c.cfg).Capabilities(t.Context())
		i := slices.IndexFunc(statuses, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityHistory })
		if err != nil || i < 0 || !reflect.DeepEqual(statuses[i], c.want) {
			t.Fatalf("from %q: statuses %+v, %v; want %+v", c.cfg.From, statuses, err, c.want)
		}
	}
}

// Through the engine: a message the webhook delivered is skipped by an
// import of its conversation, the others are imported once and marked
// imported, and a second import adds nothing.
func TestHistoryImportSkipsWebhookCopy(t *testing.T) {
	fake := newFakeTwilio(t)
	h := newHookHostOn(t, hookConfig(), fake)
	body := fixture(t, "incoming_sms.form")
	if rec := post(h.handler(), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("webhook = %d", rec.Code)
	}
	live := h.messages()
	if len(live) != 1 {
		t.Fatalf("messages after the webhook = %d, want 1", len(live))
	}
	at := time.Unix(1_760_000_000, 0).UTC()
	for i, sid := range []string{"SM10000000000000000000000000000004", "SM10000000000000000000000000000001", "SM10000000000000000000000000000005"} {
		fake.holdInbound(hookAccount, fakeMessage{SID: sid, From: peerNumber, To: senderNumber, Body: "message " + sid, Sent: at.Add(time.Duration(i-3) * time.Hour)})
	}
	conv := live[0].Ref.Conversation
	req := uma.ImportRequest{Requester: "importer", Conversation: &conv, Since: at.Add(-24 * time.Hour)}
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: hookTenant, Actor: "hooks"}, Call: model.Call{Correlation: "import"}}
	rep, err := h.engine.Runtime().Import(t.Context(), call, req)
	if err != nil || rep != (uma.ImportReport{Imported: 2, Skipped: 1, Complete: true}) {
		t.Fatalf("Import = %+v, %v; want 2 imported and the webhook's copy skipped", rep, err)
	}
	ms := h.messages()
	if len(ms) != 3 {
		t.Fatalf("messages = %d, want 3", len(ms))
	}
	for _, m := range ms {
		webhook := slices.Contains(m.External, model.ProviderMessageRef(hookChannel, "SM10000000000000000000000000000001"))
		if m.Ref.Conversation != conv || (m.ImportedBy == "") != webhook {
			t.Errorf("message %v in %v imported by %q; want it in %v, imported unless the webhook delivered it", m.External, m.Ref.Conversation, m.ImportedBy, conv)
		}
	}
	if again, err := h.engine.Runtime().Import(t.Context(), call, req); err != nil || again != (uma.ImportReport{Skipped: 3, Complete: true}) {
		t.Fatalf("second Import = %+v, %v; want all 3 skipped", again, err)
	}
}

// History passes Until to the listing: a message received at Until is not
// in the window.
func TestHistoryUntilEndsTheWindow(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 30, 5, 0, time.UTC)
	fake := newFakeTwilio(t)
	fake.holdInbound(hookAccount, fakeMessage{SID: "SM10000000000000000000000000000001", From: peerNumber, To: senderNumber, Body: "before", Sent: at.Add(-time.Second)})
	fake.holdInbound(hookAccount, fakeMessage{SID: "SM10000000000000000000000000000002", From: peerNumber, To: senderNumber, Body: "at", Sent: at})
	inst := openHistory(t, fake, hookConfig())
	page, err := history(t, inst, adapter.HistoryRequest{Since: at.Add(-time.Hour), Until: at, Limit: 10})
	if err != nil || len(page.Envelopes) != 1 || !page.Envelopes[0].OccurredAt.Equal(at.Add(-time.Second)) {
		t.Fatalf("History = %+v, %v; want only the message before Until", page, err)
	}
}
