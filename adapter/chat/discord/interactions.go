package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Discord's windows for an interaction ("Receiving and Responding",
// https://docs.discord.com/developers/interactions/receiving-and-responding):
// "you must send an initial response within 3 seconds of receiving the
// event", and "Interaction tokens are valid for 15 minutes".
const (
	AckDeadline    = 3 * time.Second
	ResponseWindow = 15 * time.Minute
)

// Interaction types ("Interaction Type").
const (
	interactionPing         = 1
	interactionCommand      = 2
	interactionComponent    = 3
	interactionAutocomplete = 4
	interactionModalSubmit  = 5
)

// Interaction callback types ("Interaction Callback Type").
const (
	callbackPong                  = 1
	callbackDeferredMessage       = 5
	callbackDeferredUpdateMessage = 6
	callbackAutocompleteResult    = 8
)

// Message flags.
const (
	flagEphemeral = 1 << 6
	flagLoading   = 1 << 7
)

// ModalSchemaPrefix starts the schema name of a modal submission's fields;
// a segment made of the modal's custom_id follows it. Fields is version 1:
// a JSON object holding custom_id and values, each submitted component's
// value (or values) by its custom_id.
const (
	ModalSchemaPrefix  = "discord.chat.modal"
	ModalSchemaVersion = 1
)

// option is an application command option as an interaction reports it.
type option struct {
	Name    string          `json:"name"`
	Type    int             `json:"type"`
	Value   json.RawMessage `json:"value"`
	Options []option        `json:"options"`
}

// interaction is an interaction object
// (https://docs.discord.com/developers/interactions/receiving-and-responding).
type interaction struct {
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	Type          int    `json:"type"`
	Token         string `json:"token"`
	GuildID       string `json:"guild_id"`
	ChannelID     string `json:"channel_id"`
	Channel       *struct {
		ID       string `json:"id"`
		Type     int    `json:"type"`
		ParentID string `json:"parent_id"`
	} `json:"channel"`
	Member *struct {
		User *user `json:"user"`
	} `json:"member"`
	User    *user    `json:"user"`
	Message *message `json:"message"`
	Data    *struct {
		Name       string          `json:"name"`
		Type       int             `json:"type"`
		Options    []option        `json:"options"`
		TargetID   string          `json:"target_id"`
		CustomID   string          `json:"custom_id"`
		Values     []string        `json:"values"`
		Components json.RawMessage `json:"components"`
	} `json:"data"`
}

// actor is the user who interacted: the member's user in a guild, the user
// elsewhere.
func (p interaction) actor() *user {
	if p.Member != nil && p.Member.User != nil {
		return p.Member.User
	}
	return p.User
}

// deferType is the callback that acknowledges an interaction of kind, with
// or without a message the control sits on: an update of that message
// later (DEFERRED_UPDATE_MESSAGE) for a component, or a modal opened from
// one, else a response message later (DEFERRED_CHANNEL_MESSAGE_WITH_SOURCE),
// shown to everyone as loading until it is answered. Respond reads the same
// rule from the stored interaction.
func deferType(kind string, onMessage bool) int {
	if onMessage && (kind == model.InteractionAction || kind == model.InteractionSubmission) {
		return callbackDeferredUpdateMessage
	}
	return callbackDeferredMessage
}

type malformed string

func (m malformed) Error() string { return "discord: malformed interaction: " + string(m) }

// fromInteraction maps an interaction arrived at time at to its delivery.
// A nil delivery is an interaction the adapter does not record.
func (x *Instance) fromInteraction(p interaction, at time.Time) (*delivery, error) {
	if !isSnowflake(p.ID) || p.ApplicationID != x.cfg.ApplicationID {
		return nil, malformed("id or application")
	}
	a := p.actor()
	if a == nil || !isSnowflake(a.ID) || p.Data == nil {
		return nil, malformed("user or data")
	}
	in := model.InteractionInput{ID: "interaction:" + p.ID, Actor: x.userRef(a.ID), Deadline: at.Add(AckDeadline)}
	switch p.Type {
	case interactionCommand:
		in.Kind = model.InteractionCommand
		in.Command, in.Arguments = commandOf(p)
		if in.Command == "" {
			return nil, malformed("command name")
		}
	case interactionComponent:
		if p.Data.CustomID == "" {
			return nil, malformed("custom_id")
		}
		in.Kind, in.Action, in.Arguments = model.InteractionAction, p.Data.CustomID, strings.Join(p.Data.Values, "\n")
	case interactionModalSubmit:
		in.Kind, in.Fields = model.InteractionSubmission, modalFields(p.Data.CustomID, p.Data.Components)
	default:
		return nil, nil
	}
	channel, parent := p.ChannelID, ""
	if c := p.Channel; c != nil && isSnowflake(c.ID) {
		channel = c.ID
		x.learnChannel(c.ID, c.Type, c.ParentID)
		if isThreadType(c.Type) && isSnowflake(c.ParentID) {
			parent = c.ParentID
		}
	}
	if m := p.Message; m != nil && isSnowflake(m.ID) && isSnowflake(m.ChannelID) && m.Flags&flagEphemeral == 0 {
		ref := model.ProviderMessageRef(x.channel.Ref, messageID(m.ChannelID, m.ID))
		in.Message = &ref
	}
	if validToken(p.Token) {
		in.ResponseToken, in.ResponseExpiresAt = p.Token, at.Add(ResponseWindow)
	}
	in.Bot = ptr(x.userRef(x.cfg.BotUserID))
	env := model.Envelope{
		EventID: in.ID, Destination: x.cfg.ApplicationID, Kind: model.EnvelopeInteraction, Sender: in.Actor, Assurance: "verified",
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: at.UTC(),
	}
	if isSnowflake(channel) {
		env.References = x.references(channel, parent)
		in.Context = slices.Clone(env.References)
	}
	env.Interaction = &in
	return &delivery{ingress: ingress(env)}, nil
}

// commandOf is a command's name, with its subcommand group and subcommand
// (option types 2 and 1) as further words, and its arguments: the leaf
// options as a JSON object of name to value, or a context menu command's
// target ID.
func commandOf(p interaction) (string, string) {
	name := p.Data.Name
	opts := p.Data.Options
	for len(opts) == 1 && (opts[0].Type == 1 || opts[0].Type == 2) {
		name += " " + opts[0].Name
		opts = opts[0].Options
	}
	if p.Data.TargetID != "" {
		return name, p.Data.TargetID
	}
	if len(opts) == 0 {
		return name, ""
	}
	args := map[string]json.RawMessage{}
	for _, o := range opts {
		args[o.Name] = o.Value
	}
	b, _ := json.Marshal(args)
	return name, string(b)
}

// modalFields is the Fields of a modal submission, named after its
// custom_id: every component under components that carries a custom_id and
// a value or values.
func modalFields(customID string, components json.RawMessage) model.Extension {
	values := map[string]any{}
	var walk func(json.RawMessage)
	walk = func(raw json.RawMessage) {
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) == nil {
			for _, e := range arr {
				walk(e)
			}
			return
		}
		var c struct {
			CustomID   string          `json:"custom_id"`
			Value      *string         `json:"value"`
			Values     []string        `json:"values"`
			Component  json.RawMessage `json:"component"`
			Components json.RawMessage `json:"components"`
		}
		if json.Unmarshal(raw, &c) != nil {
			return
		}
		switch {
		case c.CustomID != "" && c.Value != nil:
			values[c.CustomID] = *c.Value
		case c.CustomID != "" && c.Values != nil:
			values[c.CustomID] = c.Values
		}
		if len(c.Component) > 0 {
			walk(c.Component)
		}
		if len(c.Components) > 0 {
			walk(c.Components)
		}
	}
	walk(components)
	b, _ := json.Marshal(map[string]any{"custom_id": customID, "values": values})
	return model.Extension{Name: modalSchema(customID), Version: ModalSchemaVersion, Data: b}
}

var versionSegment = regexp.MustCompile(`^v[0-9]+$`)

// modalSchema is the schema name of a modal: ModalSchemaPrefix and a
// segment made of the custom_id, lowercased, anything outside letters,
// digits, "_" and "-" as "_", trimmed to start and end alphanumeric and to
// 48 bytes. Nothing usable gives the prefix alone, and a segment that reads
// as a version is prefixed ("id-"): registry names must not end in one.
func modalSchema(customID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(customID) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	seg := b.String()
	if len(seg) > 48 {
		seg = seg[:48]
	}
	seg = strings.Trim(seg, "_-")
	if seg == "" {
		return ModalSchemaPrefix
	}
	if versionSegment.MatchString(seg) {
		seg = "id-" + seg
	}
	return ModalSchemaPrefix + "." + seg
}

// tokenPattern is the shape of an interaction token: letters, digits and a
// few separators, as it goes into a URL path unescaped.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

func validToken(t string) bool { return len(t) <= 1024 && tokenPattern.MatchString(t) }

// interactionAnswer records the interaction p, arrived at time at, and
// returns the callback that acknowledges it: a deferral once recorded, the
// empty choices of an autocomplete request (nothing recorded), or the
// sink's refusal.
func (x *Instance) interactionAnswer(ctx context.Context, sink adapter.Sink, p interaction, at time.Time, cursor string) (int, json.RawMessage, error) {
	if p.Type == interactionAutocomplete {
		return callbackAutocompleteResult, json.RawMessage(`{"choices":[]}`), nil
	}
	d, err := x.fromInteraction(p, at)
	if err != nil {
		return 0, nil, err
	}
	if d == nil {
		return 0, nil, malformed("type")
	}
	if err := deliver(ctx, sink, []delivery{*d}, cursor); err != nil {
		return 0, nil, err
	}
	in := d.ingress.Envelope.Interaction
	return deferType(in.Kind, in.Message != nil), nil, nil
}

// gatewayInteraction records an INTERACTION_CREATE and acknowledges it with
// the callback endpoint: "If you are receiving Interactions over the
// gateway, you have to respond via HTTP". A callback that fails leaves the
// record; the response then finds the interaction gone.
func (x *Instance) gatewayInteraction(ctx context.Context, sink adapter.Sink, data json.RawMessage, cursor string) error {
	var p interaction
	if json.Unmarshal(data, &p) != nil || p.Type == interactionPing {
		return nil
	}
	typ, cbData, err := x.interactionAnswer(ctx, sink, p, x.now(), cursor)
	if _, bad := err.(malformed); bad {
		return nil
	}
	if err != nil {
		return err
	}
	if !isSnowflake(p.ID) || !validToken(p.Token) {
		return nil
	}
	body := map[string]any{"type": typ}
	if cbData != nil {
		body["data"] = cbData
	}
	_ = x.do(ctx, request{method: http.MethodPost, path: "interactions/" + p.ID + "/" + p.Token + "/callback", body: body, bare: true}, nil)
	return nil
}
