package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Slack's windows for an interaction
// (https://docs.slack.dev/interactivity/handling-user-interaction/): the
// acknowledgement "must be sent within 3 seconds of receiving the
// payload", and a response_url takes "up to 5 times within 30 minutes of
// receiving the payload".
const (
	AckDeadline    = 3 * time.Second
	ResponseWindow = 30 * time.Minute
)

// ViewSchemaPrefix starts the schema name of a modal submission's fields; the
// callback_id of the view follows it. Fields is version 1: a JSON object
// holding callback_id, view_id, private_metadata (when set) and values, the
// view's state as Slack sent it.
const (
	ViewSchemaPrefix  = "slack.chat.view"
	ViewSchemaVersion = 1
)

// interaction is a signed interaction payload
// (https://docs.slack.dev/reference/interaction-payloads/): the fields of
// block_actions and view_submission the adapter reads.
type interaction struct {
	Type      string `json:"type"`
	APIAppID  string `json:"api_app_id"`
	TriggerID string `json:"trigger_id"`
	// A block_suggestion names the menu asking and the text typed so far.
	ActionID    string `json:"action_id"`
	BlockID     string `json:"block_id"`
	Value       string `json:"value"`
	ResponseURL string `json:"response_url"`
	Team        *struct {
		ID string `json:"id"`
	} `json:"team"`
	User *struct {
		ID     string `json:"id"`
		TeamID string `json:"team_id"`
	} `json:"user"`
	Channel *struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container *struct {
		MessageTS   string `json:"message_ts"`
		ChannelID   string `json:"channel_id"`
		IsEphemeral bool   `json:"is_ephemeral"`
	} `json:"container"`
	Message *struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	Actions []struct {
		ActionID       string `json:"action_id"`
		ActionTS       string `json:"action_ts"`
		Value          string `json:"value"`
		SelectedOption *struct {
			Value string `json:"value"`
		} `json:"selected_option"`
	} `json:"actions"`
	View *struct {
		ID              string `json:"id"`
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
		State           struct {
			Values json.RawMessage `json:"values"`
		} `json:"state"`
		ResponseURLs []struct {
			ChannelID   string `json:"channel_id"`
			ResponseURL string `json:"response_url"`
		} `json:"response_urls"`
	} `json:"view"`
}

// form answers a form request: an interaction (a payload field) or a slash
// command (a command field), arrived at time at.
func (x *Instance) form(ctx context.Context, sink adapter.Sink, body []byte, at time.Time) verdict {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return statusAnswer(http.StatusBadRequest)
	}
	switch {
	case form.Get("payload") != "":
		return x.interaction(ctx, sink, []byte(form.Get("payload")), at, body)
	case form.Get("command") != "":
		return x.command(ctx, sink, form, at)
	}
	return statusAnswer(http.StatusBadRequest)
}

// interaction answers an interaction payload, the JSON of a request's payload
// field or of a Socket Mode envelope, arrived at time at. raw is what a
// payload without a trigger_id is identified by (idOr). The sink has recorded
// it before the answer: an empty 200, which Slack takes as the
// acknowledgement and, for a view, shows nothing for ("Your HTTP 200
// response must be empty", https://docs.slack.dev/surfaces/modals/).
func (x *Instance) interaction(ctx context.Context, sink adapter.Sink, payload []byte, at time.Time, raw []byte) verdict {
	var p interaction
	if json.Unmarshal(payload, &p) != nil {
		return statusAnswer(http.StatusBadRequest)
	}
	if p.Type == "block_suggestion" {
		return x.suggestion(ctx, sink, p, at, raw)
	}
	d, err := x.fromPayload(p, at, raw)
	switch {
	case err != nil:
		return statusAnswer(http.StatusBadRequest)
	case d == nil:
		return emptyOK()
	}
	if in := d.ingress.Envelope.Interaction; in != nil && in.Kind == model.InteractionSubmission && x.cfg.EvaluateInteractions {
		if a, record := x.evaluateSubmission(ctx, sink, *in); !record {
			return a
		}
	}
	return x.deliver(ctx, sink, []delivery{*d}, emptyOK())
}

// command answers a slash command, the fields of a request's form or of a
// Socket Mode envelope's payload, arrived at time at, recorded before the
// empty 200 that acknowledges it.
func (x *Instance) command(ctx context.Context, sink adapter.Sink, f url.Values, at time.Time) verdict {
	d, err := x.fromCommand(f, at)
	switch {
	case err != nil:
		return statusAnswer(http.StatusBadRequest)
	case d == nil:
		return emptyOK()
	}
	return x.deliver(ctx, sink, []delivery{*d}, emptyOK())
}

// requestTime is the time the verified request carries; the host's clock
// when it carries none the adapter can read.
func requestTime(r *http.Request, fallback time.Time) time.Time {
	if t := slackTime(r.Header.Get(HeaderTimestamp) + ".000000"); !t.IsZero() {
		return t
	}
	return fallback
}

// ours reports whether an interaction names this channel's team and app.
// An org-installed app's payload carries no team: the user's team stands in
// for it, and none at all is not evidence of another team.
func (x *Instance) ours(team, userTeam, app string) bool {
	if team == "" {
		team = userTeam
	}
	return (team == "" || team == x.cfg.TeamID) && (x.cfg.AppID == "" || app == "" || app == x.cfg.AppID)
}

func (x *Instance) fromPayload(p interaction, at time.Time, body []byte) (*delivery, error) {
	var team string
	if p.Team != nil {
		team = p.Team.ID
	}
	switch p.Type {
	case "block_actions", "view_submission":
	default:
		// view_closed, shortcuts and message actions: not consumed. An
		// option request (block_suggestion) never gets here: it is answered
		// by serveSuggestion and recorded as nothing.
		return nil, nil
	}
	if p.User == nil || !slackID.MatchString(p.User.ID) {
		return nil, malformed("interaction user")
	}
	if !x.ours(team, p.User.TeamID, p.APIAppID) {
		return nil, nil
	}
	actor := x.userRef(p.User.ID)
	in := model.InteractionInput{Actor: actor, Deadline: at.Add(AckDeadline)}
	occurred := at
	var conv, thread string
	switch p.Type {
	case "block_actions":
		if len(p.Actions) == 0 {
			return nil, nil
		}
		a := p.Actions[0]
		if a.ActionID == "" {
			return nil, malformed("action_id")
		}
		in.Kind, in.Action, in.Arguments = model.InteractionAction, a.ActionID, a.Value
		if in.Arguments == "" && a.SelectedOption != nil {
			in.Arguments = a.SelectedOption.Value
		}
		in.ID = "action:" + idOr(p.TriggerID, body)
		if p.Channel != nil {
			conv = p.Channel.ID
		}
		if p.Container != nil && conv == "" {
			conv = p.Container.ChannelID
		}
		var messageTS string
		if p.Message != nil {
			messageTS, thread = p.Message.TS, p.Message.ThreadTS
		}
		if p.Container != nil && messageTS == "" {
			messageTS = p.Container.MessageTS
		}
		if ts := slackTime(a.ActionTS); !ts.IsZero() {
			occurred = ts
		}
		if slackID.MatchString(conv) && tsPattern.MatchString(messageTS) && (p.Container == nil || !p.Container.IsEphemeral) {
			ref := model.ProviderMessageRef(x.channel.Ref, messageID(conv, messageTS))
			in.Message = &ref
		}
		in.ResponseToken = x.responseToken(p.ResponseURL)
	case "view_submission":
		if p.View == nil || p.View.ID == "" {
			return nil, malformed("view")
		}
		in.Kind = model.InteractionSubmission
		// The view and the submission's trigger_id: each submit is an
		// interaction with a trigger of its own ("Triggers may only be used
		// once"), and a payload delivered again carries the same one. The
		// reference's example omits trigger_id; Slack's Bolt types it as a
		// required field of view_submission. Without it, the payload's
		// digest stands in, as for an action.
		in.ID = "view:" + p.View.ID + ":" + idOr(p.TriggerID, body)
		in.Fields = viewFields(p)
		for _, u := range p.View.ResponseURLs {
			if in.ResponseToken = x.responseToken(u.ResponseURL); in.ResponseToken != "" {
				conv = u.ChannelID
				break
			}
		}
	}
	if in.ResponseToken != "" {
		in.ResponseExpiresAt = at.Add(ResponseWindow)
	}
	return x.interactionDelivery(in, conv, thread, occurred), nil
}

func (x *Instance) fromCommand(f url.Values, at time.Time) (*delivery, error) {
	if !x.ours(f.Get("team_id"), "", f.Get("api_app_id")) {
		return nil, nil
	}
	name := strings.TrimPrefix(f.Get("command"), "/")
	user := f.Get("user_id")
	if name == "" || !slackID.MatchString(user) {
		return nil, malformed("command or user")
	}
	in := model.InteractionInput{
		ID: "command:" + idOr(f.Get("trigger_id"), []byte(f.Encode())), Kind: model.InteractionCommand,
		Command: name, Arguments: f.Get("text"), Actor: x.userRef(user), Deadline: at.Add(AckDeadline),
		ResponseToken: x.responseToken(f.Get("response_url")),
	}
	if in.ResponseToken != "" {
		in.ResponseExpiresAt = at.Add(ResponseWindow)
	}
	return x.interactionDelivery(in, f.Get("channel_id"), "", at), nil
}

// interactionDelivery wraps in in its envelope: the interaction ID is the
// event ID, and the room and thread scope it as they scope a message.
func (x *Instance) interactionDelivery(in model.InteractionInput, conv, thread string, at time.Time) *delivery {
	in.Bot = ptr(x.userRef(x.cfg.BotUserID))
	env := model.Envelope{
		EventID: in.ID, Destination: x.cfg.TeamID, Kind: model.EnvelopeInteraction, Sender: in.Actor, Assurance: "verified",
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: at.UTC(), Interaction: &in,
	}
	if slackID.MatchString(conv) {
		ts := ""
		if in.Message != nil {
			_, ts, _ = parseMessageID(in.Message.ID)
		}
		env.References = x.references(conv, thread, ts)
		in.Context = append(in.Context, env.References...)
	}
	return ptr(delivery{ingress: x.ingress(env)})
}

// idOr is id, or, for a payload without one, a digest of its body: a
// redelivery of the same bytes is the same interaction.
func idOr(id string, body []byte) string {
	if id != "" {
		return id
	}
	sum := sha256.Sum256(body)
	return "sha256-" + hex.EncodeToString(sum[:16])
}

// responseToken is what the adapter keeps of a response_url: its path, as
// "up to 5 times within 30 minutes" a credential. The origin is a host
// setting (Options.ResponseBaseURL), so a payload can never point an answer
// anywhere else; a URL of another origin, or of an odd shape, is not kept
// and the interaction has no token.
func (x *Instance) responseToken(raw string) string {
	u, err := url.Parse(raw)
	base, berr := url.Parse(x.responseBase())
	if err != nil || berr != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	tok := strings.TrimPrefix(u.Path, "/")
	if !validToken(tok) {
		return ""
	}
	return tok
}

// tokenPattern is the shape of a response URL path: path segments of
// letters, digits and a few separators.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)*$`)

func validToken(t string) bool {
	if len(t) == 0 || len(t) > 1024 || !tokenPattern.MatchString(t) {
		return false
	}
	for seg := range strings.SplitSeq(t, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func (x *Instance) responseBase() string {
	if x.opts.ResponseBaseURL != "" {
		return strings.TrimRight(x.opts.ResponseBaseURL, "/")
	}
	return DefaultResponseBaseURL
}

// viewFields is the Fields of a modal submission, under a schema named
// after the view's callback_id so a bot routes on it.
func viewFields(p interaction) model.Extension {
	b, _ := json.Marshal(viewData(p))
	return model.Extension{Name: viewSchema(p.View.CallbackID), Version: ViewSchemaVersion, Data: b}
}

// viewData is the data of a view's fields: its identity, private metadata
// and state.
func viewData(p interaction) map[string]any {
	data := map[string]any{"view_id": p.View.ID, "callback_id": p.View.CallbackID}
	if p.View.PrivateMetadata != "" {
		data["private_metadata"] = p.View.PrivateMetadata
	}
	vals := p.View.State.Values
	if len(vals) == 0 {
		vals = json.RawMessage(`{}`)
	}
	data["values"] = vals
	return data
}

var versionSegment = regexp.MustCompile(`^v[0-9]+$`)

// viewSchema is the schema name of a view: ViewSchemaPrefix and a segment
// made of the callback_id, lowercased, with anything outside letters,
// digits, "_" and "-" as "_", trimmed to start and end alphanumeric and to
// 48 bytes. A callback with nothing usable gives the prefix alone, and a
// segment that reads as a version is prefixed ("cb-"): registry names must
// not end in one.
func viewSchema(callback string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(callback) {
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
		return ViewSchemaPrefix
	}
	if versionSegment.MatchString(seg) {
		seg = "cb-" + seg
	}
	return ViewSchemaPrefix + "." + seg
}
