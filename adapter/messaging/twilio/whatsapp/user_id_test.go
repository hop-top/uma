package whatsapp_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/whatsapp"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

// userID is a WhatsApp business-scoped user ID (CC.<id>), how Twilio names
// a user whose phone number WhatsApp hides behind a username.
const userID = "CA.0000000000000002"

// A user without a visible number arrives with the user ID in From and in
// ExternalUserId and no WaId: the sender is the ID, and the payload says
// it is one.
func TestWebhookUserIDOnly(t *testing.T) {
	sink := &recordingSink{}
	p := newProtocol()
	body := fixture(t, "incoming_text_user_id.form")
	if rec := post(hook(t, &tokenHost{}, p, sink), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q", rec.Code, rec.Body)
	}
	env := sink.got[0].Envelope
	if want := (model.ExternalRef{Channel: testChannel, Namespace: whatsapp.Namespace, Kind: whatsapp.RouteKind, ID: userID}); env.Sender != want {
		t.Fatalf("sender %+v, want %+v", env.Sender, want)
	}
	var in whatsapp.Inbound
	if json.Unmarshal(env.Payload.Data, &in) != nil ||
		in != (whatsapp.Inbound{ProfileName: "Ada", MessageType: "text", ExternalUserID: userID}) {
		t.Fatalf("payload %s", env.Payload.Data)
	}
	if got := p.lastInbound(userID); !got.Equal(testNow) {
		t.Fatalf("window = %v, want %v", got, testNow)
	}
}

// With a visible number From is the number and ExternalUserId carries the
// user ID: the sender stays the number and the payload keeps the ID.
func TestWebhookPhoneAndUserID(t *testing.T) {
	sink := &recordingSink{}
	body := fixture(t, "incoming_text_phone_user_id.form")
	if rec := post(hook(t, &tokenHost{}, nil, sink), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q", rec.Code, rec.Body)
	}
	env := sink.got[0].Envelope
	if env.Sender.ID != userNumber {
		t.Fatalf("sender %+v, want the number", env.Sender)
	}
	var in whatsapp.Inbound
	if json.Unmarshal(env.Payload.Data, &in) != nil ||
		in != (whatsapp.Inbound{ProfileName: "Ada", WaID: "15558675310", MessageType: "text", ExternalUserID: "US.0000000000000003"}) {
		t.Fatalf("payload %s", env.Payload.Data)
	}
}

// The user ID arrives in a JSON-bodied webhook under the same name.
func TestWebhookJSONUserID(t *testing.T) {
	sink := &recordingSink{}
	body, q := jsonOf(t, fixture(t, "incoming_text_phone_user_id.form"))
	if rec := post(hook(t, &tokenHost{}, nil, sink), q, body, jsonType, signJSON(hookToken, q)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q", rec.Code, rec.Body)
	}
	var in whatsapp.Inbound
	if json.Unmarshal(sink.got[0].Envelope.Payload.Data, &in) != nil || in.ExternalUserID != "US.0000000000000003" {
		t.Fatalf("payload %s", sink.got[0].Envelope.Payload.Data)
	}
}

// A quick-reply tap from a user known only by ID keeps the button's hidden
// payload, variables included, and its label.
func TestWebhookButtonFromUserID(t *testing.T) {
	sink := &recordingSink{}
	body := fixture(t, "incoming_button_user_id.form")
	if rec := post(hook(t, &tokenHost{}, nil, sink), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q", rec.Code, rec.Body)
	}
	env := sink.got[0].Envelope
	parts := env.Content.Parts
	if len(parts) != 2 || parts[0].Text != "Join" || parts[1].Structured == nil || parts[1].Structured.Name != whatsapp.PayloadButton {
		t.Fatalf("parts %+v", parts)
	}
	var b whatsapp.Button
	if json.Unmarshal(parts[1].Structured.Data, &b) != nil || b != (whatsapp.Button{Payload: "invite:0000000000000042", Text: "Join"}) {
		t.Fatalf("button %s", parts[1].Structured.Data)
	}
	var in whatsapp.Inbound
	if json.Unmarshal(env.Payload.Data, &in) != nil || in.ExternalUserID != userID || in.MessageType != "button" ||
		in.RepliedTo != "SM20000000000000000000000000000010" || env.Sender.ID != userID {
		t.Fatalf("sender %+v payload %s", env.Sender, env.Payload.Data)
	}
}

// A route to a user ID, a parent ID (CC.ENT.<id>) included, goes to
// Twilio as whatsapp: plus the ID, unchanged.
func TestSendToUserID(t *testing.T) {
	for _, id := range []string{userID, "US.ENT.0000000000000004"} {
		t.Run(id, func(t *testing.T) {
			fake := newFakeTwilio(t)
			req := sendReq()
			req.Route.ID = id
			out, err := sender(t, fake, &tokenHost{}, nil).Send(t.Context(), req)
			if err != nil || out.State != model.ProviderAccepted {
				t.Fatalf("Send = %+v, %v", out, err)
			}
			calls := fake.calls(hookAccount)
			if len(calls) != 1 || calls[0].Form.Get("To") != "whatsapp:"+id {
				t.Fatalf("calls %+v", calls)
			}
		})
	}
}

// The window an ID-only user's message opens is the one a reply to that
// ID checks.
func TestUserIDWindow(t *testing.T) {
	fake := newFakeTwilio(t)
	clock := portstest.NewFixedClock(testNow)
	inst := open(t, fake, hookConfig(), adapter.Dependencies{Credentials: &tokenHost{}, Protocol: newProtocol(), Clock: clock})
	body := fixture(t, "incoming_text_user_id.form")
	if rec := post(inst.(adapter.HTTPReceiver).Handler(&recordingSink{}), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("webhook = %d", rec.Code)
	}
	clock.Advance(whatsapp.ServiceWindow)
	req := sendReq()
	req.Route.ID = userID
	if out, err := inst.(adapter.Sender).Send(t.Context(), req); err != nil || out.State != model.Rejected {
		t.Fatalf("Send after the window = %+v, %v; want rejected", out, err)
	}
	if n := len(fake.calls(hookAccount)); n != 0 {
		t.Fatalf("calls = %d, want none", n)
	}
}

// An incoming message reports the sender's verified attributes: the number
// when WhatsApp shows one, the user ID when Twilio sends one. WhatsApp
// hides the number of a user with a username, so the same person may
// arrive keyed by number (with both) or by ID (with the ID alone), and a
// host links the two through the shared user ID.
func TestWebhookSenderAttributes(t *testing.T) {
	phone := model.Attribute{Kind: model.AttributePhone, Value: userNumber, Verified: true}
	id := func(v string) model.Attribute {
		return model.Attribute{Kind: model.AttributeWhatsAppUserID, Value: v, Verified: true}
	}
	cases := map[string]struct {
		fixture string
		edit    func(string) string
		want    []model.Attribute
	}{
		"number only":    {"incoming_text.form", nil, []model.Attribute{phone}},
		"hidden number":  {"incoming_text_user_id.form", nil, []model.Attribute{id(userID)}},
		"number and ID":  {"incoming_text_phone_user_id.form", nil, []model.Attribute{phone, id("US.0000000000000003")}},
		"button from ID": {"incoming_button_user_id.form", nil, []model.Attribute{id(userID)}},
		"parent ID": {"incoming_text_phone_user_id.form", func(b string) string {
			return strings.Replace(b, "US.0000000000000003", "US.ENT.0000000000000004", 1)
		}, []model.Attribute{phone, id("US.ENT.0000000000000004")}},
		// Not a user ID: the message is still accepted, the claim dropped.
		"malformed ID": {"incoming_text_phone_user_id.form", func(b string) string {
			return strings.Replace(b, "US.0000000000000003", "not+an+id", 1)
		}, []model.Attribute{phone}},
		// A number that is not E.164 is no attribute.
		"unusable number": {"incoming_text.form", func(b string) string {
			b = strings.Replace(b, "WaId=15558675310&", "", 1)
			return strings.Replace(b, "From=whatsapp%3A%2B15558675310", "From=whatsapp%3A0155", 1)
		}, nil},
		// With no usable From, WaId is the number.
		"number from WaId": {"incoming_text.form", func(b string) string {
			return strings.Replace(b, "From=whatsapp%3A%2B15558675310", "From=whatsapp%3A0155", 1)
		}, []model.Attribute{phone}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sink := &recordingSink{}
			body := fixture(t, tc.fixture)
			if tc.edit != nil {
				body = []byte(tc.edit(string(body)))
			}
			if rec := post(hook(t, &tokenHost{}, nil, sink), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
				t.Fatalf("status = %d %q", rec.Code, rec.Body)
			}
			if got := sink.got[0].Envelope.SenderAttributes; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sender attributes %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A status callback names no sender and reports no attributes.
func TestWebhookStatusCarriesNoSenderAttributes(t *testing.T) {
	sink := &recordingSink{}
	body := fixture(t, "status_read.form")
	if rec := post(hook(t, &tokenHost{}, nil, sink), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q", rec.Code, rec.Body)
	}
	if env := sink.got[0].Envelope; env.SenderAttributes != nil {
		t.Fatalf("status attributes %+v", env.SenderAttributes)
	}
}
