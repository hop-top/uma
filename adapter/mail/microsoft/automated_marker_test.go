package microsoft

import (
	"encoding/json"
	"testing"

	"hop.top/uma/model"
)

// The Auto-Submitted header is recorded in the payload as headers.auto_submitted,
// the shape the other mail adapters use, so a host can tell automated mail
// apart; the envelope kind is unchanged.
func TestInboundRecordsAutoSubmitted(t *testing.T) {
	cases := []struct {
		name string
		hdrs []gHeader
		want string
		kind string
	}{
		{"auto-replied", []gHeader{{Name: "Auto-Submitted", Value: "auto-replied"}}, "auto-replied", model.EnvelopeAutomatedMessage},
		{"auto-generated", []gHeader{{Name: "auto-submitted", Value: "auto-generated"}}, "auto-generated", model.EnvelopeAutomatedMessage},
		{"no", []gHeader{{Name: "Auto-Submitted", Value: "no"}}, "no", model.EnvelopeMessage},
		{"absent", nil, "", model.EnvelopeMessage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, Config{})
			e.g.deliver(e.box.name, gMessage{
				ID: "m1", From: &gRecipient{EmailAddress: gEmailAddress{Address: "peer@example.test"}},
				Body:                   &gItemBody{ContentType: "text", Content: "hi"},
				InternetMessageHeaders: c.hdrs,
			})
			env := fetch(t, e, "m1")
			if env.Kind != c.kind {
				t.Errorf("kind %q, want %q", env.Kind, c.kind)
			}
			var wire struct {
				Headers struct {
					AutoSubmitted string `json:"auto_submitted"`
				} `json:"headers"`
			}
			if err := json.Unmarshal(env.Payload.Data, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Headers.AutoSubmitted != c.want {
				t.Errorf("headers.auto_submitted = %q, want %q", wire.Headers.AutoSubmitted, c.want)
			}
		})
	}
}
