package slack

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"hop.top/uma/model"
)

func suggestionPayload(mod ...func(map[string]any)) map[string]any {
	p := map[string]any{
		"type": "block_suggestion", "api_app_id": testApp, "team": map[string]any{"id": testTeam, "domain": "acme"},
		"user":      map[string]any{"id": testPerson, "username": "ann", "team_id": testTeam},
		"container": map[string]any{"type": "message", "message_ts": testTS, "channel_id": testRoom},
		"block_id":  "assignee", "action_id": "pick-user", "value": "an",
	}
	for _, m := range mod {
		m(p)
	}
	return p
}

func optionsExt(t *testing.T, v any) model.Extension {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return model.Extension{Name: OptionsSchema, Version: OptionsSchemaVersion, Data: b}
}

func opt(text, value string) map[string]any {
	return map[string]any{"text": map[string]any{"type": "plain_text", "text": text}, "value": value}
}

func TestBlockSuggestionIsAnsweredWithTheValidatorsOptions(t *testing.T) {
	x, s := open(t, evalConfig()), &sinkRecorder{}
	s.eval = func(in model.InteractionInput) (model.InteractionDecision, error) {
		return model.InteractionDecision{Options: optionsExt(t, map[string]any{"options": []any{
			opt("Ann", "U0ANN"), map[string]any{"text": map[string]any{"type": "plain_text", "text": "Andy", "emoji": true}, "value": "U0ANDY",
				"description": map[string]any{"type": "plain_text", "text": "on call"}},
		}})}, nil
	}
	res := serve(t, x, s, interactionPayload(suggestionPayload()))
	if res.Code != http.StatusOK {
		t.Fatalf("answered %d %q", res.Code, res.Body)
	}
	m := jsonBody(t, res)
	opts, _ := m["options"].([]any)
	if len(m) != 1 || len(opts) != 2 {
		t.Fatalf("body = %s, want two options and nothing else", res.Body)
	}
	if o, _ := opts[1].(map[string]any); o["value"] != "U0ANDY" || o["description"] == nil {
		t.Errorf("second option = %v, want its value and description kept", opts[1])
	}
	wantNone(t, s)
	if len(s.evals) != 1 {
		t.Fatalf("evaluated %d times, want once", len(s.evals))
	}
	in := s.evals[0]
	if in.Kind != model.InteractionOptions || in.Action != "pick-user" || in.Arguments != "an" || in.Actor != x.userRef(testPerson) {
		t.Errorf("evaluated %+v, want the options request: action, typed text, actor", in)
	}
	if in.Fields.Name != SuggestionSchema || in.Fields.Version != SuggestionSchemaVersion {
		t.Errorf("fields = %+v, want the suggestion schema", in.Fields)
	}
	var data map[string]any
	if err := json.Unmarshal(in.Fields.Data, &data); err != nil || data["block_id"] != "assignee" || data["action_id"] != "pick-user" || data["value"] != "an" {
		t.Errorf("fields data = %s, %v", in.Fields.Data, err)
	}
	if len(in.Context) != 1 || in.Context[0] != x.roomRef(testRoom) {
		t.Errorf("context = %+v, want the room of the message", in.Context)
	}
	if !in.Deadline.Equal(testNow.Add(AckDeadline)) {
		t.Errorf("deadline %v, want the acknowledgement deadline", in.Deadline)
	}
	if err := in.Validate(); err != nil {
		t.Errorf("evaluated interaction invalid: %v", err)
	}
}

func TestBlockSuggestionEvaluationFailuresFailClosed(t *testing.T) {
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
			res := serve(t, x, s, interactionPayload(suggestionPayload()))
			if res.Code != tc.code {
				t.Fatalf("answered %d %q, want %d", res.Code, res.Body, tc.code)
			}
			wantNone(t, s)
		})
	}
}

func TestBlockSuggestionInAModalCarriesTheViewState(t *testing.T) {
	x, s := open(t, evalConfig()), &sinkRecorder{}
	p := suggestionPayload(func(p map[string]any) {
		delete(p, "container")
		p["view"] = map[string]any{
			"id": "V1", "type": "modal", "callback_id": "Assign-Ticket", "private_metadata": "t-9",
			"state": map[string]any{"values": map[string]any{"team": map[string]any{"pick": map[string]any{"type": "static_select"}}}},
		}
	})
	serve(t, x, s, interactionPayload(p))
	if len(s.evals) != 1 {
		t.Fatalf("evaluated %d times, want once", len(s.evals))
	}
	var data map[string]any
	if err := json.Unmarshal(s.evals[0].Fields.Data, &data); err != nil || data["view_id"] != "V1" || data["callback_id"] != "Assign-Ticket" ||
		data["private_metadata"] != "t-9" || data["values"] == nil {
		t.Fatalf("fields data = %s, %v, want the view and its state so options can depend on other fields", s.evals[0].Fields.Data, err)
	}
	if len(s.evals[0].Context) != 0 {
		t.Errorf("context = %+v, want none: a view names no room", s.evals[0].Context)
	}
	if err := s.evals[0].Validate(); err != nil {
		t.Errorf("interaction invalid: %v", err)
	}
}

func TestBlockSuggestionOptionGroups(t *testing.T) {
	x, s := open(t, evalConfig()), &sinkRecorder{}
	s.eval = func(model.InteractionInput) (model.InteractionDecision, error) {
		return model.InteractionDecision{Options: optionsExt(t, map[string]any{"option_groups": []any{
			map[string]any{"label": map[string]any{"type": "plain_text", "text": "People"}, "options": []any{opt("Ann", "U0ANN")}},
		}})}, nil
	}
	m := jsonBody(t, serve(t, x, s, interactionPayload(suggestionPayload())))
	groups, _ := m["option_groups"].([]any)
	if len(m) != 1 || len(groups) != 1 {
		t.Fatalf("body = %v, want one option group and nothing else", m)
	}
	if g, _ := groups[0].(map[string]any); g["label"] == nil || len(g["options"].([]any)) != 1 {
		t.Errorf("group = %v", groups[0])
	}
}

func TestBlockSuggestionWithNoOptionsIsAnExplicitEmptyList(t *testing.T) {
	for name, mod := range map[string]func(*Config){
		"evaluated, nothing to offer": func(*Config) {},
		"not configured":              func(c *Config) { c.EvaluateInteractions = false },
	} {
		t.Run(name, func(t *testing.T) {
			c := evalConfig()
			mod(&c)
			x, s := open(t, c), &sinkRecorder{}
			s.eval = func(model.InteractionInput) (model.InteractionDecision, error) {
				return model.InteractionDecision{FieldErrors: map[string]string{"x": "ignored"}}, nil
			}
			res := serve(t, x, s, interactionPayload(suggestionPayload()))
			if res.Code != http.StatusOK {
				t.Fatalf("answered %d %q", res.Code, res.Body)
			}
			m := jsonBody(t, res)
			if opts, ok := m["options"].([]any); !ok || len(opts) != 0 || len(m) != 1 {
				t.Fatalf("body = %s, want {\"options\":[]}", res.Body)
			}
			wantNone(t, s)
			if !c.EvaluateInteractions && len(s.evals) != 0 {
				t.Errorf("evaluated %d times with the validator not configured", len(s.evals))
			}
		})
	}
}

func TestBlockSuggestionOfAnotherTeamGetsNoOptions(t *testing.T) {
	x, s := open(t, evalConfig()), &sinkRecorder{}
	p := suggestionPayload(func(p map[string]any) { p["api_app_id"] = "A0OTHER" })
	res := serve(t, x, s, interactionPayload(p))
	if m := jsonBody(t, res); res.Code != http.StatusOK || len(m["options"].([]any)) != 0 || len(s.evals) != 0 {
		t.Fatalf("answered %d %q after %d evaluations, want an empty list unevaluated", res.Code, res.Body, len(s.evals))
	}
}

func TestBlockSuggestionNeedsAUserAndAnAction(t *testing.T) {
	x, s := open(t, evalConfig()), &sinkRecorder{}
	for name, mod := range map[string]func(map[string]any){
		"no user":      func(p map[string]any) { delete(p, "user") },
		"user not ID":  func(p map[string]any) { p["user"] = map[string]any{"id": "ann"} },
		"no action ID": func(p map[string]any) { delete(p, "action_id") },
	} {
		t.Run(name, func(t *testing.T) {
			if res := serve(t, x, s, interactionPayload(suggestionPayload(mod))); res.Code != http.StatusBadRequest {
				t.Fatalf("answered %d %q, want 400", res.Code, res.Body)
			}
		})
	}
	if len(s.evals) != 0 {
		t.Errorf("evaluated %d malformed requests", len(s.evals))
	}
}

func TestOptionsSlackCannotCarryAreRefused(t *testing.T) {
	many := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = opt("o", "v")
		}
		return out
	}
	group := func(label string, opts []any) map[string]any {
		return map[string]any{"label": map[string]any{"type": "plain_text", "text": label}, "options": opts}
	}
	for name, data := range map[string]any{
		"more than 100 options":          map[string]any{"options": many(101)},
		"more than 100 groups":           map[string]any{"option_groups": repeat(101, group("g", many(1)))},
		"more than 100 in a group":       map[string]any{"option_groups": []any{group("g", many(101))}},
		"options and groups":             map[string]any{"options": many(1), "option_groups": []any{group("g", many(1))}},
		"neither":                        map[string]any{},
		"text past 75":                   map[string]any{"options": []any{opt(strings.Repeat("t", 76), "v")}},
		"empty text":                     map[string]any{"options": []any{opt("", "v")}},
		"blank text":                     map[string]any{"options": []any{opt("   ", "v")}},
		"value past 150":                 map[string]any{"options": []any{opt("t", strings.Repeat("v", 151))}},
		"empty value":                    map[string]any{"options": []any{opt("t", "")}},
		"markdown text":                  map[string]any{"options": []any{map[string]any{"text": map[string]any{"type": "mrkdwn", "text": "t"}, "value": "v"}}},
		"description past 75":            map[string]any{"options": []any{map[string]any{"text": map[string]any{"type": "plain_text", "text": "t"}, "value": "v", "description": map[string]any{"type": "plain_text", "text": strings.Repeat("d", 76)}}}},
		"group label past 75":            map[string]any{"option_groups": []any{group(strings.Repeat("l", 76), many(1))}},
		"group without options":          map[string]any{"option_groups": []any{group("g", nil)}},
		"unknown option field":           map[string]any{"options": []any{map[string]any{"text": map[string]any{"type": "plain_text", "text": "t"}, "value": "v", "url": "https://x.example"}}},
		"unknown top-level field":        map[string]any{"options": many(1), "extra": true},
		"options is not a list":          map[string]any{"options": "x"},
		"no text":                        map[string]any{"options": []any{map[string]any{"value": "v"}}},
		"another schema is not an offer": nil,
	} {
		t.Run(name, func(t *testing.T) {
			x, s := open(t, evalConfig()), &sinkRecorder{}
			s.eval = func(model.InteractionInput) (model.InteractionDecision, error) {
				if data == nil {
					return model.InteractionDecision{Options: model.Extension{Name: "org.example.options", Version: 1, Data: json.RawMessage(`{"options":[]}`)}}, nil
				}
				return model.InteractionDecision{Options: optionsExt(t, data)}, nil
			}
			res := serve(t, x, s, interactionPayload(suggestionPayload()))
			if res.Code != http.StatusUnprocessableEntity {
				t.Fatalf("answered %d %q, want 422: Slack would drop the list", res.Code, res.Body)
			}
		})
	}
}

func TestOptionsAtSlacksLimitsAreKept(t *testing.T) {
	x, s := open(t, evalConfig()), &sinkRecorder{}
	opts := make([]any, 100)
	for i := range opts {
		opts[i] = opt(strings.Repeat("é", 75), strings.Repeat("v", 150))
	}
	s.eval = func(model.InteractionInput) (model.InteractionDecision, error) {
		return model.InteractionDecision{Options: optionsExt(t, map[string]any{"options": opts})}, nil
	}
	res := serve(t, x, s, interactionPayload(suggestionPayload()))
	if res.Code != http.StatusOK || len(jsonBody(t, res)["options"].([]any)) != 100 {
		t.Fatalf("answered %d: 100 options of 75 characters and 150-byte values are Slack's maxima", res.Code)
	}
}

func repeat(n int, v any) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = v
	}
	return out
}
