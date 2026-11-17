package slack

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// A modal submission is identified by its view and its trigger_id, never by
// when it arrived: Slack delivering the same payload again is the same
// interaction (the engine's duplicate check keys on the channel and the
// event ID), while each submit of a view, which Slack gives a trigger of its
// own, is another.

func submissionWith(trigger, value string) map[string]any {
	p := submissionPayload()
	if trigger != "" {
		p["trigger_id"] = trigger
	}
	view := p["view"].(map[string]any)
	view["state"] = map[string]any{"values": map[string]any{"b": map[string]any{"a": map[string]any{"type": "plain_text_input", "value": value}}}}
	return p
}

// eventIDs are the event IDs the sink recorded, in order.
func eventIDs(s *sinkRecorder) []string {
	var ids []string
	for _, e := range s.envelopes() {
		ids = append(ids, e.EventID)
	}
	return ids
}

func wantSameAndDistinct(t *testing.T, ids []string) {
	t.Helper()
	if len(ids) != 3 {
		t.Fatalf("recorded %q, want three envelopes", ids)
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, "view:VNHU13V36:") {
			t.Errorf("ID %q does not name the view", id)
		}
	}
	if ids[0] != ids[1] {
		t.Errorf("a delivery again is %q, then %q: want one ID, so the engine takes it for a duplicate", ids[0], ids[1])
	}
	if ids[2] == ids[0] {
		t.Errorf("a second submit of the view has the first's ID %q", ids[2])
	}
}

func TestSubmissionDeliveredAgainAtTheRequestURLHasOneID(t *testing.T) {
	for name, triggers := range map[string][2]string{
		"trigger_id":    {"12345.98765.abcd2358fdea", "12345.98766.ef012358fdeb"},
		"no trigger_id": {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			x, s := open(t, testConfig()), &sinkRecorder{}
			first := interactionPayload(submissionWith(triggers[0], "great"))
			again := first
			again.at = testNow.Add(2 * time.Second) // a later request of the same bytes
			other := interactionPayload(submissionWith(triggers[1], "even better"))
			other.at = testNow.Add(5 * time.Second)
			for _, req := range []request{first, again, other} {
				if res := serve(t, x, s, req); res.Code != http.StatusOK {
					t.Fatalf("answered %d", res.Code)
				}
			}
			wantSameAndDistinct(t, eventIDs(s))
		})
	}
}

func TestSubmissionDeliveredAgainOverTheSocketHasOneID(t *testing.T) {
	r := newSocketRig(t, Options{})
	// The host's clock moves on between envelopes, as it does between a
	// delivery and Slack's next.
	var mu sync.Mutex
	at := testNow
	r.x.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at = at.Add(time.Second)
		return at
	}
	r.run(t)
	c := r.a.nextConn(t)
	payload := submissionWith("12345.98765.abcd2358fdea", "great")
	// The trigger names the submission, not the bytes: a delivery again
	// that differs in a field the adapter does not read is the same one.
	again := submissionWith("12345.98765.abcd2358fdea", "great")
	again["is_enterprise_install"] = false
	for _, p := range []map[string]any{payload, again, submissionWith("12345.98766.ef012358fdeb", "even better")} {
		c.envelope(t, "interactive", p, true)
		c.ack(t)
	}
	wantSameAndDistinct(t, eventIDs(r.sink))
}
