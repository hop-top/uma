package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"hop.top/uma/ports"
)

// fakeSlack is a Slack Web API and response_url server for the methods the
// adapter calls, answering in the documented shapes:
//
//   - chat.postMessage, chat.update, chat.delete:
//     https://docs.slack.dev/reference/methods/chat.postMessage/ and siblings
//   - reactions.add, reactions.remove, reactions.get:
//     https://docs.slack.dev/reference/methods/reactions.add/ and siblings
//   - conversations.open, conversations.history, conversations.replies:
//     https://docs.slack.dev/reference/methods/conversations.history/ and siblings
//   - views.open, views.push, views.update:
//     https://docs.slack.dev/reference/methods/views.open/ and siblings
//   - response_url: https://docs.slack.dev/interactivity/handling-user-interaction/
//
// Every method answers {"ok": true, ...} or {"ok": false, "error": code}:
// "Callers should always check the value of the ok parameter in the
// response"; the fake answers both with HTTP 200, which the references do
// not state. A rate limit is HTTP 429 with Retry-After
// (https://docs.slack.dev/apis/web-api/rate-limits/). Each account is a
// path prefix ("https://slack.test/<account>/"), its own workspace with its
// own conversations, script and call record.
//
// Slack documents no error body for a response_url; the fake answers what
// the incoming webhooks page lists: 200 "ok", else a status and a code such
// as "no_service".
type fakeSlack struct {
	now func() time.Time

	mu       sync.Mutex
	accounts map[string]*fakeAccount
	order    []string
	hooks    map[string]*fakeHook
	// sockSrv is the Socket Mode server (fakesocket_test.go), started on
	// first use.
	sockSrv *httptest.Server
}

const (
	fakeAPIHost  = "slack.test"
	fakeHookHost = "hooks.slack.test"
)

type fakeAccount struct {
	prefix  string
	token   string
	botUser string
	team    string
	convs   map[string]*fakeConv
	dms     map[string]string // user -> direct message conversation
	seq     int64
	dmSeq   int
	script  []answer
	calls   []fakeCall
	// rateLimitedHistory makes history reads answer 429, as the reduced
	// limit of non-Marketplace apps does.
	rateLimitedHistory bool
	// lenient lets the bot change any message, as the adapter suite needs to
	// edit and delete messages of people. Slack answers cant_update_message
	// and cant_delete_message for those; the suite scripts that refusal.
	lenient bool
	// ignoreBounds makes reads ignore oldest and latest: an answer a client
	// must not trust to have honoured them.
	ignoreBounds bool
	// views holds the modals and the trigger_ids (fakeviews_test.go).
	views fakeViews

	// Socket Mode (fakesocket_test.go): the app-level token, a script of
	// apps.connections.open answers, the URL it answers instead of a fresh
	// ticket, whether connections skip their hello, what runs on each new
	// connection, and the connections opened.
	appToken  string
	opens     []answer
	tickets   int
	socketURL string
	noHello   bool
	// deaf connections never read, so they answer no ping, until
	// deafUntil is closed.
	deafUntil chan struct{}
	onConnect func(*fakeConn)
	conns     []*fakeConn
	connected chan *fakeConn
}

type fakeConv struct {
	id   string
	msgs []*fakeMsg // in creation order
	// archived channels take no new message: Slack answers is_archived.
	archived bool
}

type fakeMsg struct {
	ts       string
	threadTS string
	user     string
	botID    string
	text     string
	blocks   json.RawMessage
	meta     *fakeMeta
	subtype  string
	// reactions by name, the users who reacted in order.
	reactions map[string][]string
	edited    string
}

type fakeMeta struct {
	EventType    string         `json:"event_type"`
	EventPayload map[string]any `json:"event_payload"`
}

// fakeCall is one request the account received.
type fakeCall struct {
	Method  string // the Web API method, or "response_url"
	Body    []byte
	Query   string
	Auth    string
	Applied bool
	// Channel and TS name what the call created or touched.
	Channel, TS string
	// Token is the response URL path of a response_url call.
	Token string
}

type fakeHook struct {
	uses    int
	expires time.Time
}

// answer is one scripted answer to an effect: a send, an edit, a deletion,
// a reaction or a response_url post. The zero value accepts.
type answer struct {
	// drop closes the connection instead of answering; unreachable fails
	// the dial: the request never reaches Slack. apply carries the effect
	// out before either.
	apply, drop, unreachable bool
	status                   int
	header                   map[string]string
	// errCode answers {"ok": false, "error": errCode}; byMethod overrides
	// it per method.
	errCode  string
	byMethod map[string]string
}

var (
	accepted    = answer{apply: true}
	answerLost  = answer{apply: true, drop: true}
	requestLost = answer{drop: true}
	unreachable = answer{unreachable: true}
)

func failWith(code string) answer { return answer{errCode: code} }

func rateLimitedFor(d time.Duration) answer {
	return answer{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": strconv.Itoa(int(d / time.Second))}}
}

func newFakeSlack(now func() time.Time) *fakeSlack {
	return &fakeSlack{now: now, accounts: map[string]*fakeAccount{}, hooks: map[string]*fakeHook{}}
}

// account returns the account under prefix, creating it as a workspace
// whose bot is botUser and whose token is token.
func (f *fakeSlack) account(prefix, team, botUser, token string) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[prefix]
	if !ok {
		a = &fakeAccount{prefix: prefix, token: token, botUser: botUser, team: team, convs: map[string]*fakeConv{}, dms: map[string]string{}, connected: make(chan *fakeConn, 64)}
		f.accounts[prefix] = a
		f.order = append(f.order, prefix)
	}
	return a
}

func (f *fakeSlack) apiURL(prefix string) string {
	return "https://" + fakeAPIHost + "/" + prefix + "/"
}

func (f *fakeSlack) responseBase() string { return "https://" + fakeHookHost }

func (f *fakeSlack) respond(prefix string, as ...answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.script = append(a.script, as...)
}

func (f *fakeSlack) calls(prefix string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.accounts[prefix]; a != nil {
		return slices.Clone(a.calls)
	}
	return nil
}

// conversation returns the conversation id of the account, creating it
// with its bot as a member.
func (a *fakeAccount) conversation(id string) *fakeConv {
	c, ok := a.convs[id]
	if !ok {
		c = &fakeConv{id: id}
		a.convs[id] = c
	}
	return c
}

// nextTS returns a ts after every earlier one of the account.
func (f *fakeSlack) nextTS(a *fakeAccount) string {
	a.seq++
	return formatTS(f.now().Add(time.Duration(a.seq) * time.Microsecond))
}

// put stores a message as if written earlier: by user, in conversation, at
// ts (a fresh one when empty), in thread when set. It returns the message.
func (f *fakeSlack) put(prefix, conversation, user, text, ts, thread string) *fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	if ts == "" {
		ts = f.nextTS(a)
	}
	m := &fakeMsg{ts: ts, threadTS: thread, user: user, text: text}
	c := a.conversation(conversation)
	c.msgs = append(c.msgs, m)
	return m
}

func (f *fakeSlack) find(prefix, conversation, ts string) *fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.accounts[prefix].convs[conversation]; c != nil {
		for _, m := range c.msgs {
			if m.ts == ts {
				return m
			}
		}
	}
	return nil
}

func (f *fakeSlack) messages(prefix, conversation string) []fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeMsg
	if c := f.accounts[prefix].convs[conversation]; c != nil {
		for _, m := range c.msgs {
			out = append(out, *m)
		}
	}
	return out
}

// client returns the HTTP client the adapter uses: it serves the fake in
// process and refuses any other host, so a test never reaches Slack.
func (f *fakeSlack) client() ports.HTTPClient { return fakeClient{f} }

type fakeClient struct{ f *fakeSlack }

func (c fakeClient) Do(r *http.Request) (*http.Response, error) {
	f := c.f
	if r.URL.Host == fakeSocketHost {
		return f.socketRoundTrip(r)
	}
	if r.URL.Scheme != "https" || (r.URL.Host != fakeAPIHost && r.URL.Host != fakeHookHost) {
		return nil, fmt.Errorf("fake Slack: refusing request to %s", r.URL)
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	rec := httptest.NewRecorder()
	if err := f.serve(rec, r, body); err != nil {
		return nil, err
	}
	res := rec.Result()
	res.Request = r
	return res, nil
}

// serve answers one request, or fails the connection with the returned
// error.
func (f *fakeSlack) serve(w http.ResponseWriter, r *http.Request, body []byte) error {
	if r.URL.Host == fakeHookHost {
		return f.serveHook(w, r, body)
	}
	prefix, method, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	f.mu.Lock()
	a := f.accounts[prefix]
	f.mu.Unlock()
	if a == nil {
		writeAPI(w, map[string]any{"ok": false, "error": "invalid_auth"})
		return nil
	}
	call := fakeCall{Method: method, Body: body, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")}
	f.mu.Lock()
	defer f.mu.Unlock()
	if method == "apps.connections.open" {
		return f.connectionsOpen(a, w, call.Auth)
	}
	effect := slices.Contains([]string{"chat.postMessage", "chat.update", "chat.delete", "reactions.add", "reactions.remove", "views.open", "views.push", "views.update", "conversations.archive", "conversations.unarchive"}, method)
	if r.Method != http.MethodPost || call.Auth != "Bearer "+a.token {
		a.calls = append(a.calls, call)
		writeAPI(w, map[string]any{"ok": false, "error": "invalid_auth"})
		return nil
	}
	if r.Header.Get("Content-Type") != "application/json; charset=utf-8" && r.Header.Get("Content-Type") != "application/json" {
		a.calls = append(a.calls, call)
		writeAPI(w, map[string]any{"ok": false, "error": "invalid_post_type"})
		return nil
	}
	ans := accepted
	if effect && len(a.script) > 0 {
		ans, a.script = a.script[0], a.script[1:]
	}
	if ans.unreachable {
		a.calls = append(a.calls, call)
		return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	if code := cmp(ans.byMethod[method], ans.errCode); code != "" && !ans.apply {
		a.calls = append(a.calls, call)
		writeAPI(w, map[string]any{"ok": false, "error": code})
		return nil
	}
	if ans.status != 0 && !ans.apply {
		a.calls = append(a.calls, call)
		for k, v := range ans.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(ans.status)
		return nil
	}
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	res, applied := f.handle(a, method, in, &call, ans.apply || !effect)
	call.Applied = applied
	a.calls = append(a.calls, call)
	if ans.drop {
		return &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}
	}
	writeAPI(w, res)
	return nil
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func writeAPI(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func str(in map[string]any, k string) string {
	s, _ := in[k].(string)
	return s
}

func apiFail(code string) map[string]any { return map[string]any{"ok": false, "error": code} }

// handle runs one Web API method on account a (locked) and returns the
// answer and whether it changed the workspace. apply is false for a
// scripted request that was lost before Slack applied it.
func (f *fakeSlack) handle(a *fakeAccount, method string, in map[string]any, call *fakeCall, apply bool) (map[string]any, bool) {
	switch method {
	case "chat.postMessage":
		return f.postMessage(a, in, call, apply)
	case "chat.update":
		return f.update(a, in, call, apply)
	case "chat.delete":
		return f.delete(a, in, call, apply)
	case "reactions.add", "reactions.remove":
		return f.react(a, method == "reactions.add", in, call, apply)
	case "reactions.get":
		return f.reactionsGet(a, in)
	case "views.open", "views.push", "views.update":
		return f.viewMethod(a, method, in, call, apply)
	case "conversations.archive", "conversations.unarchive":
		return f.archive(a, method == "conversations.archive", in, call, apply)
	case "conversations.info":
		return f.info(a, in)
	case "conversations.open":
		return f.open(a, in)
	case "conversations.history":
		return f.history(a, in)
	case "conversations.replies":
		return f.replies(a, in)
	}
	return apiFail("unknown_method"), false
}

func (f *fakeSlack) postMessage(a *fakeAccount, in map[string]any, call *fakeCall, apply bool) (map[string]any, bool) {
	channel := str(in, "channel")
	if !slackID.MatchString(channel) {
		return apiFail("channel_not_found"), false
	}
	text, hasBlocks := str(in, "text"), in["blocks"] != nil
	if text == "" && !hasBlocks {
		return apiFail("no_text"), false
	}
	c := a.conversation(channel)
	if c.archived {
		return apiFail("is_archived"), false
	}
	thread := str(in, "thread_ts")
	if thread != "" {
		parent := c.byTS(thread)
		if parent == nil {
			return apiFail("thread_not_found"), false
		}
		if parent.threadTS != "" && parent.threadTS != parent.ts {
			return apiFail("cannot_reply_to_message"), false
		}
	}
	if !apply {
		return map[string]any{"ok": true}, false
	}
	m := &fakeMsg{ts: f.nextTS(a), threadTS: thread, user: a.botUser, botID: "B0BOT", text: text}
	if hasBlocks {
		m.blocks, _ = json.Marshal(in["blocks"])
	}
	if md, ok := in["metadata"].(map[string]any); ok {
		m.meta = &fakeMeta{EventType: str(md, "event_type")}
		m.meta.EventPayload, _ = md["event_payload"].(map[string]any)
	}
	c.msgs = append(c.msgs, m)
	call.Channel, call.TS = channel, m.ts
	return map[string]any{"ok": true, "channel": channel, "ts": m.ts, "message": m.json()}, true
}

func (c *fakeConv) byTS(ts string) *fakeMsg {
	for _, m := range c.msgs {
		if m.ts == ts {
			return m
		}
	}
	return nil
}

func (f *fakeSlack) update(a *fakeAccount, in map[string]any, call *fakeCall, apply bool) (map[string]any, bool) {
	c := a.convs[str(in, "channel")]
	if c == nil {
		return apiFail("channel_not_found"), false
	}
	m := c.byTS(str(in, "ts"))
	if m == nil {
		return apiFail("message_not_found"), false
	}
	if m.user != a.botUser && !a.lenient {
		return apiFail("cant_update_message"), false
	}
	text, hasBlocks := str(in, "text"), in["blocks"] != nil
	if text == "" && !hasBlocks {
		return apiFail("no_text"), false
	}
	if !apply {
		return map[string]any{"ok": true}, false
	}
	m.text = text
	if hasBlocks {
		m.blocks, _ = json.Marshal(in["blocks"])
	} else if text != "" {
		// "the message's previous blocks will only be retained if the
		// text argument is not provided"
		m.blocks = nil
	}
	if md, ok := in["metadata"].(map[string]any); ok {
		m.meta = &fakeMeta{EventType: str(md, "event_type")}
		m.meta.EventPayload, _ = md["event_payload"].(map[string]any)
	}
	call.Channel, call.TS = c.id, m.ts
	return map[string]any{"ok": true, "channel": c.id, "ts": m.ts, "text": m.text, "message": m.json()}, true
}

func (f *fakeSlack) delete(a *fakeAccount, in map[string]any, call *fakeCall, apply bool) (map[string]any, bool) {
	c := a.convs[str(in, "channel")]
	if c == nil {
		return apiFail("channel_not_found"), false
	}
	m := c.byTS(str(in, "ts"))
	if m == nil {
		return apiFail("message_not_found"), false
	}
	if m.user != a.botUser && !a.lenient {
		return apiFail("cant_delete_message"), false
	}
	if !apply {
		return map[string]any{"ok": true}, false
	}
	c.msgs = slices.DeleteFunc(c.msgs, func(x *fakeMsg) bool { return x == m })
	call.Channel, call.TS = c.id, m.ts
	return map[string]any{"ok": true, "channel": c.id, "ts": m.ts}, true
}

func (f *fakeSlack) react(a *fakeAccount, add bool, in map[string]any, call *fakeCall, apply bool) (map[string]any, bool) {
	c := a.convs[str(in, "channel")]
	name := str(in, "name")
	if str(in, "channel") == "" || str(in, "timestamp") == "" {
		return apiFail("no_item_specified"), false
	}
	if name == "" {
		return apiFail("invalid_name"), false
	}
	if c == nil {
		return apiFail("channel_not_found"), false
	}
	m := c.byTS(str(in, "timestamp"))
	if m == nil {
		return apiFail("message_not_found"), false
	}
	has := slices.Contains(m.reactions[name], a.botUser)
	switch {
	case add && has:
		return apiFail("already_reacted"), false
	case !add && !has:
		return apiFail("no_reaction"), false
	case !apply:
		return map[string]any{"ok": true}, false
	}
	if m.reactions == nil {
		m.reactions = map[string][]string{}
	}
	if add {
		m.reactions[name] = append(m.reactions[name], a.botUser)
	} else {
		m.reactions[name] = slices.DeleteFunc(m.reactions[name], func(u string) bool { return u == a.botUser })
		if len(m.reactions[name]) == 0 {
			delete(m.reactions, name)
		}
	}
	call.Channel, call.TS = c.id, m.ts
	return map[string]any{"ok": true}, true
}

func (f *fakeSlack) reactionsGet(a *fakeAccount, in map[string]any) (map[string]any, bool) {
	c := a.convs[str(in, "channel")]
	if c == nil {
		return apiFail("channel_not_found"), false
	}
	m := c.byTS(str(in, "timestamp"))
	if m == nil {
		return apiFail("message_not_found"), false
	}
	return map[string]any{"ok": true, "type": "message", "channel": c.id, "message": m.json()}, false
}

func (f *fakeSlack) open(a *fakeAccount, in map[string]any) (map[string]any, bool) {
	users := str(in, "users")
	if users == "" || strings.Contains(users, ",") || !slackID.MatchString(users) {
		return apiFail("user_not_found"), false
	}
	preventCreation, _ := in["prevent_creation"].(bool)
	id, ok := a.dms[users]
	if !ok && preventCreation {
		// Slack documents prevent_creation but not the answer for a user with
		// no direct message yet; the fake answers channel_not_found.
		return apiFail("channel_not_found"), false
	}
	if !ok {
		a.dmSeq++
		id = "D0DM" + strconv.Itoa(a.dmSeq)
		a.dms[users] = id
		a.conversation(id)
	}
	return map[string]any{"ok": true, "channel": map[string]any{"id": id}}, false
}

// json is the message object of chat.postMessage, conversations.history
// and reactions.get answers.
func (m *fakeMsg) json() map[string]any {
	o := map[string]any{"type": "message", "text": m.text, "ts": m.ts}
	if m.user != "" {
		o["user"] = m.user
	}
	if m.botID != "" {
		o["bot_id"] = m.botID
	}
	if m.threadTS != "" {
		o["thread_ts"] = m.threadTS
	}
	if m.blocks != nil {
		o["blocks"] = json.RawMessage(m.blocks)
	}
	if m.subtype != "" {
		o["subtype"] = m.subtype
	}
	if m.edited != "" {
		o["edited"] = map[string]any{"user": m.user, "ts": m.edited}
	}
	if len(m.reactions) > 0 {
		var names []string
		for n := range m.reactions {
			names = append(names, n)
		}
		slices.Sort(names)
		rs := []any{}
		for _, n := range names {
			rs = append(rs, map[string]any{"name": n, "users": m.reactions[n], "count": len(m.reactions[n])})
		}
		o["reactions"] = rs
	}
	return o
}

// history answers conversations.history: the top-level messages (and
// broadcasts) of the conversation in its time range, newest first, a page
// at a time; with include_all_metadata each message carries its metadata.
func (f *fakeSlack) history(a *fakeAccount, in map[string]any) (map[string]any, bool) {
	if a.rateLimitedHistory {
		return apiFail("ratelimited"), false
	}
	c := a.convs[str(in, "channel")]
	if c == nil {
		return apiFail("channel_not_found"), false
	}
	var msgs []*fakeMsg
	for _, m := range c.msgs {
		if m.threadTS == "" || m.threadTS == m.ts || m.subtype == "thread_broadcast" {
			msgs = append(msgs, m)
		}
	}
	replies := map[string]int{}
	for _, m := range c.msgs {
		if m.threadTS != "" && m.threadTS != m.ts {
			replies[m.threadTS]++
		}
	}
	res, applied := f.page(a, msgs, in, true)
	if ms, ok := res["messages"].([]any); ok {
		for _, m := range ms {
			if o := m.(map[string]any); replies[o["ts"].(string)] > 0 {
				o["reply_count"] = replies[o["ts"].(string)]
				o["thread_ts"] = o["ts"]
			}
		}
	}
	return res, applied
}

// replies answers conversations.replies: the thread's parent and its
// replies, oldest first.
func (f *fakeSlack) replies(a *fakeAccount, in map[string]any) (map[string]any, bool) {
	c := a.convs[str(in, "channel")]
	if c == nil {
		return apiFail("channel_not_found"), false
	}
	// ts is "either a thread's parent message or a message in the thread", and the
	// answer "first contains the parent message object", whatever the time
	// range says of it.
	parent := str(in, "ts")
	if m := c.byTS(parent); m != nil && m.threadTS != "" && m.threadTS != m.ts {
		parent = m.threadTS
	}
	var msgs []*fakeMsg
	for _, m := range c.msgs {
		if m.ts == parent || m.threadTS == parent {
			msgs = append(msgs, m)
		}
	}
	if len(msgs) == 0 {
		return apiFail("thread_not_found"), false
	}
	in = maps.Clone(in)
	in["always"] = parent
	return f.page(a, msgs, in, false)
}

func (f *fakeSlack) page(a *fakeAccount, msgs []*fakeMsg, in map[string]any, newestFirst bool) (map[string]any, bool) {
	oldest, latest := str(in, "oldest"), str(in, "latest")
	inclusive, _ := in["inclusive"].(bool)
	var inRange []*fakeMsg
	always := str(in, "always")
	for _, m := range msgs {
		if a.ignoreBounds || (always != "" && m.ts == always) {
			inRange = append(inRange, m)
			continue
		}
		if oldest != "" && oldest != "0" && (m.ts < oldest || (m.ts == oldest && !inclusive)) {
			continue
		}
		if latest != "" && (m.ts > latest || (m.ts == latest && !inclusive)) {
			continue
		}
		inRange = append(inRange, m)
	}
	slices.SortFunc(inRange, func(x, y *fakeMsg) int { return strings.Compare(x.ts, y.ts) })
	if newestFirst {
		slices.Reverse(inRange)
	}
	limit := 100
	if l, ok := in["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	start := 0
	if cur := str(in, "cursor"); cur != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(cur, "cursor-"))
		if err != nil || !strings.HasPrefix(cur, "cursor-") || n < 0 || n > len(inRange) {
			return apiFail("invalid_cursor"), false
		}
		start = n
	}
	end := min(start+limit, len(inRange))
	withMeta, _ := in["include_all_metadata"].(bool)
	out := []any{}
	for _, m := range inRange[start:end] {
		o := m.json()
		if withMeta && m.meta != nil {
			o["metadata"] = map[string]any{"event_type": m.meta.EventType, "event_payload": m.meta.EventPayload}
		}
		out = append(out, o)
	}
	res := map[string]any{"ok": true, "messages": out, "has_more": end < len(inRange)}
	if end < len(inRange) {
		res["response_metadata"] = map[string]any{"next_cursor": "cursor-" + strconv.Itoa(end)}
	}
	return res, false
}

// serveHook answers a post to a response_url. The path is the token. The
// first account takes the call when no interaction registered the token.
func (f *fakeSlack) serveHook(w http.ResponseWriter, r *http.Request, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.order) == 0 {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no_service"))
		return nil
	}
	a := f.accounts[f.order[0]]
	token := strings.TrimPrefix(r.URL.Path, "/")
	call := fakeCall{Method: "response_url", Body: body, Token: token, Auth: r.Header.Get("Authorization")}
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
		a.calls = append(a.calls, call)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid_payload"))
		return nil
	}
	ans := accepted
	if len(a.script) > 0 {
		ans, a.script = a.script[0], a.script[1:]
	}
	if ans.unreachable {
		a.calls = append(a.calls, call)
		return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	if ans.status != 0 || ans.errCode != "" {
		a.calls = append(a.calls, call)
		for k, v := range ans.header {
			w.Header().Set(k, v)
		}
		code := cmp(ans.byMethod["response_url"], ans.errCode)
		status := ans.status
		if status == 0 {
			status = http.StatusBadRequest
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(code))
		return nil
	}
	h := f.hooks[token]
	if h == nil {
		h = &fakeHook{}
		f.hooks[token] = h
	}
	switch {
	case h.uses >= 5:
		// "up to 5 times within 30 minutes of receiving the payload"
		a.calls = append(a.calls, call)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no_service"))
		return nil
	case !h.expires.IsZero() && f.now().After(h.expires):
		a.calls = append(a.calls, call)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no_service"))
		return nil
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil || (str(in, "text") == "" && in["blocks"] == nil && in["delete_original"] == nil) {
		a.calls = append(a.calls, call)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("no_text"))
		return nil
	}
	if ans.apply {
		h.uses++
		call.Applied = true
	}
	a.calls = append(a.calls, call)
	if ans.drop {
		return &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
	return nil
}

// holdDM gives user a direct message conversation, id.
func (f *fakeSlack) holdDM(prefix, user, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.dms[user] = id
	a.conversation(id)
}

// touch makes the conversation exist.
func (f *fakeSlack) touch(prefix, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[prefix].conversation(id)
}

// userOfDM is the user a direct message conversation is with, "" for none.
func (f *fakeSlack) userOfDM(prefix, id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for u, d := range f.accounts[prefix].dms {
		if d == id {
			return u
		}
	}
	return ""
}

// setText replaces a message's text, as its author's edit does; the edit is
// stamped, and a deletion removes it.
func (f *fakeSlack) edit(prefix, conversation, ts, text, at string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.accounts[prefix].convs[conversation]; c != nil {
		if m := c.byTS(ts); m != nil {
			m.text, m.edited = text, at
		}
	}
}

func (f *fakeSlack) remove(prefix, conversation, ts string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.accounts[prefix].convs[conversation]; c != nil {
		c.msgs = slices.DeleteFunc(c.msgs, func(m *fakeMsg) bool { return m.ts == ts })
	}
}

// archive runs conversations.archive and conversations.unarchive
// (https://docs.slack.dev/reference/methods/conversations.archive/): a
// direct message cannot be archived, an archived channel answers
// already_archived and an unarchived one not_archived.
func (f *fakeSlack) archive(a *fakeAccount, archive bool, in map[string]any, call *fakeCall, apply bool) (map[string]any, bool) {
	id := str(in, "channel")
	if id == "" {
		return apiFail("channel_not_found"), false
	}
	c := a.convs[id]
	switch {
	case c == nil || !slackID.MatchString(id):
		return apiFail("channel_not_found"), false
	case strings.HasPrefix(id, "D"):
		return apiFail("method_not_supported_for_channel_type"), false
	case c.archived == archive && archive:
		return apiFail("already_archived"), true // the state is held: the call counts as applied
	case c.archived == archive:
		return apiFail("not_archived"), true
	}
	if !apply {
		return map[string]any{"ok": true}, false
	}
	c.archived = archive
	call.Channel = id
	return map[string]any{"ok": true}, true
}

// info runs conversations.info, answering what reconciliation reads.
func (f *fakeSlack) info(a *fakeAccount, in map[string]any) (map[string]any, bool) {
	c := a.convs[str(in, "channel")]
	if c == nil {
		return apiFail("channel_not_found"), false
	}
	return map[string]any{"ok": true, "channel": map[string]any{"id": c.id, "is_archived": c.archived}}, false
}

// setArchived changes a channel as a person would in the client, with no
// call of the app.
func (f *fakeSlack) setArchived(prefix, conversation string, archived bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[prefix].conversation(conversation).archived = archived
}

// archivedChannel reports whether the account's channel is archived.
func (f *fakeSlack) archivedChannel(prefix, conversation string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.accounts[prefix].convs[conversation]
	return c != nil && c.archived
}
