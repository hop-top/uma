package teams

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"hop.top/uma/ports"
)

// fakeTeams is a stand-in for the Bot Framework services the adapter
// talks to, answering in the documented shapes. It is not a recording;
// docs/recording/teams.md is how the real exchanges get recorded.
//
//   - The OpenID metadata and key set of
//     https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication:
//     metadata naming jwks_uri, RSA keys with "endorsements" (the live set
//     endorses Teams keys for "msteams"), and tokens signed RS256 with
//     issuer https://api.botframework.com, the bot's App ID as audience
//     and the serviceurl claim.
//   - The Bot Connector REST API of
//     https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-api-reference:
//     POST /v3/conversations (ConversationResourceResponse), POST, PUT and
//     DELETE /v3/conversations/{id}/activities[/{activityId}]
//     (ResourceResponse {"id"}), errors as {"error":{"code","message"}}
//     with the Teams codes of
//     https://learn.microsoft.com/en-us/microsoftteams/platform/bots/build-conversational-capability
//     (403 ForbiddenOperationException, 404 ConversationNotFound and
//     ActivityNotFoundInConversation, 413 MessageSizeTooBig, 429 Throttled
//     with Retry-After).
//   - Reactions as the Teams SDK sends them: PUT and DELETE
//     /v3/conversations/{id}/activities/{activityId}/reactions/{type}
//     (https://learn.microsoft.com/en-us/microsoftteams/platform/teams-sdk/in-depth-guides/message-reactions;
//     the REST path is the SDK's, not on Learn).
//
// An account is a service URL path prefix
// ("https://smba.teams.test/<account>/"): its own bot, conversations,
// script and call record.
type fakeTeams struct {
	now func() time.Time

	mu       sync.Mutex
	accounts map[string]*fakeAccount
	// keys signs and publishes tokens: kid → key and its endorsements.
	keys        map[string]fakeKey
	metaFetches int
	keyFetches  int
	keysDown    bool
}

const (
	fakeLoginHost     = "login.botframework.test"
	fakeConnectorHost = "smba.teams.test"
	fakeMetadataURL   = "https://" + fakeLoginHost + "/v1/.well-known/openidconfiguration"
	fakeKeysURL       = "https://" + fakeLoginHost + "/v1/.well-known/keys"
	fakeKid           = "kid-teams-1"
)

type fakeAccount struct {
	prefix string
	token  string
	bot    string
	tenant string
	convs  map[string]*fakeConv
	// installed are the users the app is installed for: a personal chat can
	// be created with them only.
	installed map[string]bool
	// personal is the personal chat of each user, once created.
	personal map[string]string
	script   []scripted
	calls    []fakeCall
	seq      int64
	// lenient lets the bot change any message, as the adapter suite needs
	// to edit and delete messages of people; Teams refuses it.
	lenient bool
	// assume holds every conversation and message a call names to exist.
	assume bool
}

type fakeConv struct {
	id   string
	msgs []*fakeMsg
}

type fakeMsg struct {
	id        string
	root      string
	from      string
	text      string
	card      json.RawMessage
	replyTo   string
	reactions []string
}

// fakeCall is one Connector request the account received.
type fakeCall struct {
	Op      string // create, send, typing, update, delete, react, unreact
	Body    []byte
	Token   string
	Applied bool
	// Conversation and Activity name what the call created or touched;
	// Reaction is a reaction call's type.
	Conversation, Activity, Reaction string
}

func (c fakeCall) args() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(c.Body, &m)
	return m
}

// scripted is one scripted answer to an effect. The zero value accepts.
type scripted struct {
	// apply carries the effect out before dropping the connection (a lost
	// answer); drop alone loses the request; unreachable fails the dial.
	apply, drop, unreachable bool
	status                   int
	code                     string
	header                   map[string]string
	// byOp answers an operation with its own scripted answer instead.
	byOp map[string]scripted
}

var (
	accepted    = scripted{}
	answerLost  = scripted{apply: true, drop: true}
	requestLost = scripted{drop: true}
	unreachable = scripted{unreachable: true}
)

func failWith(status int, code string) scripted { return scripted{status: status, code: code} }

func rateLimitedFor(d time.Duration) scripted {
	return scripted{status: http.StatusTooManyRequests, code: "Throttled", header: map[string]string{"Retry-After": strconv.Itoa(int(d / time.Second))}}
}

func newFakeTeams(now func() time.Time) *fakeTeams {
	k, _ := signingKeys()
	return &fakeTeams{now: now, accounts: map[string]*fakeAccount{}, keys: map[string]fakeKey{
		fakeKid: {priv: k, endorsements: []string{"msteams", "skype"}, published: true},
	}}
}

// account returns the account under prefix, creating it.
func (f *fakeTeams) account(prefix, bot, tenant, token string) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[prefix]
	if !ok {
		a = &fakeAccount{prefix: prefix, token: token, bot: bot, tenant: tenant, convs: map[string]*fakeConv{}, installed: map[string]bool{}, personal: map[string]string{}}
		f.accounts[prefix] = a
	}
	return a
}

func (f *fakeTeams) serviceURL(prefix string) string {
	return "https://" + fakeConnectorHost + "/" + prefix + "/"
}

func (f *fakeTeams) respond(prefix string, rs ...scripted) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.script = append(a.script, rs...)
}

func (f *fakeTeams) calls(prefix string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.accounts[prefix]; a != nil {
		return slices.Clone(a.calls)
	}
	return nil
}

// conv returns the conversation, creating it.
func (a *fakeAccount) conv(id string) *fakeConv {
	base := baseConversation(id)
	c, ok := a.convs[base]
	if !ok {
		c = &fakeConv{id: base}
		a.convs[base] = c
	}
	return c
}

func (a *fakeAccount) nextID() string {
	a.seq++
	return strconv.FormatInt(1_700_000_000_000+a.seq, 10)
}

// put stores a message as if written earlier by from.
func (f *fakeTeams) put(prefix, conversation, from, text, id string) *fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	c := a.conv(conversation)
	if id == "" {
		id = a.nextID()
	}
	m := &fakeMsg{id: id, root: threadRoot(conversation), from: from, text: text}
	c.msgs = append(c.msgs, m)
	return m
}

func (f *fakeTeams) find(prefix, conversation, id string) *fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.accounts[prefix].convs[baseConversation(conversation)]; c != nil {
		for _, m := range c.msgs {
			if m.id == id {
				return m
			}
		}
	}
	return nil
}

func (f *fakeTeams) install(prefix, user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[prefix].installed[user] = true
}

// userOfPersonal is the user of a personal chat, "" for another
// conversation.
func (f *fakeTeams) userOfPersonal(prefix, conversation string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for u, c := range f.accounts[prefix].personal {
		if c == conversation {
			return u
		}
	}
	return ""
}

// client returns the HTTP client the adapter uses: it serves the fake in
// process and refuses any other host, so a test never reaches Microsoft.
func (f *fakeTeams) client() ports.HTTPClient { return fakeClient{f} }

type fakeClient struct{ f *fakeTeams }

func (c fakeClient) Do(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || (r.URL.Host != fakeConnectorHost && r.URL.Host != fakeLoginHost) {
		return nil, fmt.Errorf("fake Teams: refusing request to %s", r.URL.Host)
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	rec := httptest.NewRecorder()
	var err error
	if r.URL.Host == fakeLoginHost {
		c.f.serveLogin(rec, r)
	} else {
		err = c.f.serveConnector(rec, r, body)
	}
	if err != nil {
		return nil, err
	}
	res := rec.Result()
	res.Request = r
	return res, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errorBody(code string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": code}}
}

// pathSegments splits an escaped path into its unescaped segments.
func pathSegments(p string) []string {
	var out []string
	for _, s := range strings.Split(strings.Trim(p, "/"), "/") {
		u, err := url.PathUnescape(s)
		if err != nil {
			u = s
		}
		out = append(out, u)
	}
	return out
}

var effectOps = []string{"send", "update", "delete", "react", "unreact"}

// serveConnector answers one Connector request, or fails the connection
// with the returned error.
func (f *fakeTeams) serveConnector(w http.ResponseWriter, r *http.Request, body []byte) error {
	seg := pathSegments(r.URL.EscapedPath())
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(seg) < 3 || seg[1] != "v3" || seg[2] != "conversations" || f.accounts[seg[0]] == nil {
		writeJSON(w, http.StatusNotFound, errorBody("NotFound"))
		return nil
	}
	a := f.accounts[seg[0]]
	seg = seg[3:]
	call := fakeCall{Body: body, Token: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	switch {
	case len(seg) == 0 && r.Method == http.MethodPost:
		call.Op = "create"
	case len(seg) == 2 && seg[1] == "activities" && r.Method == http.MethodPost && in["type"] == "typing":
		call.Op, call.Conversation = "typing", seg[0]
	case len(seg) == 2 && seg[1] == "activities" && r.Method == http.MethodPost:
		call.Op, call.Conversation = "send", seg[0]
	case len(seg) == 3 && seg[1] == "activities" && r.Method == http.MethodPut:
		call.Op, call.Conversation, call.Activity = "update", seg[0], seg[2]
	case len(seg) == 3 && seg[1] == "activities" && r.Method == http.MethodDelete:
		call.Op, call.Conversation, call.Activity = "delete", seg[0], seg[2]
	case len(seg) == 5 && seg[1] == "activities" && seg[3] == "reactions" && (r.Method == http.MethodPut || r.Method == http.MethodDelete):
		call.Op, call.Conversation, call.Activity, call.Reaction = "react", seg[0], seg[2], seg[4]
		if r.Method == http.MethodDelete {
			call.Op = "unreact"
		}
	default:
		a.calls = append(a.calls, call)
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("MethodNotAllowed"))
		return nil
	}
	if call.Token != a.token {
		a.calls = append(a.calls, call)
		writeJSON(w, http.StatusUnauthorized, errorBody("Unauthorized"))
		return nil
	}
	rep := accepted
	if slices.Contains(effectOps, call.Op) && len(a.script) > 0 {
		rep, a.script = a.script[0], a.script[1:]
	}
	if m, ok := rep.byOp[call.Op]; ok {
		rep = m
	}
	if rep.unreachable {
		a.calls = append(a.calls, call)
		return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	if rep.status != 0 && !rep.apply {
		a.calls = append(a.calls, call)
		for k, v := range rep.header {
			w.Header().Set(k, v)
		}
		writeJSON(w, rep.status, errorBody(rep.code))
		return nil
	}
	status, res, applied := f.handle(a, &call, in, !rep.drop || rep.apply)
	call.Applied = applied
	a.calls = append(a.calls, call)
	if rep.drop {
		return &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}
	}
	if res == nil {
		w.WriteHeader(status)
		return nil
	}
	writeJSON(w, status, res)
	return nil
}

// handle runs one operation. apply is false for an effect whose request
// is lost before Teams acts on it.
func (f *fakeTeams) handle(a *fakeAccount, call *fakeCall, in map[string]any, apply bool) (int, any, bool) {
	fail := func(status int, code string) (int, any, bool) { return status, errorBody(code), false }
	switch call.Op {
	case "create":
		members, _ := in["members"].([]any)
		if len(members) != 1 {
			return fail(http.StatusBadRequest, "BadArgument")
		}
		m, _ := members[0].(map[string]any)
		user, _ := m["id"].(string)
		cd, _ := in["channelData"].(map[string]any)
		tenant, _ := cd["tenant"].(map[string]any)
		if tid, _ := tenant["id"].(string); tid != a.tenant {
			return fail(http.StatusBadRequest, "BadArgument")
		}
		if !a.installed[user] && !a.assume {
			return fail(http.StatusForbidden, "ForbiddenOperationException")
		}
		id, ok := a.personal[user]
		if !ok {
			id = "a:1personal-" + strings.TrimPrefix(user, "29:")
			a.personal[user] = id
			a.conv(id)
		}
		call.Conversation = id
		return http.StatusCreated, map[string]any{"id": id}, false
	case "typing":
		if a.convs[baseConversation(call.Conversation)] == nil && !a.assume {
			return fail(http.StatusNotFound, "ConversationNotFound")
		}
		return http.StatusOK, map[string]any{}, false
	case "send":
		c := a.convs[baseConversation(call.Conversation)]
		if c == nil && a.assume {
			c = a.conv(call.Conversation)
		}
		if c == nil {
			return fail(http.StatusNotFound, "ConversationNotFound")
		}
		if len(call.Body) > 100<<10 {
			return fail(http.StatusRequestEntityTooLarge, "MessageSizeTooBig")
		}
		m := &fakeMsg{id: a.nextID(), root: threadRoot(call.Conversation), from: a.bot, replyTo: str(in, "replyToId")}
		m.text = str(in, "text")
		if atts, ok := in["attachments"].([]any); ok && len(atts) > 0 {
			att, _ := atts[0].(map[string]any)
			m.card, _ = json.Marshal(att["content"])
		}
		if m.text == "" && m.card == nil {
			return fail(http.StatusBadRequest, "BadArgument")
		}
		call.Activity = m.id
		if apply {
			c.msgs = append(c.msgs, m)
		}
		return http.StatusCreated, map[string]any{"id": m.id}, apply
	case "update", "delete", "react", "unreact":
		c := a.convs[baseConversation(call.Conversation)]
		if c == nil && a.assume {
			c = a.conv(call.Conversation)
		}
		if c == nil {
			return fail(http.StatusNotFound, "ConversationNotFound")
		}
		i := slices.IndexFunc(c.msgs, func(m *fakeMsg) bool { return m.id == call.Activity })
		if i < 0 && a.assume {
			c.msgs = append(c.msgs, &fakeMsg{id: call.Activity, from: a.bot, text: "assumed"})
			i = len(c.msgs) - 1
		}
		if i < 0 {
			return fail(http.StatusNotFound, "ActivityNotFoundInConversation")
		}
		m := c.msgs[i]
		switch call.Op {
		case "update":
			if m.from != a.bot && !a.lenient {
				return fail(http.StatusForbidden, "ForbiddenOperationException")
			}
			if apply {
				m.text = str(in, "text")
			}
			return http.StatusOK, map[string]any{"id": m.id}, apply
		case "delete":
			if m.from != a.bot && !a.lenient {
				return fail(http.StatusForbidden, "ForbiddenOperationException")
			}
			if apply {
				c.msgs = slices.Delete(c.msgs, i, i+1)
			}
			return http.StatusOK, nil, apply
		case "react":
			if apply && !slices.Contains(m.reactions, call.Reaction) {
				m.reactions = append(m.reactions, call.Reaction)
			}
			return http.StatusOK, nil, apply
		default:
			if apply {
				m.reactions = slices.DeleteFunc(m.reactions, func(r string) bool { return r == call.Reaction })
			}
			return http.StatusOK, nil, apply
		}
	}
	return fail(http.StatusNotFound, "NotFound")
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}
