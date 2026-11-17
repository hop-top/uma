package imessage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// BlueBubblesChatPrefix makes a one-to-one chat GUID of a handle:
// "iMessage;-;<handle>". BlueBubbles Server's AppleScript send uses the chat
// with that GUID, and when Messages has none it falls back to a script that
// addresses the handle directly, so a first message to a handle needs no
// Private API.
const BlueBubblesChatPrefix = "iMessage;-;"

// BlueBubblesOptions configure the BlueBubbles backend.
type BlueBubblesOptions struct {
	// URL is the server's address, such as http://192.0.2.10:1234. Required.
	URL string
	// Password is the server password. Required; never logged. BlueBubbles
	// takes it as the password query parameter, so it travels in the URL:
	// reach the server over a private network or TLS only.
	Password string
	// Account is the Apple ID address the Mac sends from: the "to" of every
	// inbound message (webhooks name no recipient). Required.
	Account string
	// ChatPrefix replaces BlueBubblesChatPrefix, for a Messages release
	// that names one-to-one chats otherwise.
	ChatPrefix string
	// Client is the HTTP client. Nil is http.DefaultClient. A send waits
	// for Messages, so give it a timeout of a minute or more.
	Client *http.Client
}

// BlueBubbles is the BlueBubbles Server backend: AppleScript sends to
// one-to-one chats (no Private API), unsigned webhooks.
type BlueBubbles struct {
	o    BlueBubblesOptions
	send string
}

// NewBlueBubbles checks o and returns the backend.
func NewBlueBubbles(o BlueBubblesOptions) (*BlueBubbles, error) {
	u, err := url.Parse(o.URL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.User != nil:
		return nil, errors.New("bluebubbles: server URL must be an http or https URL without query or credentials")
	case o.Password == "":
		return nil, errors.New("bluebubbles: server password required")
	case o.Account == "":
		return nil, errors.New("bluebubbles: account address required")
	}
	if o.ChatPrefix == "" {
		o.ChatPrefix = BlueBubblesChatPrefix
	}
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/message/text"
	u.RawQuery = url.Values{"password": {o.Password}}.Encode()
	return &BlueBubbles{o: o, send: u.String()}, nil
}

// Name is "bluebubbles".
func (*BlueBubbles) Name() string { return "bluebubbles" }

// SignsWebhooks is false: BlueBubbles signs no webhook.
func (*BlueBubbles) SignsWebhooks() bool { return false }

// Send posts to /api/v1/message/text with method apple-script and the
// relay's key as tempGuid, which BlueBubbles requires for an AppleScript
// send, refuses while a send with it is queued, and echoes in its answer.
// The server answers once Messages has the message. Answers:
//
//   - 200: sent, with the message's guid;
//   - 401: not sent (wrong password);
//   - 400, 404 and any other 4xx: rejected;
//   - 5xx: unknown, since Messages may have sent it (BlueBubbles answers
//     500 for a message Messages reports failed, too).
func (b *BlueBubbles) Send(ctx context.Context, m Outbound) Result {
	data, err := json.Marshal(map[string]string{
		"chatGuid": b.o.ChatPrefix + m.To, "tempGuid": m.Key, "message": m.Text, "method": "apple-script",
	})
	if err != nil {
		return Result{Outcome: Rejected, Detail: "bluebubbles: message cannot be encoded"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.send, bytes.NewReader(data))
	if err != nil {
		return Result{Outcome: Rejected, Detail: "bluebubbles: request cannot be built"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := b.o.Client.Do(req)
	if err != nil {
		// The error names the URL, password included: classify only.
		return classify(err, "bluebubbles")
	}
	defer func() { _ = resp.Body.Close() }()
	code := resp.StatusCode
	detail := "bluebubbles: http " + strconv.Itoa(code)
	switch {
	case code == http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var out struct {
			Data struct {
				GUID string `json:"guid"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &out)
		return Result{Outcome: Sent, MessageID: out.Data.GUID, Detail: detail}
	case code == http.StatusUnauthorized:
		return Result{Outcome: NotSent, Detail: detail}
	case code >= 500 || code == http.StatusRequestTimeout:
		return Result{Outcome: Unknown, Detail: detail}
	case code >= 200 && code < 300:
		return Result{Outcome: Unknown, Detail: detail}
	}
	return Result{Outcome: Rejected, Detail: detail}
}

// VerifyWebhook accepts every request: BlueBubbles signs nothing, and the
// gateway checks the secret path token before this runs.
func (*BlueBubbles) VerifyWebhook(http.Header, []byte) error { return nil }

// bbEvent is a BlueBubbles webhook: {"type", "data"}, data a serialized
// message for new-message.
type bbEvent struct {
	Type string `json:"type"`
	Data struct {
		GUID                  string `json:"guid"`
		Text                  string `json:"text"`
		IsFromMe              bool   `json:"isFromMe"`
		AssociatedMessageGUID string `json:"associatedMessageGuid"`
		Handle                *struct {
			Address string `json:"address"`
		} `json:"handle"`
		Chats []struct {
			GUID string `json:"guid"`
		} `json:"chats"`
	} `json:"data"`
}

// objectReplacement is the character Messages puts in a message's text where
// an attachment sits.
const objectReplacement = "￼"

// Messages maps a new-message event of a one-to-one chat. The account's own
// messages (isFromMe), reactions (they name an associated message), group
// chats (GUIDs holding ";+;") and every other event type carry nothing.
// Attachments are not forwarded: BlueBubbles serves them only behind the
// server password, never at a URL uma could be given.
func (b *BlueBubbles) Messages(body []byte) ([]Inbound, error) {
	var e bbEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, err
	}
	d := e.Data
	switch {
	case e.Type != "new-message", d.IsFromMe, d.AssociatedMessageGUID != "", d.Handle == nil,
		len(d.Chats) == 0 || !strings.Contains(d.Chats[0].GUID, ";-;"):
		return nil, nil
	}
	text := strings.TrimSpace(strings.ReplaceAll(d.Text, objectReplacement, ""))
	if text == "" || d.GUID == "" || d.Handle.Address == "" {
		return nil, nil
	}
	return []Inbound{{From: d.Handle.Address, To: b.o.Account, Body: text, MessageID: d.GUID}}, nil
}

var _ Backend = (*BlueBubbles)(nil)
