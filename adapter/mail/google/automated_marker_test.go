package google

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
		name  string
		extra []string
		want  string
		kind  string
	}{
		{"auto-replied", []string{"Auto-Submitted: auto-replied"}, "auto-replied", model.EnvelopeAutomatedMessage},
		{"auto-generated", []string{"Auto-Submitted: auto-generated"}, "auto-generated", model.EnvelopeAutomatedMessage},
		{"no", []string{"Auto-Submitted: no"}, "no", model.EnvelopeMessage},
		{"absent", nil, "", model.EnvelopeMessage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, Config{})
			e.inbound("m1", "peer@example.test", "hi", c.extra...)
			in, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), &stagingSink{}, "m1")
			if err != nil || skip {
				t.Fatalf("fetchIngress = %v %v", skip, err)
			}
			if in.Envelope.Kind != c.kind {
				t.Errorf("kind %q, want %q", in.Envelope.Kind, c.kind)
			}
			var wire struct {
				Headers struct {
					AutoSubmitted string `json:"auto_submitted"`
				} `json:"headers"`
			}
			if err := json.Unmarshal(in.Envelope.Payload.Data, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Headers.AutoSubmitted != c.want {
				t.Errorf("headers.auto_submitted = %q, want %q", wire.Headers.AutoSubmitted, c.want)
			}
		})
	}
}

// A message too large to fetch raw is read by its metadata headers, and
// records the header the same way.
func TestHeaderOnlyRecordsAutoSubmitted(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", "peer@example.test", "hi", "Auto-Submitted: auto-replied")
	in, skip, err := e.inst.fetchHeaderOnly(t.Context(), e.inst.c.session(), "m1")
	if err != nil || skip {
		t.Fatalf("fetchHeaderOnly = %v %v", skip, err)
	}
	var wire struct {
		Headers struct {
			AutoSubmitted string `json:"auto_submitted"`
		} `json:"headers"`
	}
	if err := json.Unmarshal(in.Envelope.Payload.Data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Headers.AutoSubmitted != "auto-replied" || in.Envelope.Kind != model.EnvelopeAutomatedMessage {
		t.Errorf("auto_submitted %q kind %q", wire.Headers.AutoSubmitted, in.Envelope.Kind)
	}
}
