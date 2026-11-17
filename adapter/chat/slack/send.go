package slack

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Schemas of what the adapter sends.
const (
	// BlocksSchema is a message's Block Kit blocks: the data is the JSON
	// array chat.postMessage takes as blocks
	// (https://docs.slack.dev/reference/block-kit/blocks/). The message's
	// text parts travel as the fallback text. A button or select in the
	// blocks comes back as an interaction.
	BlocksSchema  = "slack.chat.blocks"
	BlocksVersion = 1
)

// MaxTextRunes is how much text one message may carry: "Slack will
// truncate messages containing more than 40,000 characters"
// (https://docs.slack.dev/reference/methods/chat.postMessage/). The
// adapter refuses such text instead of letting Slack cut it.
const MaxTextRunes = 40000

// MetadataEventType is the event_type of the message metadata that names
// the attempt of a message the adapter sends or edits. Slack documents that
// an app registers its metadata schemas in its manifest before sending
// metadata (https://docs.slack.dev/messaging/message-metadata/); a message
// whose metadata Slack refuses is sent without it, and reconciliation then
// finds it by its text.
const MetadataEventType = "uma_attempt"

type metadata struct {
	EventType    string            `json:"event_type"`
	EventPayload map[string]string `json:"event_payload"`
}

func attemptMetadata(e adapter.Effect) *metadata {
	return &metadata{EventType: MetadataEventType, EventPayload: map[string]string{
		"operation": string(e.Operation.ID), "attempt": string(e.Attempt),
	}}
}

// rendered is what Slack is sent for a message's content.
type rendered struct {
	text   string
	blocks json.RawMessage
}

var controlCharacters = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// render maps content to Slack's text and blocks. Text parts are literal
// text: Slack treats "&", "<" and ">" as control characters and "only the
// specific characters shown above will be decoded"
// (https://docs.slack.dev/messaging/formatting-message-text/), so they go
// as entities, with mrkdwn off. Formatting and mentions travel as blocks.
// Anything else is unsupported, and nothing is cut.
func render(c model.Content) (rendered, *model.Error) {
	var out rendered
	var texts []string
	for _, p := range c.Parts {
		switch {
		case p.Kind == model.PartText:
			texts = append(texts, controlCharacters.Replace(p.Text))
		case p.Kind == model.PartStructured && p.Structured != nil && p.Structured.Name == BlocksSchema && p.Structured.Version == BlocksVersion:
			if out.blocks != nil {
				return rendered{}, final(model.Unsupported, "a Slack message carries one list of blocks")
			}
			var blocks []json.RawMessage
			if json.Unmarshal(p.Structured.Data, &blocks) != nil || len(blocks) == 0 {
				return rendered{}, final(model.Validation, BlocksSchema+" must hold a non-empty JSON array of blocks")
			}
			out.blocks = slices.Clone(p.Structured.Data)
		default:
			return rendered{}, final(model.Unsupported, "Slack messages carry text and blocks only")
		}
	}
	out.text = strings.Join(texts, "\n\n")
	if out.text == "" && out.blocks == nil {
		return rendered{}, final(model.Validation, "content has no text or blocks")
	}
	if utf8.RuneCountInString(out.text) > MaxTextRunes {
		return rendered{}, final(model.Validation, "text exceeds Slack's limit of 40000 characters")
	}
	return out, nil
}

type postArgs struct {
	Channel  string          `json:"channel"`
	Text     string          `json:"text,omitempty"`
	Blocks   json.RawMessage `json:"blocks,omitempty"`
	ThreadTS string          `json:"thread_ts,omitempty"`
	Mrkdwn   bool            `json:"mrkdwn"`
	Metadata *metadata       `json:"metadata,omitempty"`
}

type postReply struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// conversation resolves the conversation of an effect's target: the one it
// names, or, for a user, the direct message conversations.open opens or
// resumes (https://docs.slack.dev/reference/methods/conversations.open/).
// A failure is the effect's outcome and error.
func (x *Instance) conversation(ctx context.Context, t target) (string, adapter.Outcome, error) {
	if t.user == "" {
		return t.conversation, adapter.Outcome{}, nil
	}
	var res struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := x.call(ctx, "conversations.open", map[string]any{"users": t.user}, &res); err != nil {
		out, ferr := failure(ctx, err, openCall)
		return "", out, ferr
	}
	if !slackID.MatchString(res.Channel.ID) {
		return "", adapter.Outcome{}, model.NewError(model.Transient, "Slack opened no conversation")
	}
	return res.Channel.ID, adapter.Outcome{}, nil
}

// Send posts content to the conversation the route names: a room or a
// direct message (a user route, through conversations.open), as a reply in
// a thread when the route is a thread or the send answers a message.
//
// A reply that names the message it answers goes into that message's
// thread: its parent, when the answered message is itself a reply
// ("Avoid using a reply's ts value; use its parent instead"), else the
// message. Only public audiences are sent: a conversation's members read
// a message.
func (x *Instance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	t, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return rejected(model.Unsupported, "Slack messages are public to the conversation"), nil
	}
	msg, rerr := render(r.Content)
	if rerr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: rerr}, nil
	}
	conv, out, err := x.conversation(ctx, t)
	if err != nil || out.State != "" {
		return out, err
	}
	args := postArgs{Channel: conv, Text: msg.text, Blocks: msg.blocks, ThreadTS: threadFor(t, conv, r), Metadata: x.marker(r.Effect)}
	var reply postReply
	if err := x.postMessage(ctx, &args, &reply); err != nil {
		return failure(ctx, err, writeCall)
	}
	if !slackID.MatchString(reply.Channel) || !tsPattern.MatchString(reply.TS) {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Slack answer names no message; reconcile before retrying")
	}
	return adapter.Outcome{
		State:      model.ProviderAccepted,
		References: []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, messageID(reply.Channel, reply.TS))},
	}, nil
}

// marker is the attempt metadata of an effect, nil once Slack refused it.
func (x *Instance) marker(e adapter.Effect) *metadata {
	if x.noMetadata.Load() {
		return nil
	}
	return attemptMetadata(e)
}

// postMessage calls chat.postMessage. Metadata Slack refuses is dropped and
// the post repeated: a refusal applied nothing.
func (x *Instance) postMessage(ctx context.Context, args *postArgs, reply *postReply) error {
	err := x.call(ctx, "chat.postMessage", args, reply)
	if args.Metadata != nil && x.metadataRefused(err) {
		args.Metadata = nil
		err = x.call(ctx, "chat.postMessage", args, reply)
	}
	return err
}

// metadataRefused reports whether err is Slack refusing the metadata of a
// call, and remembers a refusal that will repeat: the app registered no
// schema, or may not send metadata with this token.
func (x *Instance) metadataRefused(err error) bool {
	ae, ok := errors.AsType[*apiError](err)
	if !ok {
		return false
	}
	switch ae.code {
	case "invalid_metadata_schema", "metadata_must_be_sent_from_app":
		x.noMetadata.Store(true)
		return true
	case "invalid_metadata_format", "metadata_too_large":
		return true
	}
	return false
}

// threadFor is the ts of the thread a send lands in, "" for none.
func threadFor(t target, conversation string, r adapter.SendRequest) string {
	if t.thread != "" {
		return t.thread
	}
	if r.Target == nil || r.Target.Namespace != model.ProviderMessageNamespace || r.Target.Kind != model.RefKindMessage {
		return ""
	}
	c, ts, ok := parseMessageID(r.Target.ID)
	if !ok || c != conversation {
		return ""
	}
	if r.Thread != nil {
		for _, m := range r.Thread.Metadata {
			if m.Name != PayloadSchema {
				continue
			}
			var p struct {
				ThreadTS string `json:"thread_ts"`
			}
			if json.Unmarshal(m.Data, &p) == nil && tsPattern.MatchString(p.ThreadTS) {
				return p.ThreadTS
			}
		}
	}
	return ts
}

var _ adapter.Sender = (*Instance)(nil)
