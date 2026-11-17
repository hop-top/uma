package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// An external select asks the app for its options with a block_suggestion
// payload, and the HTTP response to that request carries them: "an HTTP 200
// OK response with an application/json body containing either an options
// array or an option_groups array"
// (https://docs.slack.dev/reference/block-kit/block-elements/select-menu-element/).
// The request is never recorded: it is a question, not something the user
// did. With Config.EvaluateInteractions the adapter puts it to the engine's
// interaction validator (adapter.Sink.EvaluateInteraction, kind options)
// and answers with the options of the decision; without, or when the
// decision has none, it answers an explicit empty list.

// Schemas of the options exchange. SuggestionSchema is the Fields of the
// request the validator reads, version 1: a JSON object holding action_id,
// block_id and value (the text typed so far) and, for a menu inside a modal,
// view_id, callback_id, private_metadata and values, so options can depend
// on the other fields. OptionsSchema is the Options of the decision it
// answers with, version 1: the data is the object Slack's answer holds,
// {"options": [...]} or {"option_groups": [...]}, whose option objects have
// text, value and optionally description.
const (
	SuggestionSchema        = "slack.chat.suggestion"
	SuggestionSchemaVersion = 1
	OptionsSchema           = "slack.chat.options"
	OptionsSchemaVersion    = 1
)

// Limits of an options answer: "a maximum number of 100 options", and for
// groups 100 groups of up to 100 options; text, group label and description
// "75 characters", a value 150
// (https://docs.slack.dev/reference/block-kit/composition-objects/option-object/).
const (
	MaxOptions       = 100
	MaxOptionText    = 75
	MaxOptionValue   = 150
	optionTextType   = "plain_text"
	emptyOptionsBody = `{"options":[]}`
)

type optionText struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Emoji *bool  `json:"emoji,omitempty"`
}

type option struct {
	Text        optionText  `json:"text"`
	Value       string      `json:"value"`
	Description *optionText `json:"description,omitempty"`
}

type optionGroup struct {
	Label   optionText `json:"label"`
	Options []option   `json:"options"`
}

// optionsAnswer is the body of an options answer: exactly one of its lists.
type optionsAnswer struct {
	Options      *[]option      `json:"options,omitempty"`
	OptionGroups *[]optionGroup `json:"option_groups,omitempty"`
}

// suggestion answers a block_suggestion request.
func (x *Instance) suggestion(ctx context.Context, sink adapter.Sink, p interaction, at time.Time, body []byte) verdict {
	if p.User == nil || !slackID.MatchString(p.User.ID) || p.ActionID == "" {
		return statusAnswer(http.StatusBadRequest)
	}
	var team string
	if p.Team != nil {
		team = p.Team.ID
	}
	if !x.cfg.EvaluateInteractions || !x.ours(team, p.User.TeamID, p.APIAppID) {
		return emptyOptions()
	}
	in := model.InteractionInput{
		ID: "options:" + idOr(p.TriggerID, body), Kind: model.InteractionOptions, Action: p.ActionID, Arguments: p.Value,
		Actor: x.userRef(p.User.ID), Bot: ptr(x.userRef(x.cfg.BotUserID)), Deadline: at.Add(AckDeadline), Fields: suggestionFields(p),
	}
	if p.Container != nil && slackID.MatchString(p.Container.ChannelID) {
		in.Context = []model.ExternalRef{x.roomRef(p.Container.ChannelID)}
	}
	dec, err := sink.EvaluateInteraction(ctx, in)
	if err != nil {
		return evaluationFailed(err)
	}
	if dec.Options.Name == "" {
		return emptyOptions()
	}
	options, ok := parseOptions(dec.Options)
	if !ok {
		return statusAnswer(http.StatusUnprocessableEntity)
	}
	return jsonAnswer(options)
}

func emptyOptions() verdict {
	return verdict{status: http.StatusOK, contentType: "application/json", body: []byte(emptyOptionsBody)}
}

// suggestionFields is the request the validator reads.
func suggestionFields(p interaction) model.Extension {
	data := map[string]any{"action_id": p.ActionID, "value": p.Value}
	if p.BlockID != "" {
		data["block_id"] = p.BlockID
	}
	if p.View != nil {
		for k, v := range viewData(p) {
			data[k] = v
		}
	}
	b, _ := json.Marshal(data)
	return model.Extension{Name: SuggestionSchema, Version: SuggestionSchemaVersion, Data: b}
}

// parseOptions reads a decision's options as OptionsSchema and checks them
// against Slack's limits: Slack documents no behavior for a list it cannot
// take, so none is sent. Anything the object holds beyond the fields of an
// option is refused rather than dropped, so a mistake shows.
func parseOptions(x model.Extension) (optionsAnswer, bool) {
	var a optionsAnswer
	if x.Name != OptionsSchema || x.Version != OptionsSchemaVersion {
		return a, false
	}
	dec := json.NewDecoder(bytes.NewReader(x.Data))
	dec.DisallowUnknownFields()
	if dec.Decode(&a) != nil || dec.More() || (a.Options == nil) == (a.OptionGroups == nil) {
		return optionsAnswer{}, false
	}
	switch {
	case a.Options != nil:
		if !validOptions(*a.Options) {
			return optionsAnswer{}, false
		}
	default:
		if len(*a.OptionGroups) > MaxOptions {
			return optionsAnswer{}, false
		}
		for _, g := range *a.OptionGroups {
			if !validText(g.Label) || g.Options == nil || !validOptions(g.Options) {
				return optionsAnswer{}, false
			}
		}
	}
	return a, true
}

func validOptions(opts []option) bool {
	if len(opts) > MaxOptions {
		return false
	}
	for _, o := range opts {
		n := utf8.RuneCountInString(o.Value)
		if !validText(o.Text) || n == 0 || n > MaxOptionValue || !utf8.ValidString(o.Value) {
			return false
		}
		if o.Description != nil && !validText(*o.Description) {
			return false
		}
	}
	return true
}

// validText reports whether t is a plain_text object Slack takes as an
// option's text, a group's label or a description.
func validText(t optionText) bool {
	n := utf8.RuneCountInString(t.Text)
	return t.Type == optionTextType && n > 0 && n <= MaxOptionText && utf8.ValidString(t.Text) && strings.TrimSpace(t.Text) != ""
}
