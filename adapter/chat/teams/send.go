package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Schemas of what the adapter sends.
const (
	// CardSchema is an Adaptive Card: the data is the card's JSON object,
	// sent as an attachment of content type
	// application/vnd.microsoft.card.adaptive
	// (https://learn.microsoft.com/en-us/microsoftteams/platform/task-modules-and-cards/cards/cards-reference#adaptive-card).
	// An Action.Execute in it comes back as an interaction.
	CardSchema  = "microsoft.teams.card"
	CardVersion = 1
)

// AdaptiveCardType is the attachment content type of an Adaptive Card.
const AdaptiveCardType = "application/vnd.microsoft.card.adaptive"

// MaxMessageBytes is how large the activity of one message may be: "The
// agent message size limit is 100 KB ... ensure that the size of the
// message itself is within 80 KB"
// (https://learn.microsoft.com/en-us/microsoftteams/platform/bots/build-conversational-capability).
// The adapter refuses a larger one instead of letting Teams answer 413.
const MaxMessageBytes = 80 << 10

// outActivity is a message activity the adapter sends.
type outActivity struct {
	Type        string       `json:"type"`
	ID          string       `json:"id,omitempty"`
	Text        string       `json:"text,omitempty"`
	TextFormat  string       `json:"textFormat,omitempty"`
	Attachments []attachment `json:"attachments,omitempty"`
	ReplyToID   string       `json:"replyToId,omitempty"`
}

// render maps content to a message activity. A message carries text or
// one Adaptive Card, never both: Teams splits text and an attachment into
// two messages and "you do not receive the message ID in response"
// (https://learn.microsoft.com/en-us/microsoftteams/platform/bots/build-conversational-capability),
// which would leave the send without the message it made. Text parts are
// literal text, sent with textFormat plain. Anything else is unsupported,
// and nothing is cut.
func render(c model.Content) (outActivity, *model.Error) {
	out := outActivity{Type: "message"}
	var texts []string
	for _, p := range c.Parts {
		switch {
		case p.Kind == model.PartText:
			texts = append(texts, p.Text)
		case p.Kind == model.PartStructured && p.Structured != nil && p.Structured.Name == CardSchema && p.Structured.Version == CardVersion:
			if out.Attachments != nil {
				return outActivity{}, final(model.Unsupported, "a Teams message carries one card")
			}
			var card map[string]any
			if json.Unmarshal(p.Structured.Data, &card) != nil || card["type"] != "AdaptiveCard" {
				return outActivity{}, final(model.Validation, CardSchema+" must hold an Adaptive Card JSON object")
			}
			out.Attachments = []attachment{{ContentType: AdaptiveCardType, Content: bytes.Clone(p.Structured.Data)}}
		default:
			return outActivity{}, final(model.Unsupported, "Teams messages carry text or one Adaptive Card only")
		}
	}
	out.Text = strings.Join(texts, "\n\n")
	switch {
	case out.Text != "" && out.Attachments != nil:
		return outActivity{}, final(model.Unsupported, "a Teams message carries text or a card, not both")
	case out.Text == "" && out.Attachments == nil:
		return outActivity{}, final(model.Validation, "content has no text or card")
	case out.Text != "":
		out.TextFormat = "plain"
	}
	if b, _ := json.Marshal(out); len(b) > MaxMessageBytes {
		return outActivity{}, final(model.Validation, "message exceeds Teams' limit of 80 KB")
	}
	return out, nil
}

// conversation resolves the conversation ID a post to t goes to: the one
// it names, or, for a user, the personal chat the Connector creates or
// returns (POST /v3/conversations). Teams needs the app installed for the
// user, or the user in a team where it is installed
// (https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/conversations/send-proactive-messages);
// its refusal is the effect's outcome.
func (x *Instance) conversation(ctx context.Context, t target) (string, adapter.Outcome, error) {
	if t.user == "" {
		return t.conversationID(), adapter.Outcome{}, nil
	}
	body := map[string]any{
		"bot": map[string]string{"id": x.cfg.botAccount()}, "members": []map[string]string{{"id": t.user}},
		"isGroup": false, "tenantId": x.cfg.TenantID,
		"channelData": map[string]any{"tenant": map[string]string{"id": x.cfg.TenantID}},
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := x.call(ctx, http.MethodPost, "/v3/conversations", body, &res); err != nil {
		out, ferr := failure(ctx, err, openCall)
		return "", out, ferr
	}
	if !validConversation(res.ID) {
		return "", adapter.Outcome{}, model.NewError(model.Transient, "the Bot Connector created no conversation")
	}
	return res.ID, adapter.Outcome{}, nil
}

// Send posts content to the conversation the route names: a channel (a new
// post), a channel thread (a reply in it), a group chat or a personal chat
// (a user route, through the conversation the Connector creates or
// returns). A reply that names a message of the same conversation carries
// it as replyToId. Only public audiences are sent: a conversation's members
// read a message.
func (x *Instance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	t, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return rejected(model.Unsupported, "Teams messages are public to the conversation"), nil
	}
	msg, rerr := render(r.Content)
	if rerr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: rerr}, nil
	}
	conv, out, err := x.conversation(ctx, t)
	if err != nil || out.State != "" {
		return out, err
	}
	msg.ReplyToID = replyTo(conv, r.Target)
	var res struct {
		ID string `json:"id"`
	}
	if err := x.call(ctx, http.MethodPost, conversationPath(conv), msg, &res); err != nil {
		return failure(ctx, err, writeCall)
	}
	if !validID(res.ID) {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Bot Connector answer names no message; the message may have been posted")
	}
	return adapter.Outcome{
		State:      model.ProviderAccepted,
		References: []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, messageID(conv, res.ID))},
	}, nil
}

// replyTo is the activity ID a send answering target carries as
// replyToId: a message of the same conversation, "" otherwise.
func replyTo(conversation string, target *model.ExternalRef) string {
	if target == nil || target.Namespace != model.ProviderMessageNamespace || target.Kind != model.RefKindMessage {
		return ""
	}
	c, id, ok := parseMessageID(target.ID)
	if !ok || c != baseConversation(conversation) {
		return ""
	}
	return id
}

// Signal sends a typing indicator to the conversation the route names
// (an activity of type typing). Teams shows it in personal chats and small
// group chats, not in channels, for a few seconds. A user route goes
// through the personal chat as Send does.
func (x *Instance) Signal(ctx context.Context, r adapter.SignalRequest) error {
	if err := x.ready(); err != nil {
		return err
	}
	if r.Kind != "typing" {
		return final(model.Unsupported, "Teams has no such signal")
	}
	t, err := x.parseRoute(r.Route)
	if err != nil {
		return err
	}
	conv, out, err := x.conversation(ctx, t)
	if err == nil && out.Failure != nil {
		err = out.Failure
	}
	if err != nil {
		return err
	}
	if err := x.call(ctx, http.MethodPost, conversationPath(conv), map[string]string{"type": "typing"}, nil); err != nil {
		out, ferr := failure(ctx, err, signalCall)
		if ferr == nil && out.Failure != nil {
			ferr = out.Failure
		}
		return ferr
	}
	return nil
}

var (
	_ adapter.Sender   = (*Instance)(nil)
	_ adapter.Signaler = (*Instance)(nil)
)
