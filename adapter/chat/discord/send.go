package discord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Schemas of what the adapter sends.
const (
	// ComponentsSchema is a message's components: the data is the JSON
	// array Create Message takes as components (action rows of buttons and
	// selects, https://docs.discord.com/developers/components/reference).
	// A button or select in it comes back as an interaction.
	ComponentsSchema  = "discord.chat.components"
	ComponentsVersion = 1
)

// MaxTextRunes is how much text one message may carry: content "up to 2000
// characters" (https://docs.discord.com/developers/resources/message). The
// adapter refuses longer text instead of cutting it.
const MaxTextRunes = 2000

// rendered is what Discord is sent for a message's content.
type rendered struct {
	text       string
	components json.RawMessage
}

// render maps content to Discord's content and components. Text parts are
// joined by a blank line and sent as written: Discord's markdown applies,
// while allowed_mentions stops any mention from notifying anyone.
// Components travel as one structured part; anything else is unsupported,
// and nothing is cut.
func render(c model.Content) (rendered, *model.Error) {
	var out rendered
	var texts []string
	for _, p := range c.Parts {
		switch {
		case p.Kind == model.PartText:
			texts = append(texts, p.Text)
		case p.Kind == model.PartStructured && p.Structured != nil && p.Structured.Name == ComponentsSchema && p.Structured.Version == ComponentsVersion:
			if out.components != nil {
				return rendered{}, final(model.Unsupported, "a Discord message carries one list of components")
			}
			var rows []json.RawMessage
			if json.Unmarshal(p.Structured.Data, &rows) != nil || len(rows) == 0 {
				return rendered{}, final(model.Validation, ComponentsSchema+" must hold a non-empty JSON array of components")
			}
			out.components = slices.Clone(p.Structured.Data)
		default:
			return rendered{}, final(model.Unsupported, "Discord messages carry text and components only")
		}
	}
	out.text = strings.Join(texts, "\n\n")
	if strings.TrimSpace(out.text) == "" && out.components == nil {
		return rendered{}, final(model.Validation, "content has no text or components")
	}
	if utf8.RuneCountInString(out.text) > MaxTextRunes {
		return rendered{}, final(model.Validation, "text exceeds Discord's limit of 2000 characters")
	}
	return out, nil
}

// noMentions is the allowed_mentions of everything the adapter sends:
// "parse": [] suppresses every mention, so text never pings anyone.
var noMentions = map[string]any{"parse": []string{}}

// body is the JSON of a message rendered, with mentions suppressed.
func (m rendered) body() map[string]any {
	b := map[string]any{"content": m.text, "allowed_mentions": noMentions}
	if m.components != nil {
		b["components"] = m.components
	}
	return b
}

// operationNonce is the nonce of every send of an operation: 25
// characters at most ("nonce ... max 25 characters"). With enforce_nonce,
// "If another message was created by the same author with the same nonce,
// that message will be returned and no new message will be created": a
// second attempt of the operation inside Discord's few minutes is the
// first one's message.
func operationNonce(e adapter.Effect) string {
	sum := sha256.Sum256([]byte(string(e.Operation.Tenant) + "\x00" + string(e.Operation.ID)))
	return "u" + hex.EncodeToString(sum[:12])
}

// channelOf resolves the channel of an effect's target: the one it names,
// or, for a user, the direct message channel Create DM opens or returns
// ("if one already exists, it will be returned instead",
// https://docs.discord.com/developers/resources/user). A failure is the
// effect's outcome and error.
func (x *Instance) channelOf(ctx context.Context, t target) (string, adapter.Outcome, error) {
	if t.user == "" {
		return t.channel, adapter.Outcome{}, nil
	}
	var ch struct {
		ID string `json:"id"`
	}
	if err := x.call(ctx, http.MethodPost, "users/@me/channels", map[string]any{"recipient_id": t.user}, &ch); err != nil {
		out, ferr := failure(ctx, err, openCall)
		return "", out, ferr
	}
	if !isSnowflake(ch.ID) {
		return "", adapter.Outcome{}, model.NewError(model.Transient, "Discord opened no direct message channel")
	}
	x.learnChannel(ch.ID, 1, "")
	return ch.ID, adapter.Outcome{}, nil
}

// Send posts content to the channel the route names: a room, a thread, or
// a user's direct message channel. A send that answers a message of the
// same channel replies to it (message_reference, not failing when the
// message is gone). Only public audiences are sent: a channel's members
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
		return rejected(model.Unsupported, "Discord messages are public to the channel; an ephemeral one answers an interaction"), nil
	}
	msg, rerr := render(r.Content)
	if rerr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: rerr}, nil
	}
	channel, out, err := x.channelOf(ctx, t)
	if err != nil || out.State != "" {
		return out, err
	}
	body := msg.body()
	body["nonce"], body["enforce_nonce"] = operationNonce(r.Effect), true
	if ref := replyTo(channel, r.Target); ref != "" {
		body["message_reference"] = map[string]any{"message_id": ref, "channel_id": channel, "fail_if_not_exists": false}
	}
	var m message
	if err := x.call(ctx, http.MethodPost, "channels/"+channel+"/messages", body, &m); err != nil {
		return failure(ctx, err, writeCall)
	}
	if !isSnowflake(m.ID) {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Discord answer names no message; reconcile before retrying")
	}
	return adapter.Outcome{
		State:      model.ProviderAccepted,
		References: []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, messageID(channel, m.ID))},
	}, nil
}

// replyTo is the message a send in channel answers: its target, when that
// is a provider message of the same channel.
func replyTo(channel string, target *model.ExternalRef) string {
	if target == nil || target.Namespace != model.ProviderMessageNamespace || target.Kind != model.RefKindMessage {
		return ""
	}
	c, m, ok := parseMessageID(target.ID)
	if !ok || c != channel {
		return ""
	}
	return m
}

var _ adapter.Sender = (*Instance)(nil)
