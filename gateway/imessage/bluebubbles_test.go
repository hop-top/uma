package imessage

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// bbPassword is a fixture value, not a credential.
const bbPassword = "bb-test-password" // #nosec G101

// standInBlueBubbles is a stand-in for BlueBubbles Server's REST API,
// modelled on the Postman reference linked from
// https://docs.bluebubbles.app/server/developer-guides/rest-api-and-webhooks
// (POST /api/v1/message/text?password=..., body chatGuid, tempGuid, message,
// method) and the server's answer envelope {status, message, data}, data
// the sent message with its guid and the tempGuid echoed.
type standInBlueBubbles struct {
	srv *httptest.Server

	mu     sync.Mutex
	reqs   []seenRequest
	status int
}

func newStandInBlueBubbles(t *testing.T) *standInBlueBubbles {
	t.Helper()
	b := &standInBlueBubbles{status: http.StatusOK}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		b.mu.Lock()
		b.reqs = append(b.reqs, seenRequest{r.Method, r.URL.RequestURI(), r.Header.Clone(), m})
		status := b.status
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = io.WriteString(w, `{"status":`+strconv.Itoa(status)+`,"message":"error","error":{"type":"x","error":"y"}}`)
			return
		}
		out, _ := json.Marshal(map[string]any{"status": 200, "message": "Message sent!", "data": map[string]any{
			"guid": "p:0/AAAA-0001", "tempGuid": m["tempGuid"], "text": m["message"], "isFromMe": true, "handle": nil,
			"chats": []any{map[string]any{"guid": m["chatGuid"]}},
		}})
		_, _ = w.Write(out)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *standInBlueBubbles) backend(t *testing.T) *BlueBubbles {
	t.Helper()
	be, err := NewBlueBubbles(BlueBubblesOptions{URL: b.srv.URL, Password: bbPassword, Account: "me@example.com", Client: b.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return be
}

func TestBlueBubblesSendRequest(t *testing.T) {
	sb := newStandInBlueBubbles(t)
	res := sb.backend(t).Send(t.Context(), Outbound{Key: "op-1", To: "+15550100001", Text: "hi"})
	if res.Outcome != Sent || res.MessageID != "p:0/AAAA-0001" {
		t.Fatalf("result %+v, want sent p:0/AAAA-0001", res)
	}
	req := sb.reqs[0]
	if req.Method != http.MethodPost || req.Path != "/api/v1/message/text?password="+bbPassword {
		t.Errorf("request %s %s", req.Method, req.Path)
	}
	want := map[string]any{"chatGuid": "iMessage;-;+15550100001", "tempGuid": "op-1", "message": "hi", "method": "apple-script"}
	if len(req.Body) != len(want) {
		t.Errorf("body %v, want %v", req.Body, want)
	}
	for k, v := range want {
		if req.Body[k] != v {
			t.Errorf("body %s = %v, want %v", k, req.Body[k], v)
		}
	}
}

func TestBlueBubblesSendAnswers(t *testing.T) {
	for status, want := range map[int]Outcome{
		400: Rejected, 404: Rejected, 422: Rejected, 401: NotSent, 408: Unknown, 500: Unknown, 502: Unknown, 202: Unknown,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			sb := newStandInBlueBubbles(t)
			sb.status = status
			if res := sb.backend(t).Send(t.Context(), Outbound{Key: "op-1", To: "+15550100001", Text: "hi"}); res.Outcome != want {
				t.Fatalf("result %+v, want %v", res, want)
			}
		})
	}
}

// A transport failure never reaches a result or a log line with the URL,
// which holds the server password.
func TestBlueBubblesErrorsHoldNoPassword(t *testing.T) {
	sb := newStandInBlueBubbles(t)
	be := sb.backend(t)
	sb.srv.Close()
	res := be.Send(t.Context(), Outbound{Key: "op-1", To: "+15550100001", Text: "hi"})
	if res.Outcome != NotSent || strings.Contains(res.Detail, bbPassword) {
		t.Fatalf("result %+v", res)
	}
}

func TestNewBlueBubblesRefuses(t *testing.T) {
	for name, o := range map[string]BlueBubblesOptions{
		"no URL":        {Password: "p", Account: "a"},
		"query":         {URL: "http://192.0.2.1:1234/?guid=x", Password: "p", Account: "a"},
		"other scheme":  {URL: "ftp://192.0.2.1", Password: "p", Account: "a"},
		"no password":   {URL: "http://192.0.2.1:1234", Account: "a"},
		"no account":    {URL: "http://192.0.2.1:1234", Password: "p"},
		"credentials":   {URL: "http://u:p@192.0.2.1:1234", Password: "p", Account: "a"}, // #nosec G101 -- refused fixture
		"relative path": {URL: "/api", Password: "p", Account: "a"},
	} {
		if _, err := NewBlueBubbles(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// bbNewMessage is a new-message webhook as BlueBubbles Server posts it
// ({"type","data"}, data a serialized message: guid, text, handle.address,
// isFromMe, chats[].guid). Field names from the server's webhook service and
// message serializer (github.com/BlueBubblesApp/bluebubbles-server,
// packages/server/src/server/services/webhookService and
// api/serializers/MessageSerializer.ts). Stand-in content.
const bbNewMessage = `{"type":"new-message","data":{"originalROWID":12,"guid":"AAAA-IN-0001","text":"recorded test reply",` +
	`"handle":{"originalROWID":3,"address":"+15550100001","service":"iMessage","country":"us"},"handleId":3,` +
	`"attachments":[],"dateCreated":1760000000000,"isFromMe":false,"associatedMessageGuid":null,"associatedMessageType":null,` +
	`"chats":[{"originalROWID":4,"guid":"iMessage;-;+15550100001","style":45,"chatIdentifier":"+15550100001","displayName":""}]}}`

func TestBlueBubblesMessages(t *testing.T) {
	be := newStandInBlueBubbles(t).backend(t)
	got, err := be.Messages([]byte(bbNewMessage))
	if err != nil || len(got) != 1 {
		t.Fatalf("Messages = %v, %v", got, err)
	}
	if g := got[0]; g.From != "+15550100001" || g.To != "me@example.com" || g.Body != "recorded test reply" || g.MessageID != "AAAA-IN-0001" {
		t.Fatalf("message %+v", g)
	}
	attachment := strings.Replace(bbNewMessage, `"recorded test reply"`, `"￼look"`, 1)
	if got, _ := be.Messages([]byte(attachment)); len(got) != 1 || got[0].Body != "look" {
		t.Errorf("text around an attachment = %+v, want look", got)
	}
	drop := map[string]string{
		"own message":     strings.Replace(bbNewMessage, `"isFromMe":false`, `"isFromMe":true`, 1),
		"reaction":        strings.Replace(bbNewMessage, `"associatedMessageGuid":null`, `"associatedMessageGuid":"p:0/AAAA-0001"`, 1),
		"group chat":      strings.Replace(bbNewMessage, `"guid":"iMessage;-;+15550100001"`, `"guid":"iMessage;+;chat123"`, 1),
		"updated message": strings.Replace(bbNewMessage, `"new-message"`, `"updated-message"`, 1),
		"attachment only": strings.Replace(bbNewMessage, `"recorded test reply"`, `"￼"`, 1),
		"no handle":       strings.Replace(bbNewMessage, `"handle":{"originalROWID":3,"address":"+15550100001","service":"iMessage","country":"us"}`, `"handle":null`, 1),
		"no chat":         strings.Replace(bbNewMessage, `"chats":[{"originalROWID":4,"guid":"iMessage;-;+15550100001","style":45,"chatIdentifier":"+15550100001","displayName":""}]`, `"chats":[]`, 1),
	}
	for name, body := range drop {
		t.Run(name, func(t *testing.T) {
			if body == bbNewMessage {
				t.Fatal("fixture not changed")
			}
			if got, err := be.Messages([]byte(body)); err != nil || len(got) != 0 {
				t.Fatalf("Messages = %v, %v; want nothing", got, err)
			}
		})
	}
}
