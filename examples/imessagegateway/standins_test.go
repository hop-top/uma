package imessagegateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// Stand-ins for the two backends and a webhook fixture each, for the
// end-to-end stand-in tests. The gateway package's own tests hold the same
// stand-ins; these are the copies this module's tests need, since test files
// do not cross packages.

// standInBlooio is a stand-in for the Blooio v4 API, modelled on
// https://docs.blooio.com/llms.txt (POST /messages, operationId
// sendMessage: from, to, text, Idempotency-Key header) and
// https://docs.blooio.com/guides/message-fields-v4 (the send answers the
// message object at the top level, id prefixed msg_). It answers what the
// test scripts; by default 202 with a message object naming the request's
// sender and recipient.
type standInBlooio struct {
	srv *httptest.Server

	mu     sync.Mutex
	reqs   []seenRequest
	status int
	body   string
	header http.Header
}

type seenRequest struct {
	Method, Path string
	Header       http.Header
	Body         map[string]any
}

func newStandInBlooio(t *testing.T) *standInBlooio {
	t.Helper()
	b := &standInBlooio{status: http.StatusAccepted}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		b.mu.Lock()
		b.reqs = append(b.reqs, seenRequest{r.Method, r.URL.RequestURI(), r.Header.Clone(), m})
		status, body, header := b.status, b.body, b.header
		b.mu.Unlock()
		for k, v := range header {
			w.Header()[k] = v
		}
		if body == "" {
			echo, _ := json.Marshal(map[string]any{
				"id": "msg_0001", "chat_id": "chat_0001", "channel_type": "blooio", "direction": "outbound",
				"sender": m["from"], "recipient": m["to"], "text": m["text"], "status": "queued", "protocol": "pending",
			})
			body = string(echo)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// blooioReceived is a message.received event as Blooio's v4 webhooks carry
// it (https://docs.blooio.com/guides/twilio-integration, "Receiving
// messages"; payload version 2026-10-01). Stand-in content.
const blooioReceived = `{"id":"evt_0001","type":"message.received","api_version":"2026-10-01","created_at":1760000000000,` +
	`"organization_id":"org_test","data":{"message_id":"msg_in_0001","chat_id":"chat_0001","channel_id":"ch_0001",` +
	`"channel_type":"blooio","channel_address":"+15550100002","kind":"received","direction":"inbound","status":"received",` +
	`"protocol":"imessage","message_type":"text","text":"recorded test reply","sender":"alice@example.com",` +
	`"recipient":"+15550100002","contact":{"contact_id":"ct_0001","identifier":"alice@example.com","name":null},` +
	`"group":null,"attachments":[{"url":"https://cdn.example.com/a.jpg","media_type":"image/jpeg","size":10}]}}`

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

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
