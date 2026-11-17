package slack

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// formRequest is a signed form request: an interaction (payload=JSON) or a
// slash command, as https://docs.slack.dev/interactivity/ describes.
func formRequest(v url.Values) request {
	return request{contentType: "application/x-www-form-urlencoded", body: []byte(v.Encode()), at: testNow, secret: testSecret, method: http.MethodPost}
}

func interactionPayload(p map[string]any) request {
	b, _ := json.Marshal(p)
	return formRequest(url.Values{"payload": {string(b)}})
}

const (
	hookURL = "https://hooks.slack.com/actions/T0001/397700885554/96rGlfmibIGlgcZRskXaIFfN"
	hookTok = "actions/T0001/397700885554/96rGlfmibIGlgcZRskXaIFfN"
)

func buttonPayload(mod ...func(map[string]any)) map[string]any {
	p := map[string]any{
		"type": "block_actions", "api_app_id": testApp, "trigger_id": "398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c",
		"team": map[string]any{"id": testTeam, "domain": "acme"}, "user": map[string]any{"id": testPerson, "username": "ann", "team_id": testTeam},
		"container":    map[string]any{"type": "message", "message_ts": testTS, "channel_id": testRoom, "is_ephemeral": false},
		"channel":      map[string]any{"id": testRoom, "name": "ops"},
		"message":      map[string]any{"type": "message", "ts": testTS, "user": testBotUser},
		"response_url": hookURL,
		"actions": []any{map[string]any{
			"action_id": "approve", "block_id": "b1", "type": "button", "value": "deploy-42", "action_ts": "1700000001.000200",
			"text": map[string]any{"type": "plain_text", "text": "Approve"},
		}},
	}
	for _, m := range mod {
		m(p)
	}
	return p
}

func TestButtonPressIsAnActionInteraction(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	res := serve(t, x, s, interactionPayload(buttonPayload()))
	if res.Code != http.StatusOK || res.Body != "" {
		t.Fatalf("answered %d %q, want 200 and an empty body", res.Code, res.Body)
	}
	e := wantOne(t, s)
	in := e.Interaction
	if e.Kind != model.EnvelopeInteraction || in == nil || e.EventID != in.ID || e.Destination != testTeam || e.Assurance != "verified" {
		t.Fatalf("envelope = %+v", e)
	}
	if in.ID != "action:398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c" || in.Kind != model.InteractionAction || in.Action != "approve" || in.Arguments != "deploy-42" {
		t.Errorf("interaction = %+v, want the action, its value and the trigger as ID", in)
	}
	if in.Actor != x.userRef(testPerson) || e.Sender != in.Actor || e.SenderKind != "" {
		t.Errorf("actor = %+v sender %+v, want the signed payload's user", in.Actor, e.Sender)
	}
	if in.Bot == nil || *in.Bot != x.userRef(testBotUser) {
		t.Errorf("bot = %+v, want the app's bot user", in.Bot)
	}
	wantMsg := model.ProviderMessageRef(testChannel, testRoom+":"+testTS)
	if in.Message == nil || *in.Message != wantMsg {
		t.Errorf("message = %+v, want %+v", in.Message, wantMsg)
	}
	if len(e.References) != 2 || e.References[0] != x.roomRef(testRoom) || e.References[1] != x.threadRef(testRoom, testTS) || !slices.Equal(in.Context, e.References) {
		t.Errorf("references %+v context %+v, want the room and the thread the control's message roots", e.References, in.Context)
	}
	if in.ResponseToken != hookTok {
		t.Errorf("token = %q, want the response URL's path", in.ResponseToken)
	}
	if !in.ResponseExpiresAt.Equal(testNow.Add(ResponseWindow)) || !in.Deadline.Equal(testNow.Add(AckDeadline)) {
		t.Errorf("expires %v deadline %v, want 30 minutes and 3 seconds from the request", in.ResponseExpiresAt, in.Deadline)
	}
	if err := in.Validate(); err != nil {
		t.Errorf("interaction invalid: %v", err)
	}
	if !e.OccurredAt.Equal(slackTime("1700000001.000200")) {
		t.Errorf("occurred at %v, want the action's time", e.OccurredAt)
	}
}

func TestInteractionInAThreadNamesTheThread(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	p := buttonPayload(func(p map[string]any) {
		p["message"] = map[string]any{"type": "message", "ts": "1700000050.000300", "thread_ts": testTS}
		p["container"].(map[string]any)["message_ts"] = "1700000050.000300"
	})
	serve(t, x, s, interactionPayload(p))
	e := wantOne(t, s)
	if len(e.References) != 2 || e.References[1] != x.threadRef(testRoom, testTS) || len(e.Interaction.Context) != 2 {
		t.Fatalf("references %+v context %+v, want the room and the thread", e.References, e.Interaction.Context)
	}
}

func TestSelectMenuArgumentIsTheSelectedValue(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	p := buttonPayload(func(p map[string]any) {
		p["actions"] = []any{map[string]any{"action_id": "pick", "block_id": "b2", "type": "static_select", "action_ts": "1700000001.000200",
			"selected_option": map[string]any{"text": map[string]any{"type": "plain_text", "text": "Blue"}, "value": "blue"}}}
	})
	serve(t, x, s, interactionPayload(p))
	if in := wantOne(t, s).Interaction; in.Action != "pick" || in.Arguments != "blue" {
		t.Fatalf("interaction = %+v, want the pick action with the option's value", in)
	}
}

func TestControlOfAnEphemeralMessageHasNoMessage(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	serve(t, x, s, interactionPayload(buttonPayload(func(p map[string]any) {
		p["container"].(map[string]any)["is_ephemeral"] = true
		delete(p, "message")
	})))
	if in := wantOne(t, s).Interaction; in.Message != nil {
		t.Fatalf("message = %+v, want none: an ephemeral message has no ts to name", in.Message)
	}
}

func TestResponseURLOfAnotherHostIsNotKept(t *testing.T) {
	for name, u := range map[string]string{ // #nosec G101 -- fixture URLs, one with user info
		"another host":      "https://evil.example/actions/T0001/1/2",
		"plain http":        "http://hooks.slack.com/actions/T0001/1/2",
		"user info":         "https://u:p@hooks.slack.com/actions/T0001/1/2",
		"a query":           "https://hooks.slack.com/actions/T0001/1/2?x=1",
		"path traversal":    "https://hooks.slack.com/actions/../x",
		"not a URL":         "%%%",
		"no path":           "https://hooks.slack.com",
		"another port":      "https://hooks.slack.com:8443/actions/T0001/1/2",
		"a host that nears": "https://hooks.slack.com.evil.example/actions/1",
	} {
		t.Run(name, func(t *testing.T) {
			x, s := open(t, testConfig()), &sinkRecorder{}
			serve(t, x, s, interactionPayload(buttonPayload(func(p map[string]any) { p["response_url"] = u })))
			if in := wantOne(t, s).Interaction; in.ResponseToken != "" || !in.ResponseExpiresAt.IsZero() {
				t.Fatalf("token %q expires %v: a URL outside the response origin is never kept", in.ResponseToken, in.ResponseExpiresAt)
			}
		})
	}
}

func TestSlashCommand(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	res := serve(t, x, s, formRequest(url.Values{
		"token": {"deprecated"}, "team_id": {testTeam}, "team_domain": {"acme"}, "channel_id": {testRoom}, "channel_name": {"ops"},
		"user_id": {testPerson}, "user_name": {"ann"}, "command": {"/deploy"}, "text": {"api to prod --force"},
		"api_app_id": {testApp}, "response_url": {hookURL}, "trigger_id": {"13345224609.738474920.8088930838d88f008e0"},
	}))
	if res.Code != http.StatusOK || res.Body != "" {
		t.Fatalf("answered %d %q, want 200 and an empty body: a body would be shown to the user", res.Code, res.Body)
	}
	e := wantOne(t, s)
	in := e.Interaction
	if in.Kind != model.InteractionCommand || in.Command != "deploy" || in.Arguments != "api to prod --force" || in.ID != "command:13345224609.738474920.8088930838d88f008e0" {
		t.Fatalf("interaction = %+v", in)
	}
	if in.Actor != x.userRef(testPerson) || len(e.References) != 1 || e.References[0] != x.roomRef(testRoom) || in.ResponseToken != hookTok || in.Message != nil {
		t.Errorf("actor %+v references %+v token %q message %v", in.Actor, e.References, in.ResponseToken, in.Message)
	}
	if err := in.Validate(); err != nil {
		t.Errorf("interaction invalid: %v", err)
	}
}

func TestViewSubmission(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	p := map[string]any{
		"type": "view_submission", "api_app_id": testApp, "team": map[string]any{"id": testTeam}, "user": map[string]any{"id": testPerson},
		"view": map[string]any{
			"id": "VNHU13V36", "type": "modal", "callback_id": "Feedback-Form", "private_metadata": "ticket-9",
			"state":         map[string]any{"values": map[string]any{"b": map[string]any{"a": map[string]any{"type": "plain_text_input", "value": "great"}}}},
			"response_urls": []any{map[string]any{"block_id": "b", "action_id": "c", "channel_id": testRoom, "response_url": hookURL}},
		},
	}
	res := serve(t, x, s, interactionPayload(p))
	if res.Code != http.StatusOK || res.Body != "" {
		t.Fatalf("answered %d %q, want 200 and an empty body: that closes the modal", res.Code, res.Body)
	}
	e := wantOne(t, s)
	in := e.Interaction
	if in.Kind != model.InteractionSubmission || in.Fields.Name != "slack.chat.view.feedback-form" || in.Fields.Version != 1 {
		t.Fatalf("interaction = %+v, want a submission under a schema named after the callback", in)
	}
	var data map[string]any
	if err := json.Unmarshal(in.Fields.Data, &data); err != nil || data["callback_id"] != "Feedback-Form" || data["view_id"] != "VNHU13V36" || data["private_metadata"] != "ticket-9" {
		t.Fatalf("fields data = %s, %v", in.Fields.Data, err)
	}
	if vals, _ := data["values"].(map[string]any); vals["b"] == nil {
		t.Errorf("values missing: %s", in.Fields.Data)
	}
	if !strings.HasPrefix(in.ID, "view:VNHU13V36:") {
		t.Errorf("ID = %q", in.ID)
	}
	if len(e.References) != 1 || e.References[0] != x.roomRef(testRoom) || in.ResponseToken != hookTok {
		t.Errorf("references %+v token %q, want the room and URL of the view's response_urls", e.References, in.ResponseToken)
	}
	if err := in.Validate(); err != nil {
		t.Errorf("interaction invalid: %v", err)
	}
}

func TestViewSubmissionWithoutAConversation(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	p := map[string]any{"type": "view_submission", "team": map[string]any{"id": testTeam}, "user": map[string]any{"id": testPerson},
		"view": map[string]any{"id": "V1", "type": "modal", "callback_id": "12345", "state": map[string]any{"values": map[string]any{}}}}
	serve(t, x, s, interactionPayload(p))
	e := wantOne(t, s)
	if len(e.References) != 0 || e.Interaction.ResponseToken != "" || e.Interaction.Fields.Name != "slack.chat.view.12345" {
		t.Fatalf("envelope = %+v: no room and no response URL to name", e)
	}
	if err := e.Interaction.Validate(); err != nil {
		t.Errorf("interaction invalid: %v", err)
	}
}

func TestCallbackIDsBecomeSchemaNames(t *testing.T) {
	for in, want := range map[string]string{
		"feedback":       "slack.chat.view.feedback",
		"Feedback Form!": "slack.chat.view.feedback_form",
		"v2":             "slack.chat.view.cb-v2",
		"":               "slack.chat.view",
		"é":              "slack.chat.view",
		"12345":          "slack.chat.view.12345",
	} {
		got := viewSchema(in)
		if got != want || model.ValidateExtensionName(got) != nil {
			t.Errorf("viewSchema(%q) = %q (valid: %v), want %q", in, got, model.ValidateExtensionName(got) == nil, want)
		}
	}
	if long := viewSchema(strings.Repeat("a", 500)); model.ValidateExtensionName(long) != nil {
		t.Errorf("a long callback gives an invalid name: %v", model.ValidateExtensionName(long))
	}
}

func TestInteractionsTheAdapterDoesNotConsumeAreAcknowledged(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	for name, p := range map[string]map[string]any{
		"view_closed":             {"type": "view_closed", "team": map[string]any{"id": testTeam}, "user": map[string]any{"id": testPerson}, "view": map[string]any{"id": "V1"}},
		"shortcut":                {"type": "shortcut", "team": map[string]any{"id": testTeam}, "user": map[string]any{"id": testPerson}, "callback_id": "x"},
		"message_action":          {"type": "message_action", "team": map[string]any{"id": testTeam}, "user": map[string]any{"id": testPerson}},
		"another team":            buttonPayload(func(p map[string]any) { p["team"] = map[string]any{"id": "T0OTHER"} }),
		"another app":             buttonPayload(func(p map[string]any) { p["api_app_id"] = "A0OTHER" }),
		"an action list is empty": buttonPayload(func(p map[string]any) { p["actions"] = []any{} }),
	} {
		t.Run(name, func(t *testing.T) {
			if res := serve(t, x, s, interactionPayload(p)); res.Code != http.StatusOK || res.Body != "" {
				t.Fatalf("answered %d %q, want 200, empty", res.Code, res.Body)
			}
			wantNone(t, s)
		})
	}
	other := formRequest(url.Values{"team_id": {"T0OTHER"}, "command": {"/x"}, "user_id": {testPerson}, "channel_id": {testRoom}, "trigger_id": {"1.2.3"}})
	if res := serve(t, x, s, other); res.Code != http.StatusOK {
		t.Fatalf("a command of another team answered %d", res.Code)
	}
	wantNone(t, s)
}

func TestMalformedInteractionsAre400(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	for name, r := range map[string]request{
		"no payload or command": formRequest(url.Values{"x": {"y"}}),
		"payload is not JSON":   formRequest(url.Values{"payload": {"{"}}),
		"not a form":            {contentType: "application/x-www-form-urlencoded", body: []byte("%zz"), at: testNow, secret: testSecret, method: http.MethodPost},
		"no user":               interactionPayload(buttonPayload(func(p map[string]any) { delete(p, "user") })),
		"user not an ID":        interactionPayload(buttonPayload(func(p map[string]any) { p["user"] = map[string]any{"id": "ann"} })),
		"action without ID":     interactionPayload(buttonPayload(func(p map[string]any) { p["actions"] = []any{map[string]any{"type": "button"}} })),
		"command without name":  formRequest(url.Values{"team_id": {testTeam}, "user_id": {testPerson}, "channel_id": {testRoom}, "trigger_id": {"1"}}),
		"command user missing":  formRequest(url.Values{"team_id": {testTeam}, "command": {"/x"}, "channel_id": {testRoom}, "trigger_id": {"1"}}),
		"submission no view":    interactionPayload(map[string]any{"type": "view_submission", "team": map[string]any{"id": testTeam}, "user": map[string]any{"id": testPerson}}),
	} {
		t.Run(name, func(t *testing.T) {
			if res := serve(t, x, s, r); res.Code != http.StatusBadRequest {
				t.Fatalf("answered %d %q, want 400", res.Code, res.Body)
			}
			wantNone(t, s)
		})
	}
}

func TestFormRequestsNeedASignature(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	r := interactionPayload(buttonPayload())
	r.secret = "other"
	if res := serve(t, x, s, r); res.Code != http.StatusUnauthorized {
		t.Fatalf("answered %d, want 401", res.Code)
	}
	r = interactionPayload(buttonPayload())
	r.at = testNow.Add(-time.Hour)
	if res := serve(t, x, s, r); res.Code != http.StatusUnauthorized {
		t.Fatalf("replayed interaction answered %d, want 401", res.Code)
	}
	wantNone(t, s)
}

func TestInteractionSinkFailures(t *testing.T) {
	x := open(t, testConfig())
	for name, tc := range map[string]struct {
		err  error
		code int
	}{
		"retryable": {model.NewError(model.Unavailable, "busy"), http.StatusServiceUnavailable},
		"refused":   {model.NewError(model.Validation, "selection needs a room"), http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			s := &sinkRecorder{fail: func(model.Ingress) error { return tc.err }}
			if res := serve(t, x, s, interactionPayload(buttonPayload())); res.Code != tc.code || res.Body != http.StatusText(tc.code)+"\n" {
				t.Fatalf("answered %d %q, want %d", res.Code, res.Body, tc.code)
			}
		})
	}
	// A duplicate is acknowledged again.
	s := &sinkRecorder{}
	serve(t, x, s, interactionPayload(buttonPayload()))
	if res := serve(t, x, s, interactionPayload(buttonPayload())); res.Code != http.StatusOK || res.Body != "" {
		t.Fatalf("redelivery answered %d %q", res.Code, res.Body)
	}
}
