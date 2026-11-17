package discord

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

// fakeDiscord is a stand-in Discord REST API for the routes the adapter
// calls, answering in the shapes the references document:
//
//   - messages, reactions: https://docs.discord.com/developers/resources/message
//   - channels, typing, thread members: https://docs.discord.com/developers/resources/channel
//   - Create DM: https://docs.discord.com/developers/resources/user
//   - Get Gateway Bot: https://docs.discord.com/developers/events/gateway
//   - interaction callbacks and webhooks:
//     https://docs.discord.com/developers/interactions/receiving-and-responding
//   - errors {"code", "message"} with the HTTP statuses of
//     https://docs.discord.com/developers/topics/opcodes-and-status-codes;
//     429 with retry_after and global, and X-RateLimit-* headers:
//     https://docs.discord.com/developers/topics/rate-limits
//
// It is not evidence about Discord: what the references leave open (does a
// read return a message's nonce; does after answer the messages nearest
// the ID) is answered one way here and named in the adapter's page. Each
// account is a path prefix ("https://discord.test/<account>/api/v10/"), its
// own bot with its own channels, script and call record. The gateway side
// is fakegateway_test.go.
type fakeDiscord struct {
	now func() time.Time

	mu       sync.Mutex
	accounts map[string]*fakeAccount
	order    []string
	// gwSrv is the gateway's WebSocket server, started on first use.
	gwSrv *httptest.Server
}

const (
	fakeAPIHost     = "discord.test"
	fakeGatewayHost = "gateway.discord.test"
	fakeResumeHost  = "resume.discord.test"
)

type fakeAccount struct {
	prefix, token, botUser, app string
	channels                    map[string]*fakeChannel
	dms                         map[string]string // user -> DM channel
	seq                         uint64
	script                      []answer
	calls                       []fakeCall
	// lenient lets the bot change any message, as the suite edits and
	// deletes messages of people; Discord answers 50005 for those, which
	// the suite scripts.
	lenient bool
	// nonceInReads makes reads return a message's nonce; the reference does
	// not say they do.
	nonceInReads bool
	// originals holds each interaction token's original response.
	originals map[string]*fakeMsg
	// startsLeft is session_start_limit.remaining.
	startsLeft int
	// gateway side (fakegateway_test.go)
	gw *fakeGateway
}

type fakeChannel struct {
	id, parent string
	typ        int
	msgs       []*fakeMsg
	members    map[string]bool
	// archived is a thread's archived flag (thread_metadata), archivedAt
	// when it last changed.
	archived   bool
	archivedAt time.Time
}

type fakeMsg struct {
	id, channel, author, content, nonce string
	bot                                 bool
	components                          json.RawMessage
	edited                              string
	flags                               int
	reactions                           map[string][]string
	at                                  time.Time
}

// fakeCall is one request the account received.
type fakeCall struct {
	Method, Path string
	Query        url.Values
	Body         []byte
	Auth, Agent  string
	Applied      bool
	// Channel and Message name what the call created or touched.
	Channel, Message string
}

// answer is one scripted answer to an effect. The zero value accepts.
type answer struct {
	// drop closes the connection instead of answering; unreachable fails
	// the dial. apply carries the effect out before either.
	apply, drop, unreachable bool
	status, code             int
	header                   map[string]string
	body                     string
}

var (
	accepted    = answer{apply: true}
	answerLost  = answer{apply: true, drop: true}
	requestLost = answer{drop: true}
	unreachable = answer{unreachable: true}
)

func refusal(status, code int) answer { return answer{status: status, code: code} }

func rateLimitedFor(d time.Duration, global bool) answer {
	secs := strconv.FormatFloat(d.Seconds(), 'f', 3, 64)
	return answer{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": strconv.Itoa(int(d.Seconds())), "X-RateLimit-Scope": "user"},
		body: `{"message":"You are being rate limited.","retry_after":` + secs + `,"global":` + strconv.FormatBool(global) + `}`}
}

func newFakeDiscord(now func() time.Time) *fakeDiscord {
	return &fakeDiscord{now: now, accounts: map[string]*fakeAccount{}}
}

// account returns the account under prefix, creating it.
func (f *fakeDiscord) account(prefix, token, botUser, app string) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[prefix]
	if !ok {
		a = &fakeAccount{prefix: prefix, token: token, botUser: botUser, app: app, channels: map[string]*fakeChannel{}, dms: map[string]string{},
			originals: map[string]*fakeMsg{}, startsLeft: 1000}
		a.gw = newFakeGateway(f, a)
		f.accounts[prefix] = a
		f.order = append(f.order, prefix)
	}
	return a
}

func (f *fakeDiscord) apiURL(prefix string) string {
	return "https://" + fakeAPIHost + "/" + prefix + "/api/v10/"
}

func (f *fakeDiscord) respond(prefix string, as ...answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.script = append(a.script, as...)
}

func (f *fakeDiscord) calls(prefix string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.accounts[prefix]; a != nil {
		return slices.Clone(a.calls)
	}
	return nil
}

// channel returns the account's channel id, creating it as a text channel.
func (a *fakeAccount) channel(id string) *fakeChannel {
	c, ok := a.channels[id]
	if !ok {
		c = &fakeChannel{id: id, members: map[string]bool{}}
		a.channels[id] = c
	}
	return c
}

// nextID returns a snowflake after every earlier one of the account.
func (f *fakeDiscord) nextID(a *fakeAccount) string {
	a.seq++
	return sid(f.now(), a.seq)
}

// thread makes id a public thread of parent.
func (f *fakeDiscord) thread(prefix, parent, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.accounts[prefix].channel(id)
	c.typ, c.parent = 11, parent
	f.accounts[prefix].channel(parent)
}

// put stores a message as if written earlier, by author, with id.
func (f *fakeDiscord) put(prefix, channel, author, content, id string) *fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	if id == "" {
		id = f.nextID(a)
	}
	m := &fakeMsg{id: id, channel: channel, author: author, content: content, at: snowflakeTime(id)}
	c := a.channel(channel)
	c.msgs = append(c.msgs, m)
	slices.SortFunc(c.msgs, func(x, y *fakeMsg) int { return compareSnowflake(x.id, y.id) })
	return m
}

func (f *fakeDiscord) find(prefix, channel, id string) *fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.accounts[prefix].channels[channel]; c != nil {
		return c.byID(id)
	}
	return nil
}

func (c *fakeChannel) byID(id string) *fakeMsg {
	for _, m := range c.msgs {
		if m.id == id {
			return m
		}
	}
	return nil
}

// holdDM gives user a direct message channel, id.
func (f *fakeDiscord) holdDM(prefix, user, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.dms[user] = id
	a.channel(id).typ = 1
}

// client returns the HTTP client the adapter uses: it serves the fake in
// process and refuses any other host, so a test never reaches Discord.
func (f *fakeDiscord) client() ports.HTTPClient { return fakeClient{f} }

type fakeClient struct{ f *fakeDiscord }

func (c fakeClient) Do(r *http.Request) (*http.Response, error) {
	f := c.f
	if r.URL.Host == fakeGatewayHost || r.URL.Host == fakeResumeHost {
		return f.gatewayRoundTrip(r)
	}
	if r.URL.Scheme != "https" || r.URL.Host != fakeAPIHost {
		return nil, fmt.Errorf("fake Discord: refusing request to %s", r.URL)
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

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func fail(w http.ResponseWriter, status, code int) {
	writeJSONStatus(w, status, map[string]any{"code": code, "message": http.StatusText(status)})
}

// effectRoute reports whether a request changes what Discord holds: those
// take the script's answers.
func effectRoute(method string, segs []string) bool {
	switch {
	case method == http.MethodGet:
		return false
	case segs[0] == "channels" && len(segs) >= 3 && segs[2] == "messages":
		return true
	case segs[0] == "channels" && len(segs) >= 3 && (segs[2] == "typing" || segs[2] == "thread-members"):
		return true
	case segs[0] == "channels" && len(segs) == 2:
		return true // Modify Channel
	case segs[0] == "webhooks":
		return true
	}
	return false
}

// serve answers one request, or fails the connection with the returned
// error.
func (f *fakeDiscord) serve(w http.ResponseWriter, r *http.Request, body []byte) error {
	prefix, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	path, ok := strings.CutPrefix(rest, "api/v10/")
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	if a == nil || !ok {
		fail(w, http.StatusNotFound, 0)
		return nil
	}
	segs := strings.Split(path, "/")
	call := fakeCall{Method: r.Method, Path: path, Query: r.URL.Query(), Body: body, Auth: r.Header.Get("Authorization"), Agent: r.Header.Get("User-Agent")}
	bare := segs[0] == "webhooks" || segs[0] == "interactions"
	if !bare && call.Auth != "Bot "+a.token {
		a.calls = append(a.calls, call)
		fail(w, http.StatusUnauthorized, 0)
		return nil
	}
	ans := accepted
	if effectRoute(r.Method, segs) && len(a.script) > 0 {
		ans, a.script = a.script[0], a.script[1:]
	}
	if ans.unreachable {
		a.calls = append(a.calls, call)
		return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	if ans.status != 0 && !ans.apply {
		a.calls = append(a.calls, call)
		for k, v := range ans.header {
			w.Header().Set(k, v)
		}
		if ans.body != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(ans.status)
			_, _ = w.Write([]byte(ans.body))
			return nil
		}
		fail(w, ans.status, ans.code)
		return nil
	}
	if ans.drop && !ans.apply {
		a.calls = append(a.calls, call)
		return &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}
	}
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	rec := httptest.NewRecorder()
	call.Applied = f.route(a, r.Method, segs, call.Query, in, rec, &call)
	a.calls = append(a.calls, call)
	if ans.drop {
		return &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	for k, v := range ans.header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
	return nil
}

func str(in map[string]any, k string) string {
	s, _ := in[k].(string)
	return s
}

// route runs one request on account a (locked) and reports whether it
// changed what the account holds.
func (f *fakeDiscord) route(a *fakeAccount, method string, segs []string, q url.Values, in map[string]any, w http.ResponseWriter, call *fakeCall) bool {
	n := len(segs)
	switch {
	case n == 2 && segs[0] == "gateway" && segs[1] == "bot" && method == http.MethodGet:
		writeJSONStatus(w, 200, map[string]any{"url": a.gatewayURL(), "shards": 1,
			"session_start_limit": map[string]any{"total": 1000, "remaining": a.startsLeft, "reset_after": 3_600_000, "max_concurrency": 1}})
	case n == 3 && segs[0] == "users" && segs[1] == "@me" && segs[2] == "channels" && method == http.MethodPost:
		u := str(in, "recipient_id")
		if !isSnowflake(u) {
			fail(w, 400, codeInvalidFormBody)
			return false
		}
		id, ok := a.dms[u]
		if !ok {
			id = f.nextID(a)
			a.dms[u] = id
			a.channel(id).typ = 1
		}
		writeJSONStatus(w, 200, map[string]any{"id": id, "type": 1, "recipients": []any{map[string]any{"id": u}}})
	case segs[0] == "channels" && n >= 2:
		return f.channelRoute(a, method, segs, q, in, w, call)
	case segs[0] == "interactions" && n == 4 && segs[3] == "callback" && method == http.MethodPost:
		typ, _ := in["type"].(float64)
		f.deferred(a, segs[2], int(typ))
		w.WriteHeader(http.StatusNoContent)
	case segs[0] == "webhooks" && n >= 3:
		return f.webhookRoute(a, method, segs, in, w, call)
	default:
		fail(w, 404, 0)
	}
	return false
}

func (f *fakeDiscord) channelRoute(a *fakeAccount, method string, segs []string, q url.Values, in map[string]any, w http.ResponseWriter, call *fakeCall) bool {
	c := a.channels[segs[1]]
	if c == nil {
		fail(w, 404, codeUnknownChannel)
		return false
	}
	call.Channel = c.id
	n := len(segs)
	switch {
	case n == 2 && method == http.MethodGet:
		writeJSONStatus(w, 200, c.json())
		return false
	case n == 2 && method == http.MethodPatch:
		// Modify Channel on a thread: archived is the only field the
		// adapter sets. Archiving or unarchiving the state held is applied.
		if c.typ < 10 || c.typ > 12 {
			fail(w, 400, codeInvalidFormBody)
			return false
		}
		if archived, ok := in["archived"].(bool); ok {
			if archived != c.archived {
				c.archivedAt = f.now()
			}
			c.archived = archived
		}
		writeJSONStatus(w, 200, c.json())
		return true
	case n == 3 && segs[2] == "typing" && method == http.MethodPost:
		w.WriteHeader(http.StatusNoContent)
		return true
	case n == 4 && segs[2] == "thread-members":
		if c.typ < 10 || c.typ > 12 {
			fail(w, 400, codeInvalidFormBody)
			return false
		}
		switch method {
		case http.MethodGet:
			if !c.members[segs[3]] {
				fail(w, 404, codeUnknownMember)
				return false
			}
			writeJSONStatus(w, 200, map[string]any{"id": c.id, "user_id": segs[3]})
			return false
		case http.MethodPut:
			c.members[segs[3]] = true
			w.WriteHeader(http.StatusNoContent)
			return true
		case http.MethodDelete:
			delete(c.members, segs[3])
			w.WriteHeader(http.StatusNoContent)
			return true
		}
	case n == 3 && segs[2] == "messages" && method == http.MethodGet:
		f.listMessages(a, c, q, w)
		return false
	case n == 3 && segs[2] == "messages" && method == http.MethodPost:
		return f.createMessage(a, c, in, w, call)
	case n >= 4 && segs[2] == "messages":
		m := c.byID(segs[3])
		if m == nil {
			fail(w, 404, codeUnknownMessage)
			return false
		}
		call.Message = m.id
		switch {
		case n == 4 && method == http.MethodGet:
			writeJSONStatus(w, 200, m.json(a, a.nonceInReads))
			return false
		case n == 4 && method == http.MethodPatch:
			if m.author != a.botUser && !a.lenient {
				fail(w, 403, codeEditOthers)
				return false
			}
			m.content = str(in, "content")
			m.components, _ = json.Marshal(in["components"])
			m.edited = f.now().UTC().Format(time.RFC3339Nano)
			writeJSONStatus(w, 200, m.json(a, false))
			return true
		case n == 4 && method == http.MethodDelete:
			if m.author != a.botUser && !a.lenient {
				fail(w, 403, codeMissingPermissions)
				return false
			}
			c.msgs = slices.DeleteFunc(c.msgs, func(x *fakeMsg) bool { return x == m })
			w.WriteHeader(http.StatusNoContent)
			return true
		case n == 7 && segs[4] == "reactions" && segs[6] == "@me":
			emoji, err := url.PathUnescape(segs[5])
			if err != nil || emoji == "" {
				fail(w, 400, codeUnknownEmoji)
				return false
			}
			if m.reactions == nil {
				m.reactions = map[string][]string{}
			}
			if method == http.MethodPut {
				if !slices.Contains(m.reactions[emoji], a.botUser) {
					m.reactions[emoji] = append(m.reactions[emoji], a.botUser)
				}
			} else {
				m.reactions[emoji] = slices.DeleteFunc(m.reactions[emoji], func(u string) bool { return u == a.botUser })
				if len(m.reactions[emoji]) == 0 {
					delete(m.reactions, emoji)
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return true
		}
	}
	fail(w, 404, 0)
	return false
}

// json is the channel object: for a thread, with its parent and its
// thread_metadata.
func (c *fakeChannel) json() map[string]any {
	o := map[string]any{"id": c.id, "type": c.typ}
	if c.parent != "" {
		o["parent_id"] = c.parent
	}
	if c.typ >= 10 && c.typ <= 12 {
		meta := map[string]any{"archived": c.archived, "auto_archive_duration": 1440, "locked": false}
		if !c.archivedAt.IsZero() {
			meta["archive_timestamp"] = c.archivedAt.UTC().Format("2006-01-02T15:04:05.000000+00:00")
		}
		o["thread_metadata"] = meta
	}
	return o
}

// nonceWindow is the fake's reading of enforce_nonce's "past few minutes".
const nonceWindow = 5 * time.Minute

func (f *fakeDiscord) createMessage(a *fakeAccount, c *fakeChannel, in map[string]any, w http.ResponseWriter, call *fakeCall) bool {
	content := str(in, "content")
	if content == "" && in["components"] == nil {
		fail(w, 400, codeEmptyMessage)
		return false
	}
	nonce := str(in, "nonce")
	if enforce, _ := in["enforce_nonce"].(bool); enforce && nonce != "" {
		for _, m := range c.msgs {
			if m.nonce == nonce && m.author == a.botUser && f.now().Sub(m.at) < nonceWindow {
				call.Message = m.id
				writeJSONStatus(w, 200, m.json(a, true))
				return false
			}
		}
	}
	if ref, ok := in["message_reference"].(map[string]any); ok {
		if c.byID(str(ref, "message_id")) == nil && ref["fail_if_not_exists"] != false {
			fail(w, 400, codeInvalidFormBody)
			return false
		}
	}
	m := &fakeMsg{id: f.nextID(a), channel: c.id, author: a.botUser, bot: true, content: content, nonce: nonce, at: f.now()}
	if in["components"] != nil {
		m.components, _ = json.Marshal(in["components"])
	}
	c.msgs = append(c.msgs, m)
	call.Message = m.id
	writeJSONStatus(w, 200, m.json(a, true))
	return true
}

// listMessages answers Get Channel Messages: with after, the limit
// messages nearest after the ID, newest first; without, the newest.
func (f *fakeDiscord) listMessages(a *fakeAccount, c *fakeChannel, q url.Values, w http.ResponseWriter) {
	limit := 50
	if l, err := strconv.Atoi(q.Get("limit")); err == nil {
		if l < 1 || l > 100 {
			fail(w, 400, codeInvalidFormBody)
			return
		}
		limit = l
	}
	var sel []*fakeMsg
	if after := q.Get("after"); after != "" {
		for _, m := range c.msgs {
			if compareSnowflake(m.id, after) > 0 {
				sel = append(sel, m)
			}
		}
		if len(sel) > limit {
			sel = sel[:limit]
		}
	} else {
		sel = c.msgs
		if len(sel) > limit {
			sel = sel[len(sel)-limit:]
		}
	}
	out := []any{}
	for i := len(sel) - 1; i >= 0; i-- {
		out = append(out, sel[i].json(a, a.nonceInReads))
	}
	writeJSONStatus(w, 200, out)
}

func (f *fakeDiscord) webhookRoute(a *fakeAccount, method string, segs []string, in map[string]any, w http.ResponseWriter, call *fakeCall) bool {
	if segs[1] != a.app {
		fail(w, 404, codeUnknownWebhook)
		return false
	}
	token := segs[2]
	orig := a.originals[token]
	switch {
	case len(segs) == 5 && segs[4] == "@original" && method == http.MethodGet:
		if orig == nil {
			fail(w, 404, codeUnknownMessage)
			return false
		}
		writeJSONStatus(w, 200, orig.json(a, false))
	case len(segs) == 5 && segs[4] == "@original" && method == http.MethodPatch:
		if orig == nil {
			orig = &fakeMsg{id: f.nextID(a), channel: "1", author: a.botUser, bot: true}
			a.originals[token] = orig
		}
		orig.content, orig.flags = str(in, "content"), orig.flags&^flagLoading
		call.Message = orig.id
		writeJSONStatus(w, 200, orig.json(a, false))
		return true
	case len(segs) == 3 && method == http.MethodPost:
		flags, _ := in["flags"].(float64)
		m := &fakeMsg{id: f.nextID(a), channel: "1", author: a.botUser, bot: true, content: str(in, "content"), flags: int(flags)}
		call.Message = m.id
		writeJSONStatus(w, 200, m.json(a, false))
		return true
	default:
		fail(w, 404, 0)
	}
	return false
}

// json is the message object of the answers.
func (m *fakeMsg) json(a *fakeAccount, withNonce bool) map[string]any {
	o := map[string]any{
		"id": m.id, "channel_id": m.channel, "type": 0, "content": m.content, "flags": m.flags,
		"timestamp": m.at.UTC().Format(time.RFC3339Nano), "edited_timestamp": nil,
		"author": map[string]any{"id": m.author, "username": "u" + m.author, "bot": m.author == a.botUser || m.bot},
	}
	if m.edited != "" {
		o["edited_timestamp"] = m.edited
	}
	if withNonce && m.nonce != "" {
		o["nonce"] = m.nonce
	}
	if len(m.components) > 0 && string(m.components) != "null" {
		o["components"] = json.RawMessage(m.components)
	}
	if len(m.reactions) > 0 {
		var names []string
		for n := range m.reactions {
			names = append(names, n)
		}
		slices.Sort(names)
		rs := []any{}
		for _, n := range names {
			rs = append(rs, map[string]any{"count": len(m.reactions[n]), "me": slices.Contains(m.reactions[n], a.botUser), "emoji": map[string]any{"id": nil, "name": n}})
		}
		o["reactions"] = rs
	}
	return o
}

// deferred records an interaction's acknowledgement: a deferred channel
// message is an original response showing loading until it is edited.
func (f *fakeDiscord) deferred(a *fakeAccount, token string, typ int) {
	if typ == callbackDeferredMessage {
		a.originals[token] = &fakeMsg{id: f.nextID(a), channel: "1", author: a.botUser, bot: true, flags: flagLoading}
	}
}

// acknowledge records an acknowledgement the handler answered with.
func (f *fakeDiscord) acknowledge(prefix, token string, typ int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deferred(f.accounts[prefix], token, typ)
}
