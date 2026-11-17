package slack

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func (r *rig) send(t *testing.T, route model.ExternalRef, c model.Content, mod ...func(*adapter.SendRequest)) (adapter.Outcome, error) {
	t.Helper()
	req := adapter.SendRequest{Effect: r.effect(route), Content: c}
	for _, m := range mod {
		m(&req)
	}
	return r.x.Send(t.Context(), req)
}

func wantAccepted(t *testing.T, out adapter.Outcome, err error) {
	t.Helper()
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("outcome = %+v, %v; want provider_accepted", out, err)
	}
}

func wantRejected(t *testing.T, out adapter.Outcome, err error, code model.ErrorCode) {
	t.Helper()
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != code || out.Failure.Retry != model.NoRetry {
		t.Fatalf("outcome = %+v, %v; want a final rejection %s", out, err, code)
	}
}

func wantError(t *testing.T, out adapter.Outcome, err error, code model.ErrorCode) {
	t.Helper()
	_ = errorOf(t, out, err, code)
}

// errorOf is wantError returning the error.
func errorOf(t *testing.T, out adapter.Outcome, err error, code model.ErrorCode) *model.Error {
	t.Helper()
	me, ok := errors.AsType[*model.Error](err)
	if !ok || me.Code != code || out.State != "" {
		t.Fatalf("outcome = %+v, %v; want error %s", out, err, code)
	}
	return me
}

func TestSendPostsAMessageOnARoom(t *testing.T) {
	r := newRig(t)
	out, err := r.send(t, r.x.roomRef(testRoom), text("hello"))
	wantAccepted(t, out, err)
	calls := r.callsOf("chat.postMessage")
	if len(calls) != 1 || !calls[0].Applied {
		t.Fatalf("calls = %+v, want one applied chat.postMessage", calls)
	}
	a := calls[0].args(t)
	if a["channel"] != testRoom || a["text"] != "hello" || a["mrkdwn"] != false {
		t.Errorf("arguments = %+v, want the room, the text and mrkdwn off", a)
	}
	if _, has := a["thread_ts"]; has {
		t.Errorf("a reply to a room carries thread_ts %v", a["thread_ts"])
	}
	if calls[0].Auth != "Bearer "+testToken {
		t.Errorf("authorization %q, want the bot token as a bearer", calls[0].Auth)
	}
	want := model.ProviderMessageRef(testChannel, testRoom+":"+calls[0].TS)
	if len(out.References) != 1 || out.References[0] != want {
		t.Errorf("references = %+v, want %+v", out.References, want)
	}
	if got := r.f.messages("acct1", testRoom); len(got) != 1 || got[0].text != "hello" {
		t.Errorf("workspace holds %+v", got)
	}
}

// Text is literal: "&", "<" and ">" are Slack's control characters and go
// as entities (https://docs.slack.dev/messaging/formatting-message-text/).
func TestSendEscapesControlCharactersAndJoinsParts(t *testing.T) {
	r := newRig(t)
	c := model.Content{Parts: []model.Part{
		{Kind: model.PartText, Text: "a < b & c > d <@U0BOT>"},
		{Kind: model.PartText, Text: "second"},
	}}
	out, err := r.send(t, r.x.roomRef(testRoom), c)
	wantAccepted(t, out, err)
	if got := r.callsOf("chat.postMessage")[0].args(t)["text"]; got != "a &lt; b &amp; c &gt; d &lt;@U0BOT&gt;\n\nsecond" {
		t.Fatalf("text = %q", got)
	}
}

func TestSendCarriesTheAttemptAsMetadata(t *testing.T) {
	r := newRig(t)
	out, err := r.send(t, r.x.roomRef(testRoom), text("hi"))
	wantAccepted(t, out, err)
	md, _ := r.callsOf("chat.postMessage")[0].args(t)["metadata"].(map[string]any)
	payload, _ := md["event_payload"].(map[string]any)
	if md["event_type"] != MetadataEventType || payload["operation"] != string(testOp) || payload["attempt"] != string(testAttempt) {
		t.Fatalf("metadata = %+v, want %s naming the operation and attempt", md, MetadataEventType)
	}
}

func TestSendInAThread(t *testing.T) {
	parent := "1699999999.000100"
	for name, tc := range map[string]struct {
		route func(*Instance) model.ExternalRef
		mod   func(*rig) func(*adapter.SendRequest)
		want  string
	}{
		"a thread route": {func(x *Instance) model.ExternalRef { return x.threadRef(testRoom, parent) }, nil, parent},
		"a room, answering a top-level message": {
			func(x *Instance) model.ExternalRef { return x.roomRef(testRoom) },
			func(r *rig) func(*adapter.SendRequest) {
				return func(q *adapter.SendRequest) {
					ref := model.ProviderMessageRef(testChannel, testRoom+":"+parent)
					q.Target, q.Thread = &ref, &adapter.Thread{References: []model.ExternalRef{ref}}
				}
			}, parent},
		"a room, answering a reply: the reply's thread, never the reply": {
			func(x *Instance) model.ExternalRef { return x.roomRef(testRoom) },
			func(r *rig) func(*adapter.SendRequest) {
				return func(q *adapter.SendRequest) {
					ref := model.ProviderMessageRef(testChannel, testRoom+":1699999999.000500")
					q.Target = &ref
					q.Thread = &adapter.Thread{Metadata: []model.Extension{{Name: PayloadSchema, Version: 1, Data: []byte(`{"thread_ts":"` + parent + `"}`)}}}
				}
			}, parent},
		"a room, answering a message of another conversation": {
			func(x *Instance) model.ExternalRef { return x.roomRef(testRoom) },
			func(r *rig) func(*adapter.SendRequest) {
				return func(q *adapter.SendRequest) {
					ref := model.ProviderMessageRef(testChannel, "C0OTHER:"+parent)
					q.Target = &ref
				}
			}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.f.put("acct1", testRoom, testPerson, "parent", parent, "")
			var mod []func(*adapter.SendRequest)
			if tc.mod != nil {
				mod = append(mod, tc.mod(r))
			}
			out, err := r.send(t, tc.route(r.x), text("answer"), mod...)
			wantAccepted(t, out, err)
			got, _ := r.callsOf("chat.postMessage")[0].args(t)["thread_ts"].(string)
			if got != tc.want {
				t.Fatalf("thread_ts = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSendToAUserOpensTheDirectMessage(t *testing.T) {
	r := newRig(t)
	out, err := r.send(t, r.x.userRef(testPerson), text("psst"))
	wantAccepted(t, out, err)
	opens := r.callsOf("conversations.open")
	if len(opens) != 1 || opens[0].args(t)["users"] != testPerson {
		t.Fatalf("conversations.open calls = %+v, want one for the user", opens)
	}
	posts := r.callsOf("chat.postMessage")
	if len(posts) != 1 || posts[0].args(t)["channel"] != "D0DM1" {
		t.Fatalf("chat.postMessage calls = %+v, want one on the opened direct message D0DM1", posts)
	}
	if len(out.References) != 1 || out.References[0].ID != "D0DM1:"+posts[0].TS {
		t.Errorf("references = %+v, want the direct message's ts", out.References)
	}
	// A second send resumes the same conversation.
	wantA, wantB := r.send(t, r.x.userRef(testPerson), text("again"))
	wantAccepted(t, wantA, wantB)
	if got := r.callsOf("chat.postMessage")[1].args(t)["channel"]; got != "D0DM1" {
		t.Errorf("second send went to %v", got)
	}
}

func TestSendBlocksTravelWithTheTextAsFallback(t *testing.T) {
	r := newRig(t)
	c := model.Content{Parts: []model.Part{
		{Kind: model.PartText, Text: "Deploy?"},
		{Kind: model.PartStructured, Structured: &model.Extension{Name: BlocksSchema, Version: BlocksVersion, Data: []byte(
			`[{"type":"actions","elements":[{"type":"button","action_id":"approve","text":{"type":"plain_text","text":"Approve"},"value":"v1"}]}]`)}},
	}}
	out, err := r.send(t, r.x.roomRef(testRoom), c)
	wantAccepted(t, out, err)
	a := r.callsOf("chat.postMessage")[0].args(t)
	blocks, _ := a["blocks"].([]any)
	if len(blocks) != 1 || a["text"] != "Deploy?" {
		t.Fatalf("arguments = %+v, want the blocks and the text as their fallback", a)
	}
}

func TestSendRefusesWhatSlackCannotCarryBeforeAnyCall(t *testing.T) {
	r := newRig(t)
	long := strings.Repeat("x", MaxTextRunes+1)
	for name, tc := range map[string]struct {
		c   model.Content
		mod func(*adapter.SendRequest)
		err model.ErrorCode
	}{
		"attachment": {model.Content{Parts: []model.Part{{Kind: model.PartAttachment, Attachment: &model.AttachmentRef{}}}}, nil, model.Unsupported},
		"unknown structured part": {model.Content{Parts: []model.Part{
			{Kind: model.PartText, Text: "x"}, {Kind: model.PartStructured, Structured: &model.Extension{Name: "com.example.card.v", Version: 1, Data: []byte(`{}`)}}}}, nil, model.Unsupported},
		"two block lists": {model.Content{Parts: []model.Part{
			{Kind: model.PartStructured, Structured: &model.Extension{Name: BlocksSchema, Version: BlocksVersion, Data: []byte(`[{"type":"divider"}]`)}},
			{Kind: model.PartStructured, Structured: &model.Extension{Name: BlocksSchema, Version: BlocksVersion, Data: []byte(`[{"type":"divider"}]`)}}}}, nil, model.Unsupported},
		"blocks that are no list": {model.Content{Parts: []model.Part{
			{Kind: model.PartStructured, Structured: &model.Extension{Name: BlocksSchema, Version: BlocksVersion, Data: []byte(`{"type":"divider"}`)}}}}, nil, model.Validation},
		"empty":               {model.Content{}, nil, model.Validation},
		"too long":            {text(long), nil, model.Validation},
		"restricted audience": {text("x"), func(q *adapter.SendRequest) { q.Audience.Visibility = model.Restricted }, model.Unsupported},
		"internal audience":   {text("x"), func(q *adapter.SendRequest) { q.Audience.Visibility = model.Internal }, model.Unsupported},
	} {
		t.Run(name, func(t *testing.T) {
			var mod []func(*adapter.SendRequest)
			if tc.mod != nil {
				mod = append(mod, tc.mod)
			}
			out, err := r.send(t, r.x.roomRef(testRoom), tc.c, mod...)
			wantRejected(t, out, err, tc.err)
			if calls := r.f.calls("acct1"); len(calls) != 0 {
				t.Fatalf("calls = %+v, want none", calls)
			}
		})
	}
}

func TestSendRefusesARouteThatIsNotSlacks(t *testing.T) {
	r := newRig(t)
	other := testChannel
	other.ID = "00000ad0-0000-7000-8000-00000000000b"
	for name, route := range map[string]model.ExternalRef{
		"another channel":       {Channel: other, Namespace: Namespace, Kind: model.RefKindRoom, ID: testRoom},
		"another namespace":     {Channel: testChannel, Namespace: "github.issues", Kind: model.RefKindRoom, ID: testRoom},
		"a work item":           {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindWorkItem, ID: testRoom},
		"a room that is no ID":  {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindRoom, ID: "general"},
		"a thread without a ts": {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindThread, ID: "soon", ParentID: testRoom},
		"a user with a parent":  {Channel: testChannel, Namespace: UserNamespace, Kind: RefKindUser, ID: testPerson, ParentID: testRoom},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := r.send(t, route, text("x"))
			wantError(t, out, err, model.Validation)
			if calls := r.f.calls("acct1"); len(calls) != 0 {
				t.Fatalf("calls = %+v, want none", calls)
			}
		})
	}
}

func TestSendNeedsAHostAndACredential(t *testing.T) {
	r := newRig(t)
	if _, err := r.send(t, r.x.roomRef(testRoom), text("x")); err != nil {
		t.Fatal(err)
	}
	_ = r.x.Close(t.Context())
	out, err := r.send(t, r.x.roomRef(testRoom), text("x"))
	wantError(t, out, err, model.Unavailable)

	noHTTP := open(t, testConfig())
	out, err = noHTTP.Send(t.Context(), adapter.SendRequest{Effect: r.effect(noHTTP.roomRef(testRoom)), Content: text("x")})
	wantError(t, out, err, model.Unavailable)

	r2 := newRig(t)
	r2.creds.err = model.NewError(model.Unauthorized, "consent revoked")
	out, err = r2.send(t, r2.x.roomRef(testRoom), text("x"))
	wantError(t, out, err, model.Unauthorized)
	if calls := r2.f.calls("acct1"); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none without a credential", calls)
	}
}

func TestSendMapsSlacksAnswers(t *testing.T) {
	type check func(*testing.T, adapter.Outcome, error)
	rejects := func(code model.ErrorCode) check {
		return func(t *testing.T, o adapter.Outcome, err error) { wantRejected(t, o, err, code) }
	}
	errs := func(code model.ErrorCode) check {
		return func(t *testing.T, o adapter.Outcome, err error) { wantError(t, o, err, code) }
	}
	for name, tc := range map[string]struct {
		ans  []answer
		want check
	}{
		"channel not found":              {[]answer{failWith("channel_not_found")}, rejects(model.NotFound)},
		"thread not found":               {[]answer{failWith("thread_not_found")}, rejects(model.NotFound)},
		"not in channel":                 {[]answer{failWith("not_in_channel")}, rejects(model.Unauthorized)},
		"archived":                       {[]answer{failWith("is_archived")}, rejects(model.Unauthorized)},
		"missing scope":                  {[]answer{failWith("missing_scope")}, rejects(model.Unauthorized)},
		"no text":                        {[]answer{failWith("no_text")}, rejects(model.Validation)},
		"blocks refused":                 {[]answer{failWith("invalid_blocks")}, rejects(model.Validation)},
		"a code nobody listed":           {[]answer{failWith("some_new_code")}, rejects(model.Permanent)},
		"bad token, refused again fresh": {[]answer{failWith("invalid_auth"), failWith("invalid_auth")}, errs(model.Unauthorized)},
		"revoked token":                  {[]answer{failWith("token_revoked")}, errs(model.Unauthorized)},
		"internal error":                 {[]answer{failWith("internal_error")}, errs(model.OutcomeUnknown)},
		"unavailable":                    {[]answer{failWith("service_unavailable")}, errs(model.OutcomeUnknown)},
		"rate limited, body":             {[]answer{failWith("ratelimited")}, errs(model.RateLimited)},
		"server error":                   {[]answer{{status: 502}}, errs(model.OutcomeUnknown)},
		"answer lost":                    {[]answer{answerLost}, errs(model.OutcomeUnknown)},
		"request lost":                   {[]answer{requestLost}, errs(model.OutcomeUnknown)},
		"never reached":                  {[]answer{unreachable}, errs(model.Transient)},
		"unexpected status":              {[]answer{{status: 404}}, errs(model.Permanent)},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.f.respond("acct1", tc.ans...)
			out, err := r.send(t, r.x.roomRef(testRoom), text("x"))
			tc.want(t, out, err)
		})
	}
}

func TestSendRateLimitStatesItsWait(t *testing.T) {
	r := newRig(t)
	r.f.respond("acct1", rateLimitedFor(30*time.Second))
	out, err := r.send(t, r.x.roomRef(testRoom), text("x"))
	me := errorOf(t, out, err, model.RateLimited)
	if me.RetryAfter != 30*time.Second || me.Retry != model.RetrySameOperation {
		t.Fatalf("error = %+v, want a retry of the same operation after 30s", me)
	}
	r.f.respond("acct1", answer{status: 429})
	_, err = r.send(t, r.x.roomRef(testRoom), text("x"))
	if me, _ := errors.AsType[*model.Error](err); me == nil || me.RetryAfter != defaultRateWait {
		t.Fatalf("error = %v, want the default wait without Retry-After", err)
	}
}

// A credential Slack refuses as invalid is resolved again, fresh, once:
// nothing was applied, so the call is repeated.
func TestSendResolvesAFreshTokenOnce(t *testing.T) {
	r := newRig(t)
	r.creds.token = "stale"
	var fresh []bool
	r.x.creds = credentialFunc(func(ctx context.Context, q ports.CredentialRequest) (ports.Credential, error) {
		fresh = append(fresh, q.Fresh)
		if q.Fresh {
			r.creds.token = testToken
		}
		return r.creds.Resolve(ctx, q)
	})
	out, err := r.send(t, r.x.roomRef(testRoom), text("x"))
	wantAccepted(t, out, err)
	if len(fresh) != 2 || fresh[0] || !fresh[1] {
		t.Fatalf("credential requests fresh = %v, want a plain one and a fresh one", fresh)
	}
	if calls := r.callsOf("chat.postMessage"); len(calls) != 2 || calls[0].Applied || !calls[1].Applied {
		t.Fatalf("calls = %+v, want a refused then an applied call", calls)
	}
}

func TestSendAnswerWithoutATsIsOutcomeUnknown(t *testing.T) {
	r := newRig(t)
	// An answer {"ok": true} that names no message: the message exists but
	// the adapter cannot name it.
	r.x.http = rewriteClient{r.x.http, func(*http.Request, []byte) []byte { return []byte(`{"ok":true}`) }}
	out, err := r.send(t, r.x.roomRef(testRoom), text("x"))
	wantError(t, out, err, model.OutcomeUnknown)
}

func TestSendDropsMetadataSlackRefuses(t *testing.T) {
	// Slack documents that metadata schemas are registered in the app
	// manifest, and answers a refused one with an error: the send is
	// repeated without metadata, and later sends skip it.
	for _, code := range []string{"invalid_metadata_schema", "metadata_must_be_sent_from_app"} {
		t.Run(code, func(t *testing.T) {
			r := newRig(t)
			r.f.respond("acct1", answer{byMethod: map[string]string{"chat.postMessage": code}, errCode: code})
			out, err := r.send(t, r.x.roomRef(testRoom), text("x"))
			wantAccepted(t, out, err)
			posts := r.callsOf("chat.postMessage")
			if len(posts) != 2 || posts[0].args(t)["metadata"] == nil || posts[1].args(t)["metadata"] != nil {
				t.Fatalf("calls = %+v, want a send with metadata, refused, then one without", posts)
			}
			out, err = r.send(t, r.x.roomRef(testRoom), text("y"))
			wantAccepted(t, out, err)
			posts = r.callsOf("chat.postMessage")
			if len(posts) != 3 || posts[2].args(t)["metadata"] != nil {
				t.Fatalf("third call %+v: a refused schema is not tried again", posts[len(posts)-1])
			}
		})
	}
}
