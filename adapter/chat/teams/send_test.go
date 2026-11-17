package teams

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func (r *rig) lastCall(t *testing.T) fakeCall {
	t.Helper()
	calls := r.f.calls(testPrefix)
	if len(calls) == 0 {
		t.Fatal("no Connector call")
	}
	return calls[len(calls)-1]
}

func (r *rig) seed(conv string) {
	r.f.mu.Lock()
	r.f.accounts[testPrefix].conv(conv)
	r.f.mu.Unlock()
}

func TestSendToConversationsThreadsAndUsers(t *testing.T) {
	r := newRig(t)
	r.seed(testGroupChat)
	r.seed(testTeamChannel)
	r.f.install(testPrefix, testUser)
	cases := []struct {
		name  string
		route model.ExternalRef
		conv  string
	}{
		{"group chat", r.x.roomRef(testGroupChat), testGroupChat},
		{"channel post", r.x.roomRef(testTeamChannel), testTeamChannel},
		{"channel thread", r.x.threadRef(testTeamChannel, "500"), testTeamChannel + ";messageid=500"},
		{"personal chat", r.x.userRef(testUser), "a:1personal-1user-alice"},
	}
	for _, c := range cases {
		out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(c.route), Content: text("hi <there> & *you*")})
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
			t.Fatalf("%s: %+v, %v", c.name, out, err)
		}
		call := r.lastCall(t)
		if call.Op != "send" || call.Conversation != c.conv || call.Token != testToken {
			t.Fatalf("%s: call %+v, want a send to %s", c.name, call, c.conv)
		}
		if a := call.args(); a["type"] != "message" || a["text"] != "hi <there> & *you*" || a["textFormat"] != "plain" {
			t.Fatalf("%s: activity %s, want the literal text as plain", c.name, call.Body)
		}
		if want := model.ProviderMessageRef(testChannel, baseConversation(c.conv)+"|"+call.Activity); out.References[0] != want {
			t.Fatalf("%s: reference %+v, want %+v", c.name, out.References[0], want)
		}
	}
	creates := 0
	for _, c := range r.f.calls(testPrefix) {
		if c.Op == "create" {
			creates++
			if a := c.args(); a["isGroup"] != false || !strings.Contains(string(c.Body), testTenantID) || !strings.Contains(string(c.Body), testBot) {
				t.Fatalf("create %s", c.Body)
			}
		}
	}
	if creates != 1 {
		t.Fatalf("creates = %d, want one for the user route", creates)
	}
}

func TestReplyCarriesReplyToIDInItsConversation(t *testing.T) {
	r := newRig(t)
	r.seed(testGroupChat)
	target := model.ProviderMessageRef(testChannel, testGroupChat+"|1001")
	eff := r.effect(r.x.roomRef(testGroupChat))
	eff.Target = &target
	if out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: eff, Content: text("re")}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("%+v %v", out, err)
	}
	if a := r.lastCall(t).args(); a["replyToId"] != "1001" {
		t.Fatalf("activity %v, want replyToId 1001", a)
	}
	other := model.ProviderMessageRef(testChannel, testTeamChannel+"|1001")
	eff.Target = &other
	_, _ = r.x.Send(t.Context(), adapter.SendRequest{Effect: eff, Content: text("re")})
	if a := r.lastCall(t).args(); a["replyToId"] != nil {
		t.Fatalf("activity %v: a message of another conversation is no reply target", a)
	}
}

func TestSendCard(t *testing.T) {
	r := newRig(t)
	r.seed(testGroupChat)
	card := `{"type":"AdaptiveCard","version":"1.4","body":[{"type":"TextBlock","text":"Approve?"}],"actions":[{"type":"Action.Execute","verb":"approve","title":"Approve"}]}`
	content := model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: CardSchema, Version: CardVersion, Data: json.RawMessage(card)}}}}
	out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testGroupChat)), Content: content})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("%+v %v", out, err)
	}
	var a struct {
		Text        string `json:"text"`
		Attachments []struct {
			ContentType string          `json:"contentType"`
			Content     json.RawMessage `json:"content"`
		} `json:"attachments"`
	}
	_ = json.Unmarshal(r.lastCall(t).Body, &a)
	if a.Text != "" || len(a.Attachments) != 1 || a.Attachments[0].ContentType != AdaptiveCardType || !strings.Contains(string(a.Attachments[0].Content), "Approve?") {
		t.Fatalf("activity %s", r.lastCall(t).Body)
	}
}

func TestSendRefusedBeforeAnyCall(t *testing.T) {
	r := newRig(t)
	r.seed(testGroupChat)
	route := r.x.roomRef(testGroupChat)
	card := &model.Extension{Name: CardSchema, Version: CardVersion, Data: json.RawMessage(`{"type":"AdaptiveCard"}`)}
	cases := []struct {
		name    string
		content model.Content
		vis     model.Visibility
		code    model.ErrorCode
	}{
		{"restricted", text("psst"), model.Restricted, model.Unsupported},
		{"text and card", model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "a"}, {Kind: model.PartStructured, Structured: card}}}, model.Public, model.Unsupported},
		{"two cards", model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: card}, {Kind: model.PartStructured, Structured: card}}}, model.Public, model.Unsupported},
		{"not a card", model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: CardSchema, Version: CardVersion, Data: json.RawMessage(`{"type":"Other"}`)}}}}, model.Public, model.Validation},
		{"other structured", model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: "com.example.x", Version: 1, Data: json.RawMessage(`{}`)}}}}, model.Public, model.Unsupported},
		{"empty", model.Content{}, model.Public, model.Validation},
		{"too large", text(strings.Repeat("x", MaxMessageBytes)), model.Public, model.Validation},
	}
	for _, c := range cases {
		eff := r.effect(route)
		eff.Audience.Visibility = c.vis
		out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: eff, Content: c.content})
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != c.code {
			t.Fatalf("%s: %+v %v, want rejected %s", c.name, out, err, c.code)
		}
	}
	if calls := r.f.calls(testPrefix); len(calls) != 0 {
		t.Fatalf("calls %+v, want none", calls)
	}
	bad := model.ExternalRef{Channel: testChannel, Namespace: Namespace, Kind: model.RefKindRoom, ID: "a b"}
	if _, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(bad), Content: text("x")}); model.CodeOf(err) != model.Validation {
		t.Fatalf("bad route: %v", err)
	}
}

func TestSendOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		answer scripted
		state  model.DeliveryState
		code   model.ErrorCode
	}{
		{"forbidden", failWith(http.StatusForbidden, "BotNotInConversationRoster"), model.Rejected, model.Unauthorized},
		{"not found", failWith(http.StatusNotFound, "ConversationNotFound"), model.Rejected, model.NotFound},
		{"too big", failWith(http.StatusRequestEntityTooLarge, "MessageSizeTooBig"), model.Rejected, model.Validation},
		{"bad request", failWith(http.StatusBadRequest, "BadArgument"), model.Rejected, model.Validation},
		{"precondition", failWith(http.StatusPreconditionFailed, "PreconditionFailed"), model.Rejected, model.Permanent},
		{"server error", failWith(http.StatusBadGateway, ""), "", model.OutcomeUnknown},
		{"answer lost", answerLost, "", model.OutcomeUnknown},
		{"request lost", requestLost, "", model.OutcomeUnknown},
		{"unreachable", unreachable, "", model.Transient},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.seed(testGroupChat)
			r.f.respond(testPrefix, c.answer)
			out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testGroupChat)), Content: text("hi")})
			if c.state != "" {
				if err != nil || out.State != c.state || out.Failure.Code != c.code {
					t.Fatalf("%+v %v, want %s %s", out, err, c.state, c.code)
				}
				return
			}
			if model.CodeOf(err) != c.code {
				t.Fatalf("%+v %v, want error %s", out, err, c.code)
			}
		})
	}
}

func TestSendRateLimitedRetriesAfter(t *testing.T) {
	r := newRig(t)
	r.seed(testGroupChat)
	r.f.respond(testPrefix, rateLimitedFor(7*time.Second))
	_, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testGroupChat)), Content: text("hi")})
	me, ok := err.(*model.Error)
	if !ok || me.Code != model.RateLimited || me.RetryAfter != 7*time.Second {
		t.Fatalf("%v, want rate_limited after 7s", err)
	}
}

func TestPersonalChatRefusedWhenAppNotInstalled(t *testing.T) {
	r := newRig(t)
	out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.userRef(testOther)), Content: text("hi")})
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.Unauthorized {
		t.Fatalf("%+v %v, want rejected unauthorized", out, err)
	}
	for _, c := range r.f.calls(testPrefix) {
		if c.Op == "send" {
			t.Fatal("a message was sent without a conversation")
		}
	}
}

func TestTokenResolvedPerCallAndRefreshedOnce(t *testing.T) {
	r := newRig(t)
	r.seed(testGroupChat)
	r.creds.expired = true
	if out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testGroupChat)), Content: text("hi")}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("%+v %v", out, err)
	}
	asked := r.creds.asked()
	if len(asked) != 2 || asked[0].Fresh || !asked[1].Fresh || asked[1].Purpose != CredentialPurposeConnector {
		t.Fatalf("requests %+v, want a cached token refused as expired and one fresh", asked)
	}
	// A token the Connector refuses is resolved fresh once, then refused.
	r.creds.expired = false
	r.creds.token = "stale"
	_, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testGroupChat)), Content: text("hi")})
	if model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("%v, want unauthorized", err)
	}
	if n := len(r.creds.asked()) - len(asked); n != 2 {
		t.Fatalf("resolutions = %d, want the token and one fresh one", n)
	}
	for _, c := range r.f.calls(testPrefix) {
		if c.Token != testToken && c.Token != "stale" {
			t.Fatalf("call carried %q", c.Token)
		}
	}
}

func TestTokenNeverLeavesTheServiceURL(t *testing.T) {
	r := newRig(t)
	r.seed(testGroupChat)
	_, _ = r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testGroupChat)), Content: text("hi")})
	for _, c := range r.f.calls(testPrefix) {
		if c.Token != testToken {
			t.Fatalf("token %q", c.Token)
		}
	}
	// The fake refuses every host but its own; a service URL elsewhere is
	// configuration, never a payload.
	if issues := (&Factory{}).Validate(Config{AppID: testAppID, TenantID: testTenantID, ServiceURL: "http://smba.teams.test/"}.Extension()); len(issues) != 1 {
		t.Fatalf("issues %+v, want service_url refused", issues)
	}
}

func TestSignalTyping(t *testing.T) {
	r := newRig(t)
	r.seed(testGroupChat)
	r.f.install(testPrefix, testUser)
	for _, route := range []model.ExternalRef{r.x.roomRef(testGroupChat), r.x.userRef(testUser)} {
		if err := r.x.Signal(t.Context(), adapter.SignalRequest{Route: route, Kind: "typing", Audience: model.Audience{Visibility: model.Public}}); err != nil {
			t.Fatal(err)
		}
		if c := r.lastCall(t); c.Op != "typing" {
			t.Fatalf("call %+v", c)
		}
	}
	n := len(r.f.calls(testPrefix))
	if err := r.x.Signal(t.Context(), adapter.SignalRequest{Route: r.x.roomRef(testGroupChat), Kind: "recording"}); model.CodeOf(err) != model.Unsupported {
		t.Fatalf("%v, want unsupported", err)
	}
	if len(r.f.calls(testPrefix)) != n {
		t.Fatal("an unsupported signal reached Teams")
	}
	if err := r.x.Signal(t.Context(), adapter.SignalRequest{Route: r.x.userRef(testOther), Kind: "typing"}); model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("%v, want unauthorized without the app installed", err)
	}
}

func TestClosedOrUnwiredInstanceMakesNoCall(t *testing.T) {
	r := newRig(t)
	_ = r.x.Close(t.Context())
	if _, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testGroupChat)), Content: text("hi")}); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("%v", err)
	}
	nohttp := newRig(t, func(d *adapter.Dependencies) { d.HTTP = nil })
	if _, err := nohttp.x.Send(t.Context(), adapter.SendRequest{Effect: nohttp.effect(nohttp.x.roomRef(testGroupChat)), Content: text("hi")}); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("%v", err)
	}
}
