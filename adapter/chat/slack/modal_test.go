package slack

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"hop.top/uma/model"
)

func evalConfig() Config {
	c := testConfig()
	c.EvaluateInteractions = true
	return c
}

func submissionPayload() map[string]any {
	return map[string]any{
		"type": "view_submission", "api_app_id": testApp, "team": map[string]any{"id": testTeam}, "user": map[string]any{"id": testPerson},
		"view": map[string]any{
			"id": "VNHU13V36", "type": "modal", "callback_id": "feedback", "private_metadata": "ticket-9",
			"state":         map[string]any{"values": map[string]any{"b": map[string]any{"a": map[string]any{"type": "plain_text_input", "value": "great"}}}},
			"response_urls": []any{map[string]any{"block_id": "b", "action_id": "c", "channel_id": testRoom, "response_url": hookURL}},
		},
	}
}

func jsonBody(t *testing.T, res *httpResult) map[string]any {
	t.Helper()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res.Body), &m); err != nil {
		t.Fatalf("body %q is not JSON: %v", res.Body, err)
	}
	return m
}

func TestEvaluateInteractionsConfig(t *testing.T) {
	c := validConfig()
	c.EvaluateInteractions = true
	if issues := NewFactory(Options{}).Validate(c.Extension()); len(issues) > 0 {
		t.Fatalf("refused: %+v", issues)
	}
	got, issues := parseConfig(c.Extension())
	if len(issues) > 0 || !got.EvaluateInteractions {
		t.Fatalf("parsed %+v %+v, want evaluate_interactions kept", got, issues)
	}
}

func TestSubmissionIsEvaluatedBeforeItIsRecorded(t *testing.T) {
	x, s := open(t, evalConfig()), &sinkRecorder{}
	s.eval = func(model.InteractionInput) (model.InteractionDecision, error) {
		if got := s.got; len(got) != 0 {
			t.Errorf("evaluated after recording %d envelopes: the evaluation comes first", len(got))
		}
		return model.InteractionDecision{}, nil
	}
	res := serve(t, x, s, interactionPayload(submissionPayload()))
	if res.Code != http.StatusOK || res.Body != "" {
		t.Fatalf("answered %d %q, want 200 and an empty body: that closes the modal", res.Code, res.Body)
	}
	e := wantOne(t, s)
	if len(s.evals) != 1 {
		t.Fatalf("evaluated %d times, want once", len(s.evals))
	}
	in := s.evals[0]
	if in.Kind != model.InteractionSubmission || in.ID != e.Interaction.ID || in.Fields.Name != "slack.chat.view.feedback" || in.Actor != x.userRef(testPerson) {
		t.Errorf("evaluated %+v, want the submission as recorded", in)
	}
	if !in.Deadline.Equal(testNow.Add(AckDeadline)) {
		t.Errorf("deadline %v, want the acknowledgement deadline", in.Deadline)
	}
	if in.ResponseToken != "" || !in.ResponseExpiresAt.IsZero() {
		t.Errorf("validator was handed the response token %q: it reads the submission, not the means to answer it", in.ResponseToken)
	}
	if e.Interaction.ResponseToken != hookTok {
		t.Errorf("recorded token = %q, want the response URL's path still kept for the record", e.Interaction.ResponseToken)
	}
	if err := in.Validate(); err != nil {
		t.Errorf("evaluated interaction invalid: %v", err)
	}
}

func TestSubmissionFieldErrorsAreAnsweredAndNotRecorded(t *testing.T) {
	x, s := open(t, evalConfig()), &sinkRecorder{}
	s.eval = func(model.InteractionInput) (model.InteractionDecision, error) {
		return model.InteractionDecision{FieldErrors: map[string]string{"b": "too short", "due": "in the past"}}, nil
	}
	res := serve(t, x, s, interactionPayload(submissionPayload()))
	if res.Code != http.StatusOK {
		t.Fatalf("answered %d %q, want 200", res.Code, res.Body)
	}
	m := jsonBody(t, res)
	errs, _ := m["errors"].(map[string]any)
	if m["response_action"] != "errors" || len(m) != 2 || len(errs) != 2 || errs["b"] != "too short" || errs["due"] != "in the past" {
		t.Fatalf("body = %s, want response_action errors keyed by block_id", res.Body)
	}
	wantNone(t, s)
}

func TestSubmissionDecisionThatSlackCannotCarryIsRefused(t *testing.T) {
	for name, fe := range map[string]map[string]string{
		"empty key":       {"": "bad"},
		"empty message":   {"b": ""},
		"key past 255":    {strings.Repeat("k", 256): "bad"},
		"blank message":   {"b": "   "},
		"key with a null": {"b\x00": "bad"},
	} {
		t.Run(name, func(t *testing.T) {
			x, s := open(t, evalConfig()), &sinkRecorder{}
			s.eval = func(model.InteractionInput) (model.InteractionDecision, error) {
				return model.InteractionDecision{FieldErrors: fe}, nil
			}
			res := serve(t, x, s, interactionPayload(submissionPayload()))
			if res.Code != http.StatusUnprocessableEntity {
				t.Fatalf("answered %d %q, want 422: an invalid decision never accepts the submission", res.Code, res.Body)
			}
			wantNone(t, s)
		})
	}
}

func TestSubmissionEvaluationFailuresFailClosed(t *testing.T) {
	cases := map[string]struct {
		err  error
		code int
	}{
		"no validator or denied": {model.NewError(model.Unauthorized, "denied"), http.StatusForbidden},
		"unavailable":            {model.NewError(model.Unavailable, "busy"), http.StatusServiceUnavailable},
		"deadline":               {model.NewError(model.DeadlineExpired, "late"), http.StatusGatewayTimeout},
		"refused":                {model.NewError(model.Validation, "no"), http.StatusUnprocessableEntity},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			x, s := open(t, evalConfig()), &sinkRecorder{}
			s.eval = func(model.InteractionInput) (model.InteractionDecision, error) {
				return model.InteractionDecision{}, tc.err
			}
			res := serve(t, x, s, interactionPayload(submissionPayload()))
			if res.Code != tc.code {
				t.Fatalf("answered %d %q, want %d", res.Code, res.Body, tc.code)
			}
			wantNone(t, s)
		})
	}
}

func TestSubmissionIsNotEvaluatedUnlessConfigured(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	s.eval = func(model.InteractionInput) (model.InteractionDecision, error) {
		return model.InteractionDecision{FieldErrors: map[string]string{"b": "no"}}, nil
	}
	res := serve(t, x, s, interactionPayload(submissionPayload()))
	if res.Code != http.StatusOK || res.Body != "" || len(s.evals) != 0 {
		t.Fatalf("answered %d %q after %d evaluations, want the unevaluated empty 200", res.Code, res.Body, len(s.evals))
	}
	wantOne(t, s)
}

func TestSubmissionOfAnotherTeamIsDroppedUnevaluated(t *testing.T) {
	x, s := open(t, evalConfig()), &sinkRecorder{}
	p := submissionPayload()
	p["team"] = map[string]any{"id": "T0OTHER"}
	res := serve(t, x, s, interactionPayload(p))
	if res.Code != http.StatusOK || res.Body != "" || len(s.evals) != 0 {
		t.Fatalf("answered %d %q after %d evaluations", res.Code, res.Body, len(s.evals))
	}
	wantNone(t, s)
}

func TestOnlyASubmissionIsEvaluated(t *testing.T) {
	x, s := open(t, evalConfig()), &sinkRecorder{}
	s.eval = func(model.InteractionInput) (model.InteractionDecision, error) {
		return model.InteractionDecision{FieldErrors: map[string]string{"b": "no"}}, nil
	}
	if res := serve(t, x, s, interactionPayload(buttonPayload())); res.Code != http.StatusOK || res.Body != "" {
		t.Fatalf("a button answered %d %q, want the empty 200", res.Code, res.Body)
	}
	wantOne(t, s)
	if len(s.evals) != 0 {
		t.Fatalf("evaluated %d times: a press has no field to validate", len(s.evals))
	}
}
