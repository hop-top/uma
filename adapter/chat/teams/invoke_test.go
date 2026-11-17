package teams

import (
	"net/http"
	"testing"

	"hop.top/uma/model"
)

func (r *rig) execute(id, trigger string) map[string]any {
	a := r.activityOf("invoke", id, testUser, testTeamChannel+";messageid=500", "channel")
	a["name"], a["replyToId"] = ActionExecuteInvoke, "777"
	a["value"] = map[string]any{
		"action":  map[string]any{"type": "Action.Execute", "id": "btn-1", "verb": "approve", "data": map[string]any{"ticket": 42}},
		"trigger": trigger,
	}
	return a
}

func TestActionExecuteRecordedThenAcknowledged(t *testing.T) {
	r := newRig(t)
	var recordedFirst bool
	r.sink.fail = func(model.Ingress) error { recordedFirst = true; return nil }
	rec := r.post(t, r.execute("f:inv-1", "manual"))
	if rec.Code != http.StatusOK || rec.Body.String() != string(invokeAck) || !recordedFirst {
		t.Fatalf("answer %d %q, want the invoke response after the record", rec.Code, rec.Body.String())
	}
	env := only(t, r)
	in := env.Interaction
	if env.Kind != model.EnvelopeInteraction || in == nil {
		t.Fatalf("envelope %+v", env)
	}
	card := model.ProviderMessageRef(testChannel, testTeamChannel+"|777")
	switch {
	case in.Kind != model.InteractionAction || in.Action != "approve" || in.Arguments != `{"ticket":42}`:
		t.Fatalf("interaction %+v", in)
	case in.Actor != r.x.userRef(testUser) || env.Sender != in.Actor:
		t.Fatalf("actor %+v", in.Actor)
	case in.Message == nil || *in.Message != card:
		t.Fatalf("message %+v, want the card's", in.Message)
	case in.ResponseToken != "" || !in.ResponseExpiresAt.IsZero():
		t.Fatal("Teams gives no response credential")
	case !in.Deadline.Equal(testNow.Add(AckDeadline)):
		t.Fatalf("deadline %v", in.Deadline)
	case len(env.References) != 2 || env.References[1] != r.x.threadRef(testTeamChannel, "500"):
		t.Fatalf("references %+v", env.References)
	}
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	// Teams retries an unanswered invoke: the same activity is the same
	// interaction.
	r.post(t, r.execute("f:inv-1", "manual"))
	if envs := r.sink.envelopes(); envs[1].EventID != env.EventID {
		t.Fatalf("redelivery event %s, want %s", envs[1].EventID, env.EventID)
	}
}

func TestActionExecuteWithoutVerbUsesItsID(t *testing.T) {
	r := newRig(t)
	a := r.execute("f:inv-1", "manual")
	a["value"] = map[string]any{"action": map[string]any{"type": "Action.Execute", "id": "btn-1"}, "trigger": "manual"}
	r.post(t, a)
	if in := only(t, r).Interaction; in.Action != "btn-1" || in.Arguments != "" {
		t.Fatalf("interaction %+v", in)
	}
	str := r.execute("f:inv-3", "manual")
	str["value"] = map[string]any{"action": map[string]any{"type": "Action.Execute", "verb": "go", "data": "plain words"}, "trigger": "manual"}
	r.post(t, str)
	if in := r.sink.envelopes()[1].Interaction; in.Arguments != "plain words" {
		t.Fatalf("arguments %q, want a string's data as it is", in.Arguments)
	}
	empty := r.execute("f:inv-2", "manual")
	empty["value"] = map[string]any{"action": map[string]any{"type": "Action.Execute"}, "trigger": "manual"}
	if rec := r.post(t, empty); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for an action naming nothing", rec.Code)
	}
}

func TestAutomaticRefreshNotRecorded(t *testing.T) {
	r := newRig(t)
	rec := r.post(t, r.execute("f:inv-1", "automatic"))
	if rec.Code != http.StatusOK || rec.Body.String() != string(invokeAck) {
		t.Fatalf("answer %d %q", rec.Code, rec.Body.String())
	}
	if n := len(r.sink.envelopes()); n != 0 {
		t.Fatalf("envelopes %d, want none for a refresh", n)
	}
}

func TestOtherInvokesNotImplemented(t *testing.T) {
	r := newRig(t)
	for _, name := range []string{"task/fetch", "task/submit", "composeExtension/query"} {
		a := r.activityOf("invoke", "f:inv", testUser, testPersonal, "personal")
		a["name"], a["value"] = name, map[string]any{"data": map[string]any{}}
		if rec := r.post(t, a); rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s: status %d, want 501", name, rec.Code)
		}
	}
	if n := len(r.sink.envelopes()); n != 0 {
		t.Fatalf("envelopes %d", n)
	}
}

func TestInvokeRefusedBySinkIsNotAcknowledged(t *testing.T) {
	r := newRig(t)
	r.sink.fail = func(model.Ingress) error { return model.NewError(model.Unavailable, "down") }
	if rec := r.post(t, r.execute("f:inv-1", "manual")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 with no invoke response", rec.Code)
	}
}
