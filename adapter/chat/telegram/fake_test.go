package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// fakeTelegram is a Bot API server for the methods the adapter calls,
// answering in the documented shapes (https://core.telegram.org/bots/api):
//
//   - sendMessage, editMessageText, deleteMessage, setMessageReaction,
//     answerCallbackQuery, getUpdates, getMe: "The response contains a JSON
//     object, which always has a Boolean field 'ok' and may have an optional
//     String field 'description'. If 'ok' equals True, the request was
//     successful and the result of the query can be found in the 'result'
//     field. In case of an unsuccessful request, 'ok' equals False and the
//     error is explained in the 'description'. An Integer 'error_code' field
//     is also returned" (https://core.telegram.org/bots/api#making-requests)
//   - flood control: 429 with parameters.retry_after
//     (https://core.telegram.org/bots/api#responseparameters)
//
// The reference documents no error description; the descriptions the fake
// answers are the ones the Bot API is known to answer, and the adapter's
// reading of them is as unverified as they are. An account is a path prefix
// ("https://api.telegram.test/<account>/bot<token>/<method>"): its own bot,
// chats, script and call record.
type fakeTelegram struct {
	now func() time.Time

	mu       sync.Mutex
	accounts map[string]*fakeAccount
}

const fakeHost = "api.telegram.test"

type fakeAccount struct {
	prefix string
	token  string
	botID  int64
	chats  map[int64]*fakeChat
	// started are the users who have written to the bot and not blocked it;
	// a bot "can't initiate conversation with a user" outside them.
	started map[int64]bool
	blocked map[int64]bool
	script  []scripted
	// pollScript answers the next getUpdates calls, which are no effect and
	// take no answer of script.
	pollScript []scripted
	calls      []fakeCall
	seq        int64
	// lenient lets the bot change any message, as the adapter suite needs
	// to edit and delete messages of people; Telegram answers "message can't
	// be edited" and "message can't be deleted" for those.
	lenient bool
	// webhook makes getUpdates fail as it does while a webhook is set.
	webhook bool
	// updates are the queued Updates for getUpdates; an offset above an
	// update's ID confirms it, once. An update queued again after that is a
	// new delivery: Telegram never repeats an ID, and a harness that
	// redelivers an event is a provider that sends it twice.
	updates []*fakeUpdate
	// callbacks are the callback queries still open, by ID.
	callbacks map[string]*fakeCallback
	// assume holds every message and callback query a call names to exist,
	// as the adapter suite's fixtures need.
	assume bool
}

type fakeUpdate struct {
	body map[string]any
	done bool
}

type fakeCallback struct {
	chat     int64
	message  int64
	answered bool
	expired  bool
}

type fakeChat struct {
	id   int64
	kind string
	msgs []*fakeMsg
}

type fakeMsg struct {
	id        int64
	thread    int64
	from      int64
	text      string
	parseMode string
	markup    json.RawMessage
	replyTo   int64
	date      time.Time
	// reaction is the bot's single reaction.
	reaction []string
	edited   bool
}

// fakeCall is one request the account received.
type fakeCall struct {
	Method  string
	Body    []byte
	Token   string
	Applied bool
	// Chat and Message name what the call created or touched.
	Chat, Message int64
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
	// status and body answer instead of the method's own answer.
	status int
	body   string
	header map[string]string
	// byMethod answers a method with its own scripted answer instead.
	byMethod map[string]scripted
}

var (
	accepted    = scripted{}
	answerLost  = scripted{apply: true, drop: true}
	requestLost = scripted{drop: true}
	unreachable = scripted{unreachable: true}
)

// failWith answers a Bot API error object with its HTTP status.
func failWith(code int, description string) scripted {
	return scripted{status: code, body: fmt.Sprintf(`{"ok":false,"error_code":%d,"description":%q}`, code, description)}
}

func rateLimitedFor(d time.Duration) scripted {
	secs := int(d / time.Second)
	return scripted{status: http.StatusTooManyRequests, body: fmt.Sprintf(
		`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after %d","parameters":{"retry_after":%d}}`, secs, secs)}
}

func newFakeTelegram(now func() time.Time) *fakeTelegram {
	return &fakeTelegram{now: now, accounts: map[string]*fakeAccount{}}
}

// account returns the account under prefix, creating it as a bot with the
// given ID and token.
func (f *fakeTelegram) account(prefix string, botID int64, token string) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[prefix]
	if !ok {
		a = &fakeAccount{prefix: prefix, token: token, botID: botID, chats: map[int64]*fakeChat{}, started: map[int64]bool{}, blocked: map[int64]bool{}, callbacks: map[string]*fakeCallback{}}
		f.accounts[prefix] = a
	}
	return a
}

func (f *fakeTelegram) apiURL(prefix string) string {
	return "https://" + fakeHost + "/" + prefix + "/"
}

func (f *fakeTelegram) respond(prefix string, rs ...scripted) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.script = append(a.script, rs...)
}

func (f *fakeTelegram) respondPolls(prefix string, rs ...scripted) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.pollScript = append(a.pollScript, rs...)
}

func (f *fakeTelegram) calls(prefix string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.accounts[prefix]; a != nil {
		return slices.Clone(a.calls)
	}
	return nil
}

func (f *fakeTelegram) callsOf(prefix, method string) []fakeCall {
	var out []fakeCall
	for _, c := range f.calls(prefix) {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// chat returns the chat of the account, creating it. A positive ID is a
// private chat with the user of that ID, who has started the bot.
func (a *fakeAccount) chat(id int64) *fakeChat {
	c, ok := a.chats[id]
	if !ok {
		kind := "supergroup"
		if id > 0 {
			kind = "private"
			a.started[id] = true
		}
		c = &fakeChat{id: id, kind: kind}
		a.chats[id] = c
	}
	return c
}

func (a *fakeAccount) nextID(c *fakeChat) int64 {
	a.seq++
	return 1000 + a.seq
}

// put stores a message as if written earlier by from.
func (f *fakeTelegram) put(prefix string, chat int64, from int64, text string, id int64, thread int64) *fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	c := a.chat(chat)
	if id == 0 {
		id = a.nextID(c)
	}
	m := &fakeMsg{id: id, thread: thread, from: from, text: text, date: f.now()}
	c.msgs = append(c.msgs, m)
	return m
}

func (f *fakeTelegram) find(prefix string, chat, id int64) *fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.accounts[prefix].chats[chat]; c != nil {
		for _, m := range c.msgs {
			if m.id == id {
				return m
			}
		}
	}
	return nil
}

func (f *fakeTelegram) messages(prefix string, chat int64) []fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeMsg
	if c := f.accounts[prefix].chats[chat]; c != nil {
		for _, m := range c.msgs {
			out = append(out, *m)
		}
	}
	return out
}

// client returns the HTTP client the adapter uses: it serves the fake in
// process and refuses any other host, so a test never reaches Telegram.
func (f *fakeTelegram) client() ports.HTTPClient { return fakeClient{f} }

type fakeClient struct{ f *fakeTelegram }

func (c fakeClient) Do(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != fakeHost {
		return nil, fmt.Errorf("fake Telegram: refusing request to %s", r.URL.Host)
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	rec := httptest.NewRecorder()
	if err := c.f.serve(rec, r, body); err != nil {
		return nil, err
	}
	res := rec.Result()
	res.Request = r
	return res, nil
}

var effectMethods = []string{"sendMessage", "editMessageText", "deleteMessage", "setMessageReaction", "answerCallbackQuery"}

// serve answers one request, or fails the connection with the returned error.
func (f *fakeTelegram) serve(w http.ResponseWriter, r *http.Request, body []byte) error {
	prefix, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	tokenPart, method, _ := strings.Cut(rest, "/")
	f.mu.Lock()
	a := f.accounts[prefix]
	f.mu.Unlock()
	if a == nil || !strings.HasPrefix(tokenPart, "bot") {
		writeJSON(w, http.StatusNotFound, `{"ok":false,"error_code":404,"description":"Not Found"}`)
		return nil
	}
	call := fakeCall{Method: method, Body: body, Token: strings.TrimPrefix(tokenPart, "bot")}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || call.Token != a.token {
		a.calls = append(a.calls, call)
		writeJSON(w, http.StatusUnauthorized, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		return nil
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		a.calls = append(a.calls, call)
		writeJSON(w, http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: wrong content type"}`)
		return nil
	}
	rep := accepted
	// An answer to a callback query that shows nothing is bookkeeping after
	// a response, not an effect of its own: it takes no scripted answer.
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	bookkeeping := method == "answerCallbackQuery" && in["text"] == nil
	if slices.Contains(effectMethods, method) && !bookkeeping && len(a.script) > 0 {
		rep, a.script = a.script[0], a.script[1:]
	}
	if method == "getUpdates" && len(a.pollScript) > 0 {
		rep, a.pollScript = a.pollScript[0], a.pollScript[1:]
	}
	if m, ok := rep.byMethod[method]; ok {
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
		writeJSON(w, rep.status, rep.body)
		return nil
	}
	apply := rep.apply || !slices.Contains(effectMethods, method) || (!rep.drop)
	res, status, applied := f.handle(a, method, in, &call, apply)
	call.Applied = applied
	a.calls = append(a.calls, call)
	if rep.drop {
		return &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}
	}
	writeJSON(w, status, res)
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func errBody(code int, description string) (string, int) {
	return fmt.Sprintf(`{"ok":false,"error_code":%d,"description":%q}`, code, description), code
}

func okBody(result any) string {
	b, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	return string(b)
}

func num(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func (a *fakeAccount) messageObject(c *fakeChat, m *fakeMsg) map[string]any {
	chat := map[string]any{"id": c.id, "type": c.kind}
	out := map[string]any{
		"message_id": m.id, "chat": chat, "date": m.date.Unix(), "text": m.text,
		"from": map[string]any{"id": m.from, "is_bot": m.from == a.botID, "first_name": "someone"},
	}
	if m.thread != 0 {
		out["message_thread_id"] = m.thread
	}
	return out
}

// handle runs one method. apply is false for an effect whose request is
// lost before Telegram acts on it.
func (f *fakeTelegram) handle(a *fakeAccount, method string, in map[string]any, call *fakeCall, apply bool) (string, int, bool) {
	fail := func(code int, d string) (string, int, bool) {
		b, c := errBody(code, d)
		return b, c, false
	}
	switch method {
	case "getMe":
		return okBody(map[string]any{"id": a.botID, "is_bot": true, "first_name": "uma", "username": testBotName}), http.StatusOK, false
	case "sendMessage":
		chatID := num(in, "chat_id")
		call.Chat = chatID
		if _, known := a.chats[chatID]; !known && chatID > 0 && !a.started[chatID] {
			return fail(403, "Forbidden: bot can't initiate conversation with a user")
		}
		if chatID > 0 && a.blocked[chatID] {
			return fail(403, "Forbidden: bot was blocked by the user")
		}
		if chatID == 0 {
			return fail(400, "Bad Request: chat not found")
		}
		text := str(in, "text")
		switch {
		case text == "":
			return fail(400, "Bad Request: message text is empty")
		case len([]rune(text)) > 4096:
			return fail(400, "Bad Request: message is too long")
		}
		if str(in, "parse_mode") == "HTML" {
			if tag := unsupportedHTMLTag(text); tag != "" {
				return fail(400, fmt.Sprintf("Bad Request: can't parse entities: Unsupported start tag %q at byte offset 0", tag))
			}
		}
		c := a.chat(chatID)
		m := &fakeMsg{id: a.nextID(c), thread: num(in, "message_thread_id"), from: a.botID, text: text, parseMode: str(in, "parse_mode"), date: f.now()}
		if rp, ok := in["reply_parameters"].(map[string]any); ok {
			m.replyTo = num(rp, "message_id")
		}
		if rm, ok := in["reply_markup"]; ok {
			m.markup, _ = json.Marshal(rm)
		}
		call.Message = m.id
		if !apply {
			return okBody(a.messageObject(c, m)), http.StatusOK, false
		}
		c.msgs = append(c.msgs, m)
		return okBody(a.messageObject(c, m)), http.StatusOK, true
	case "editMessageText":
		c, m, errs := f.target(a, in, call, "edit")
		if errs != "" {
			return fail(400, errs)
		}
		if m.from != a.botID && !a.lenient {
			return fail(400, "Bad Request: message can't be edited")
		}
		text := str(in, "text")
		markup, _ := json.Marshal(in["reply_markup"])
		if in["reply_markup"] == nil {
			markup = nil
		}
		switch {
		case text == "":
			return fail(400, "Bad Request: message text is empty")
		case text == m.text && string(markup) == string(m.markup):
			return fail(400, "Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message")
		}
		if apply {
			m.text, m.markup, m.parseMode, m.edited = text, markup, str(in, "parse_mode"), true
		}
		return okBody(a.messageObject(c, m)), http.StatusOK, apply
	case "deleteMessage":
		c, m, errs := f.target(a, in, call, "delete")
		if errs != "" {
			return fail(400, errs)
		}
		if m.from != a.botID && !a.lenient {
			return fail(400, "Bad Request: message can't be deleted")
		}
		if f.now().Sub(m.date) > 48*time.Hour {
			return fail(400, "Bad Request: message can't be deleted for everyone")
		}
		if apply {
			c.msgs = slices.DeleteFunc(c.msgs, func(x *fakeMsg) bool { return x.id == m.id })
		}
		return okBody(true), http.StatusOK, apply
	case "setMessageReaction":
		_, m, errs := f.target(a, in, call, "react")
		if errs != "" {
			return fail(400, errs)
		}
		var names []string
		if rs, ok := in["reaction"].([]any); ok {
			for _, r := range rs {
				ro, _ := r.(map[string]any)
				if str(ro, "type") != "emoji" || !slices.Contains(reactionEmoji, str(ro, "emoji")) {
					return fail(400, "Bad Request: REACTION_INVALID")
				}
				names = append(names, str(ro, "emoji"))
			}
		}
		if len(names) > 1 {
			return fail(400, "Bad Request: REACTION_INVALID")
		}
		if apply {
			m.reaction = names
		}
		return okBody(true), http.StatusOK, apply
	case "answerCallbackQuery":
		id := str(in, "callback_query_id")
		cb := a.callbacks[id]
		if a.assume && id != "" {
			// The suite's fixtures reuse one token for several interactions
			// and answer each: every query is open.
			cb = &fakeCallback{}
			a.callbacks[id] = cb
		}
		if cb == nil || cb.answered || cb.expired {
			return fail(400, "Bad Request: query is too old and response timeout expired or query ID is invalid")
		}
		if len([]rune(str(in, "text"))) > 200 {
			return fail(400, "Bad Request: MESSAGE_TOO_LONG")
		}
		if apply {
			cb.answered = true
		}
		return okBody(true), http.StatusOK, apply
	case "getUpdates":
		return f.getUpdates(a, in)
	}
	return fail(404, "Not Found")
}

// target finds the message an edit, deletion or reaction names.
func (f *fakeTelegram) target(a *fakeAccount, in map[string]any, call *fakeCall, what string) (*fakeChat, *fakeMsg, string) {
	chatID, id := num(in, "chat_id"), num(in, "message_id")
	call.Chat, call.Message = chatID, id
	c := a.chats[chatID]
	if c == nil && a.assume {
		c = a.chat(chatID)
	}
	if c == nil {
		return nil, nil, "Bad Request: chat not found"
	}
	for _, m := range c.msgs {
		if m.id == id {
			return c, m, ""
		}
	}
	if a.assume {
		m := &fakeMsg{id: id, from: a.botID, text: "assumed", date: f.now()}
		c.msgs = append(c.msgs, m)
		return c, m, ""
	}
	return nil, nil, "Bad Request: message to " + what + " not found"
}

// getUpdates answers like the Bot API: "This method will not work if an
// outgoing webhook is set up" (409), updates below offset are confirmed and
// forgotten, and at most limit are returned.
func (f *fakeTelegram) getUpdates(a *fakeAccount, in map[string]any) (string, int, bool) {
	if a.webhook {
		b, c := errBody(409, "Conflict: can't use getUpdates method while webhook is active; use deleteWebhook to delete the webhook first")
		return b, c, false
	}
	if off := num(in, "offset"); off > 0 {
		for _, u := range a.updates {
			if num(u.body, "update_id") < off {
				u.done = true
			}
		}
	}
	limit := int(num(in, "limit"))
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	out := []map[string]any{}
	for _, u := range a.updates {
		if !u.done && len(out) < limit {
			out = append(out, u.body)
		}
	}
	return okBody(out), http.StatusOK, false
}

// queueUpdate adds an Update for getUpdates.
func (f *fakeTelegram) queueUpdate(prefix string, u map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.updates = append(a.updates, &fakeUpdate{body: u})
}

// openCallback registers a callback query the bot may answer.
func (f *fakeTelegram) openCallback(prefix, id string, chat, message int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[prefix].callbacks[id] = &fakeCallback{chat: chat, message: message}
}

func (f *fakeTelegram) expireCallback(prefix, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cb := f.accounts[prefix].callbacks[id]; cb != nil {
		cb.expired = true
	}
}

func (f *fakeTelegram) callback(prefix, id string) fakeCallback {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cb := f.accounts[prefix].callbacks[id]; cb != nil {
		return *cb
	}
	return fakeCallback{}
}

// htmlTags are the tags the HTML parse mode takes: "Only the tags mentioned
// above are currently supported" (https://core.telegram.org/bots/api#html-style).
var htmlTags = []string{"b", "strong", "i", "em", "u", "ins", "s", "strike", "del", "span", "tg-spoiler", "tg-emoji", "tg-time", "a", "code", "pre", "blockquote"}

// unsupportedHTMLTag returns the first start tag of text that the HTML parse
// mode does not take. It reads nothing else of the markup: the scripted bot
// checks the one rule the scenarios rely on, and leaves the rest of the
// parser to the real one.
func unsupportedHTMLTag(text string) string {
	for i := 0; i < len(text); i++ {
		if text[i] != '<' {
			continue
		}
		j := strings.IndexByte(text[i:], '>')
		if j < 0 {
			return text[i:]
		}
		name := strings.TrimPrefix(text[i+1:i+j], "/")
		name, _, _ = strings.Cut(name, " ")
		if !slices.Contains(htmlTags, name) {
			return name
		}
		i += j
	}
	return ""
}
