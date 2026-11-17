package microsoft

import (
	"bytes"
	"context"
	"encoding/json"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Extension effects the adapter ships, registered from Factory.Extensions.
const (
	// ExtensionCategories replaces the Outlook categories of one message:
	// PATCH /users/{mailbox}/messages/{id} with "categories". Its data is
	// {"set": [category names]}, the whole new list; an empty list clears
	// the message's categories. Names are the mailbox's master category
	// display names, each once, none empty. Fields the adapter does not
	// know are ignored and never sent. The message is the effect's Target
	// when it names a provider message of the channel, else the route's
	// ParentID (the message the peer wrote); an effect with neither is
	// rejected. uma's labels stay local and canonical: the effect is
	// authorized as model.ActionConversationLabel on the conversation and
	// reads or writes no uma label. Mail.ReadWrite covers it.
	ExtensionCategories = string(Type) + ".categories"
	// ExtensionVersion is the version of the effect.
	ExtensionVersion = 1
)

// categoriesAction are the fields of an ExtensionCategories action.
type categoriesAction struct {
	Set []string `json:"set"`
}

// Extensions returns the extension effects the adapter ships, for the host
// to register with the factory.
func (f *Factory) Extensions() []adapter.ExtensionDefinition {
	return append([]adapter.ExtensionDefinition{{
		Name: ExtensionCategories, Version: ExtensionVersion, Kind: model.ExtensionKindEffect,
		InputSchema:        model.Extension{Name: ExtensionCategories + ".input", Version: ExtensionVersion},
		RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: model.ActionConversationLabel,
	}, replyDraftDefinition(), unsubscribeDefinition()}, mailboxExtensions()...)
}

// ValidateAction checks the fields of an extension effect without I/O
// (adapter.ActionValidator): the rules Execute applies at the attempt, so
// an action Graph would refuse is a model.Validation error at the call
// with nothing stored. An action of another name or version is not judged
// here; core refuses it by its registration and Execute by its name.
func (f *Factory) ValidateAction(_ model.Audience, action model.Extension) []model.ValidationIssue {
	if action.Name == ExtensionUnsubscribe {
		return validateUnsubscribe(action)
	}
	if action.Name == string(ExtensionMailboxCreate) {
		return validateMailboxCreate(action)
	}
	if action.Version != ExtensionVersion {
		return nil
	}
	switch action.Name {
	case ExtensionCategories:
	case ExtensionReplyDraft:
		if _, issue := checkReplyDraft(action); issue != nil {
			return []model.ValidationIssue{*issue}
		}
		return nil
	default:
		return nil
	}
	if _, issue := checkCategories(action); issue != nil {
		return []model.ValidationIssue{*issue}
	}
	return nil
}

// checkCategories is the one set of field rules of the categories effect.
// It returns the action's fields when they are valid, otherwise the first
// issue, whose message is safe to show.
func checkCategories(action model.Extension) (categoriesAction, *model.ValidationIssue) {
	refuse := func(field, msg string) (categoriesAction, *model.ValidationIssue) {
		return categoriesAction{}, &model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
	}
	data := bytes.TrimSpace(action.Data)
	if len(data) == 0 {
		return refuse("data.set", "a category change sets the whole list, empty to clear")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return refuse("data", "action data must be a JSON object")
	}
	raw, set := fields["set"]
	if !set || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return refuse("data.set", "a category change sets the whole list, empty to clear")
	}
	var a categoriesAction
	if err := json.Unmarshal(raw, &a.Set); err != nil {
		return refuse("data.set", "set must be an array of category names")
	}
	if a.Set == nil {
		a.Set = []string{}
	}
	seen := make(map[string]bool, len(a.Set))
	for _, name := range a.Set {
		switch {
		case name == "":
			return refuse("data.set", "a category name is empty")
		case seen[name]:
			return refuse("data.set", "category "+name+" appears more than once")
		}
		seen[name] = true
	}
	return a, nil
}

// Execute runs the categories effect on the message the effect targets:
// PATCH with the action's list as categories. The call is idempotent (the
// list replaces whatever the message held), so a lost answer or a 5xx is
// transient and the engine repeats the attempt; it is never
// outcome_unknown. A 404 (the message is gone) is a final rejected outcome
// with not_found; a 403 (the token lacks Mail.ReadWrite on the mailbox) is
// unauthorized and never retried.
func (i *instance) Execute(ctx context.Context, req adapter.ExecuteRequest) (adapter.Outcome, error) {
	if req.Action.Name == string(ExtensionMailboxCreate) {
		return i.createMailbox(ctx, req)
	}
	if err := i.checkRoute(req.Route); err != nil {
		return adapter.Outcome{}, err
	}
	if req.Action.Name == ExtensionUnsubscribe {
		return i.unsubscribeEffect(ctx, req)
	}
	if req.Action.Name != ExtensionCategories && req.Action.Name != ExtensionReplyDraft {
		return rejected(model.Unsupported, "unknown "+string(Type)+" action"), nil
	}
	if req.Action.Version != ExtensionVersion {
		return rejected(model.SchemaMismatch, "unsupported action version"), nil
	}
	if req.Action.Name == ExtensionReplyDraft {
		return i.executeReplyDraft(ctx, req)
	}
	a, issue := checkCategories(req.Action)
	if issue != nil {
		return rejected(model.Validation, issue.Message), nil
	}
	id, ok := i.targetMessage(req.Effect)
	if !ok {
		return rejected(model.Validation, "no provider message to categorize: the effect names none and the route has no parent message"), nil
	}
	m, err := i.messageCall(ctx, i.c.session(), "PATCH", i.messageURL(id, ""), encode(gCategories{Categories: a.Set}))
	if err != nil {
		return rejectedOr(err)
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMessageRef(i.ch.Ref, m.ID)}}, nil
}

// targetMessage is the Graph message an effect acts on: Target when set,
// which must then name a provider message of this channel, else the
// route's ParentID. The second value is false when the effect names no
// message.
func (i *instance) targetMessage(eff adapter.Effect) (string, bool) {
	if t := eff.Target; t != nil {
		if t.Channel != i.ch.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage || t.ID == "" {
			return "", false
		}
		return t.ID, true
	}
	return eff.Route.ParentID, eff.Route.ParentID != ""
}

func rejected(code model.ErrorCode, msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Rejected, Failure: &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}}
}

var (
	_ adapter.ActionValidator   = (*Factory)(nil)
	_ adapter.ExtensionExecutor = (*instance)(nil)
)
