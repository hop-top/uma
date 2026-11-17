package telegram

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The scripted bot is the evidence every other test stands on, so what it
// claims of the Bot API is checked on its own.

func rawCall(t *testing.T, r *rig, method string, args map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(args)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, r.cfg.APIURL+"bot"+testToken+"/"+method, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	res, err := r.f.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestFakeFollowsTheDocumentedShapes(t *testing.T) {
	r := newRig(t)
	if code, out := rawCall(t, r, "getMe", nil); code != 200 || out["ok"] != true {
		t.Errorf("getMe %d %v", code, out)
	}
	// An error is an object with ok false, an error_code and a description,
	// on a matching HTTP status.
	code, out := rawCall(t, r, "sendMessage", map[string]any{"chat_id": testGroup, "text": ""})
	if code != 400 || out["ok"] != false || out["error_code"] != float64(400) || out["description"] == "" {
		t.Errorf("empty text: %d %v", code, out)
	}
	// A bot cannot message a user who never started it.
	if code, out = rawCall(t, r, "sendMessage", map[string]any{"chat_id": 777, "text": "hi"}); code != 403 {
		t.Errorf("unstarted user: %d %v", code, out)
	}
	// A wrong token is 401.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, r.cfg.APIURL+"botwrong/getMe", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if res, err := r.f.client().Do(req); err != nil || res.StatusCode != 401 {
		t.Errorf("wrong token: %v %v", res, err)
	}
	// 4096 characters fit and 4097 do not.
	if code, _ = rawCall(t, r, "sendMessage", map[string]any{"chat_id": testGroup, "text": strings.Repeat("a", 4096)}); code != 200 {
		t.Errorf("4096 characters: %d", code)
	}
	if code, _ = rawCall(t, r, "sendMessage", map[string]any{"chat_id": testGroup, "text": strings.Repeat("a", 4097)}); code != 400 {
		t.Errorf("4097 characters: %d", code)
	}
	// The foreign host is refused: no test reaches Telegram.
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.telegram.org/bot"+testToken+"/getMe", strings.NewReader("{}"))
	if _, err := r.f.client().Do(req); err == nil {
		t.Error("the fake served a request to the real host")
	}
}

func TestFakeGetUpdatesConfirmsByOffset(t *testing.T) {
	r := newRig(t)
	for id := int64(10); id <= 12; id++ {
		r.f.queueUpdate("acct1", obj{"update_id": id})
	}
	ids := func(out map[string]any) []float64 {
		var got []float64
		for _, u := range out["result"].([]any) {
			got = append(got, u.(map[string]any)["update_id"].(float64))
		}
		return got
	}
	_, out := rawCall(t, r, "getUpdates", obj{"timeout": 0})
	if got := ids(out); len(got) != 3 || got[0] != 10 {
		t.Fatalf("first read %v", got)
	}
	// Unconfirmed updates come again; an offset above an ID confirms it.
	_, out = rawCall(t, r, "getUpdates", obj{"offset": 12})
	if got := ids(out); len(got) != 1 || got[0] != 12 {
		t.Fatalf("after offset 12: %v", got)
	}
	_, out = rawCall(t, r, "getUpdates", obj{"offset": 13})
	if got := ids(out); len(got) != 0 {
		t.Fatalf("after offset 13: %v", got)
	}
	// While a webhook is set getUpdates is a conflict.
	r.acct.webhook = true
	if code, _ := rawCall(t, r, "getUpdates", obj{}); code != 409 {
		t.Errorf("with a webhook: %d, want 409", code)
	}
}

func TestFakeCallbackQueriesAreAnsweredOnce(t *testing.T) {
	r := newRig(t)
	r.f.openCallback("acct1", "cb1", testGroup, 5)
	r.f.openCallback("acct1", "cb2", testGroup, 5)
	r.f.expireCallback("acct1", "cb2")
	if code, _ := rawCall(t, r, "answerCallbackQuery", obj{"callback_query_id": "cb1", "text": "ok"}); code != 200 {
		t.Errorf("first answer: %d", code)
	}
	if code, _ := rawCall(t, r, "answerCallbackQuery", obj{"callback_query_id": "cb1"}); code != 400 {
		t.Errorf("second answer: %d, want 400", code)
	}
	if code, _ := rawCall(t, r, "answerCallbackQuery", obj{"callback_query_id": "cb2"}); code != 400 {
		t.Errorf("an old query: %d, want 400", code)
	}
	if code, _ := rawCall(t, r, "answerCallbackQuery", obj{"callback_query_id": "nope"}); code != 400 {
		t.Errorf("an unknown query: %d, want 400", code)
	}
	if !r.f.callback("acct1", "cb1").answered {
		t.Error("cb1 not answered")
	}
}

func TestFakeMessagesAndRules(t *testing.T) {
	r := newRig(t)
	m := r.f.put("acct1", testGroup, 55, "from a person", 0, 0)
	if r.f.find("acct1", testGroup, m.id) == nil || len(r.f.messages("acct1", testGroup)) != 1 {
		t.Fatal("put message not found")
	}
	// The bot edits and deletes only its own messages, and deletes only within 48 hours.
	if code, _ := rawCall(t, r, "editMessageText", obj{"chat_id": testGroup, "message_id": m.id, "text": "x"}); code != 400 {
		t.Errorf("editing a person's message: %d", code)
	}
	if code, _ := rawCall(t, r, "deleteMessage", obj{"chat_id": testGroup, "message_id": m.id}); code != 400 {
		t.Errorf("deleting a person's message: %d", code)
	}
	r.acct.lenient = true
	old := r.f.put("acct1", testGroup, 55, "old", 0, 0)
	old.date = testNow.Add(-49 * time.Hour)
	if code, _ := rawCall(t, r, "deleteMessage", obj{"chat_id": testGroup, "message_id": old.id}); code != 400 {
		t.Errorf("deleting after 48 hours: %d", code)
	}
	if code, _ := rawCall(t, r, "deleteMessage", obj{"chat_id": testGroup, "message_id": m.id}); code != 200 {
		t.Errorf("deleting within 48 hours: %d", code)
	}
	if code, _ := rawCall(t, r, "deleteMessage", obj{"chat_id": testGroup, "message_id": m.id}); code != 400 {
		t.Errorf("deleting twice: %d", code)
	}
}
