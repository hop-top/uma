package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Extension effects the adapter ships, registered from Factory.Extensions.
// Each opens or changes a modal (https://docs.slack.dev/surfaces/modals/)
// with one Web API call, and is authorized as model.ActionExtensionExecute
// on the target (a conversation, an inbox or the channel): a modal is shown
// to the person who acted, is no message, and uma records nothing of it.
//
// The data of every effect is a JSON object; fields the adapter does not
// know are ignored and never sent.
//
//   - ExtensionViewsOpen (views.open) and ExtensionViewsPush (views.push)
//     take "trigger_id", "expires_at" and "view". The trigger_id is the one
//     Slack sent with the interaction, and TriggerOf reads it, with the
//     instant it stops working, from the recorded interaction. Slack lets a
//     trigger_id "expire 3 seconds after it's sent to your app" and be used
//     once, so the effect is built for one attempt: expires_at (RFC 3339)
//     is that instant, the attempt is refused locally, with no call, once
//     it has passed, and an answer lost after the request was written is
//     never retried. A push adds a view to the stack of a modal the
//     trigger_id came from; Slack allows the app three views in all.
//   - ExtensionViewsUpdate (views.update) takes "view" and the view's
//     "view_id" or its "external_id" (exactly one), and optionally "hash",
//     Slack's guard against a stale update. It uses no trigger_id.
//
// "view" is a modal view object (type "modal"). The adapter checks what the
// reference states without I/O: type, title, close and submit as plain text
// of at most 24 characters, at most 100 blocks, the submit button an input
// block needs, callback_id and external_id of at most 255 characters,
// private_metadata of at most 3000, and an encoded size of at most 250 KiB
// (https://docs.slack.dev/reference/views/modal-views/). Block Kit is
// Slack's to judge and is sent unchanged.
const (
	ExtensionViewsOpen   = string(Type) + ".views.open"
	ExtensionViewsUpdate = string(Type) + ".views.update"
	ExtensionViewsPush   = string(Type) + ".views.push"
	// ExtensionVersion is the version of the effects.
	ExtensionVersion = 1
)

// RefKindView is the kind of the reference an opened or pushed view is
// reported under, in Namespace, by its Slack view ID.
const RefKindView = "view"

// Limits the references state for a view (see ExtensionViewsOpen).
const (
	// TriggerLifetime is how long Slack honours a trigger_id.
	TriggerLifetime = 3 * time.Second
	// MaxViewBytes is the encoded size Slack refuses a view above
	// (view_too_large, "View exceeds 250kb"), read as 250 KiB.
	MaxViewBytes = 250 << 10

	maxViewTitleChars    = 24
	maxViewButtonChars   = 24
	maxViewBlocks        = 100
	maxViewIDChars       = 255
	maxViewMetadataChars = 3000
	// triggerSkew is how far past the local clock an expires_at may be:
	// the host's clock and Slack's differ.
	triggerSkew = 5 * time.Second
)

// Extensions returns the extension effects the adapter ships, for the host
// to register with the factory.
func (f *Factory) Extensions() []adapter.ExtensionDefinition {
	def := func(name string) adapter.ExtensionDefinition {
		return adapter.ExtensionDefinition{
			Name: name, Version: ExtensionVersion, Kind: model.ExtensionKindEffect,
			InputSchema:        model.Extension{Name: name + ".input", Version: ExtensionVersion},
			RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: model.ActionExtensionExecute,
		}
	}
	return []adapter.ExtensionDefinition{def(ExtensionViewsOpen), def(ExtensionViewsUpdate), def(ExtensionViewsPush)}
}

// ValidateAction checks the fields of a view effect without I/O
// (adapter.ActionValidator): the rules Execute applies at the attempt, so
// an action Slack would refuse is a model.Validation error at the call with
// nothing stored. The instant the trigger_id stops working is judged at the
// attempt only: the validator has no clock. An action of another name or
// version is not judged here; core refuses it by its registration and
// Execute by its name.
func (f *Factory) ValidateAction(_ model.Audience, action model.Extension) []model.ValidationIssue {
	if action.Version != ExtensionVersion || !isViewAction(action.Name) {
		return nil
	}
	if _, issue := checkViewAction(action); issue != nil {
		return []model.ValidationIssue{*issue}
	}
	return nil
}

func isViewAction(name string) bool {
	return name == ExtensionViewsOpen || name == ExtensionViewsUpdate || name == ExtensionViewsPush
}

// Trigger is the trigger_id of an interaction and the instant Slack stops
// honouring it.
type Trigger struct {
	ID        string
	ExpiresAt time.Time
}

// TriggerOf reads the trigger_id that came with an interaction this adapter
// recorded, from the interaction ID it reported (see the Interactions
// section of the adapter page), and the interaction's acknowledgement
// deadline, which is three seconds from the request that carried it:
// exactly when the trigger_id stops working. It reports false for another
// adapter's interaction and for a payload that carried no trigger_id, which
// is named by a digest of its bytes instead.
func TriggerOf(in model.Interaction) (Trigger, bool) {
	if in.Source != Type {
		return Trigger{}, false
	}
	for _, r := range in.External {
		if r.Namespace != model.ProviderMessageNamespace || r.Kind != model.RefKindInteraction {
			continue
		}
		id := r.ID
		if i := strings.LastIndexByte(id, ':'); i >= 0 {
			id = id[i+1:]
		}
		if (strings.HasPrefix(r.ID, "action:") || strings.HasPrefix(r.ID, "command:") || strings.HasPrefix(r.ID, "view:")) && triggerPattern.MatchString(id) {
			return Trigger{ID: id, ExpiresAt: in.Deadline}, !in.Deadline.IsZero()
		}
	}
	return Trigger{}, false
}

// triggerPattern is the format Slack documents for a trigger_id: "The
// expected format for the trigger_id argument is '132456.7890123.abcdef'"
// (https://docs.slack.dev/reference/methods/views.open/).
var triggerPattern = regexp.MustCompile(`^[0-9]{1,24}\.[0-9]{1,24}\.[0-9A-Za-z]{1,64}$`)

var (
	viewIDPattern = regexp.MustCompile(`^V[0-9A-Z]{2,30}$`)
	hashPattern   = regexp.MustCompile(`^[0-9A-Za-z._-]{1,64}$`)
)

// viewAction is a validated view effect.
type viewAction struct {
	name string
	// trigger and expires are an open's or a push's.
	trigger string
	expires time.Time
	// viewID, externalID and hash are an update's.
	viewID, externalID, hash string
	// view is the compact JSON of the modal.
	view json.RawMessage
}

// checkViewAction is the one set of field rules of the view effects. It
// returns the action's fields when they are valid, otherwise the first
// issue, whose message is safe to show: it never repeats a value.
func checkViewAction(action model.Extension) (viewAction, *model.ValidationIssue) {
	refuse := func(field, msg string) (viewAction, *model.ValidationIssue) {
		return viewAction{}, &model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
	}
	var fields map[string]json.RawMessage
	if d := bytes.TrimSpace(action.Data); len(d) == 0 || json.Unmarshal(d, &fields) != nil || fields == nil {
		return refuse("data", "action data must be a JSON object")
	}
	a := viewAction{name: action.Name}
	switch action.Name {
	case ExtensionViewsOpen, ExtensionViewsPush:
		var ok bool
		if a.trigger, ok = stringField(fields, "trigger_id"); !ok || !triggerPattern.MatchString(a.trigger) {
			return refuse("data.trigger_id", "trigger_id must be the one of a Slack interaction")
		}
		at, ok := stringField(fields, "expires_at")
		var err error
		if a.expires, err = time.Parse(time.RFC3339, at); !ok || err != nil || a.expires.IsZero() {
			return refuse("data.expires_at", "expires_at must be an RFC 3339 time: when the trigger_id stops working")
		}
	case ExtensionViewsUpdate:
		viewID, hasID := optionalString(fields, "view_id")
		extID, hasExt := optionalString(fields, "external_id")
		switch {
		case hasID == hasExt:
			return refuse("data", "name the view by view_id or by external_id, one of them")
		case hasID && !viewIDPattern.MatchString(viewID):
			return refuse("data.view_id", "view_id must be a Slack view ID")
		case hasExt && !textBetween(extID, 1, maxViewIDChars):
			return refuse("data.external_id", "external_id must be 1 to 255 characters")
		}
		a.viewID, a.externalID = viewID, extID
		if h, set := optionalString(fields, "hash"); set {
			if !hashPattern.MatchString(h) {
				return refuse("data.hash", "hash must be the hash Slack gave the view")
			}
			a.hash = h
		} else if v, present := fields["hash"]; present && !isNull(v) {
			return refuse("data.hash", "hash must be a string")
		}
	default:
		return refuse("name", "unknown view effect")
	}
	raw, ok := fields["view"]
	if !ok {
		return refuse("data.view", "view is required")
	}
	view, issue := checkView(raw)
	if issue != nil {
		return viewAction{}, issue
	}
	a.view = view
	return a, nil
}

// stringField reads field as a JSON string.
func stringField(fields map[string]json.RawMessage, field string) (string, bool) {
	raw, ok := fields[field]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// optionalString reads field as a non-empty JSON string; a field absent,
// null or of another type is not set.
func optionalString(fields map[string]json.RawMessage, field string) (string, bool) {
	s, ok := stringField(fields, field)
	return s, ok && s != ""
}

// textBetween reports whether s is valid text of min to max characters with
// no control character.
func textBetween(s string, min, max int) bool {
	n := utf8.RuneCountInString(s)
	return utf8.ValidString(s) && n >= min && n <= max && strings.IndexFunc(s, unicode.IsControl) < 0
}

// checkView checks a modal view and returns it compact.
func checkView(raw json.RawMessage) (json.RawMessage, *model.ValidationIssue) {
	refuse := func(field, msg string) (json.RawMessage, *model.ValidationIssue) {
		return nil, &model.ValidationIssue{Field: "data.view." + field, Code: string(model.Validation), Message: msg}
	}
	var view map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(raw), &view) != nil || view == nil {
		return nil, &model.ValidationIssue{Field: "data.view", Code: string(model.Validation), Message: "view must be a JSON object"}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil || compact.Len() > MaxViewBytes {
		return nil, &model.ValidationIssue{Field: "data.view", Code: string(model.Validation), Message: "view is larger than Slack takes (250 KiB)"}
	}
	if t, _ := stringField(view, "type"); t != "modal" {
		return refuse("type", `type must be "modal"`)
	}
	if !plainText(view["title"], 1, maxViewTitleChars) {
		return refuse("title", "title must be plain_text of 1 to 24 characters")
	}
	for _, button := range []string{"close", "submit"} {
		if b, set := view[button]; set && !isNull(b) && !plainText(b, 1, maxViewButtonChars) {
			return refuse(button, button+" must be plain_text of 1 to 24 characters")
		}
	}
	var blocks []json.RawMessage
	if b, set := view["blocks"]; !set || json.Unmarshal(b, &blocks) != nil || blocks == nil {
		return refuse("blocks", "blocks must be an array")
	}
	if len(blocks) > maxViewBlocks {
		return refuse("blocks", "at most 100 blocks")
	}
	if hasInputBlock(blocks) {
		if b, set := view["submit"]; !set || isNull(b) {
			return refuse("submit", "submit is required when a block is an input")
		}
	}
	for _, f := range []struct {
		field string
		max   int
	}{{"callback_id", maxViewIDChars}, {"external_id", maxViewIDChars}, {"private_metadata", maxViewMetadataChars}} {
		if v, set := view[f.field]; set && !isNull(v) {
			if s, ok := stringField(view, f.field); !ok || !validViewString(s, f.max) {
				return refuse(f.field, f.field+" must be a string of at most "+strconv.Itoa(f.max)+" characters")
			}
		}
	}
	for _, f := range []string{"clear_on_close", "notify_on_close", "submit_disabled"} {
		if v, set := view[f]; set && !isNull(v) {
			var b bool
			if json.Unmarshal(v, &b) != nil {
				return refuse(f, f+" must be a boolean")
			}
		}
	}
	return compact.Bytes(), nil
}

// validViewString: private_metadata may hold any text, the identifiers
// none of the control characters.
func validViewString(s string, max int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= max
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// plainText reports whether raw is a plain_text object of min to max
// characters.
func plainText(raw json.RawMessage, min, max int) bool {
	var t struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &t) != nil || t.Type != "plain_text" {
		return false
	}
	n := utf8.RuneCountInString(t.Text)
	return utf8.ValidString(t.Text) && n >= min && n <= max
}

func hasInputBlock(blocks []json.RawMessage) bool {
	for _, b := range blocks {
		var t struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &t) == nil && t.Type == "input" {
			return true
		}
	}
	return false
}

// viewArgs are the arguments of views.open, views.push and views.update:
// the modal travels as the JSON object it is, as the modals guide shows.
type viewArgs struct {
	TriggerID  string          `json:"trigger_id,omitempty"`
	ViewID     string          `json:"view_id,omitempty"`
	ExternalID string          `json:"external_id,omitempty"`
	Hash       string          `json:"hash,omitempty"`
	View       json.RawMessage `json:"view"`
}

// viewReply is the part of a view method's answer the adapter reads.
type viewReply struct {
	View struct {
		ID string `json:"id"`
	} `json:"view"`
}

// Execute runs a view effect: one call to views.open, views.push or
// views.update, as the action names.
//
// The attempt is made once. The answer is read as a write's: a definite
// refusal is a final rejected outcome (an expired or already used
// trigger_id, a view Slack finds invalid or too large, a view that is not
// there, a stale hash, a duplicate external_id, a full stack); a rate limit
// is retryable, as nothing was applied, but only while the trigger_id lives,
// because every attempt refuses a passed expires_at before any call; an
// answer lost after the request was written, or a server error, is outcome
// unknown and is never retried, because the trigger_id may be spent. A
// modal leaves nothing to look up, so reconciling is not possible.
// The outcome of an opened or pushed view names it, as RefKindView in
// Namespace, for a later update.
func (x *Instance) Execute(ctx context.Context, r adapter.ExecuteRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	if r.Route.Channel != x.channel.Ref {
		return adapter.Outcome{}, final(model.Validation, "route is not of this channel")
	}
	a, refused := x.checkViewAction(r.Action)
	if refused.State != "" {
		return refused, nil
	}
	method, args := "views.update", viewArgs{ViewID: a.viewID, ExternalID: a.externalID, Hash: a.hash, View: a.view}
	if a.name != ExtensionViewsUpdate {
		method = "views.open"
		if a.name == ExtensionViewsPush {
			method = "views.push"
		}
		now := x.now()
		switch {
		case !now.Before(a.expires):
			return rejected(model.DeadlineExpired, "trigger_id expired before the attempt"), nil
		case a.expires.After(now.Add(TriggerLifetime + triggerSkew)):
			return rejected(model.Validation, "expires_at is later than a trigger_id lives"), nil
		}
		args = viewArgs{TriggerID: a.trigger, View: a.view}
	}
	var reply viewReply
	if err := x.call(ctx, method, &args, &reply); err != nil {
		return viewFailure(ctx, err)
	}
	out := adapter.Outcome{State: model.ProviderAccepted}
	if id := reply.View.ID; id != "" {
		out.References = []model.ExternalRef{{Channel: x.channel.Ref, Namespace: Namespace, Kind: RefKindView, ID: id}}
	}
	return out, nil
}

// checkViewAction is the full check of a view action at the attempt: the
// name and version, then the fields. A refusal is a final rejected outcome
// made before any call.
func (x *Instance) checkViewAction(action model.Extension) (viewAction, adapter.Outcome) {
	switch {
	case action.Version != ExtensionVersion:
		return viewAction{}, rejected(model.SchemaMismatch, "unsupported action version")
	case !isViewAction(action.Name):
		return viewAction{}, rejected(model.Unsupported, "unknown slack action")
	}
	a, issue := checkViewAction(action)
	if issue != nil {
		return viewAction{}, rejected(model.Validation, issue.Message)
	}
	return a, adapter.Outcome{}
}

// viewCodes are the definite refusals the view methods add to the shared
// ones (https://docs.slack.dev/reference/methods/views.open/, views.push,
// views.update), by what they mean for the effect.
var viewCodes = map[string]model.ErrorCode{
	"expired_trigger_id":    model.DeadlineExpired,
	"exchanged_trigger_id":  model.Conflict,
	"invalid_trigger_id":    model.Validation,
	"view_too_large":        model.Validation,
	"not_found":             model.NotFound,
	"hash_conflict":         model.Conflict,
	"duplicate_external_id": model.Conflict,
	"push_limit_reached":    model.Conflict,
}

// viewFailure maps a failed view call: the codes of viewCodes are final
// refusals, any other failure is a write's (see failure).
func viewFailure(ctx context.Context, err error) (adapter.Outcome, error) {
	if ae, ok := errors.AsType[*apiError](err); ok {
		if code, known := viewCodes[ae.code]; known {
			return rejected(code, "Slack refused the view: "+ae.code), nil
		}
	}
	return failure(ctx, err, writeCall)
}

var _ adapter.ExtensionExecutor = (*Instance)(nil)
var _ adapter.ActionValidator = (*Factory)(nil)
