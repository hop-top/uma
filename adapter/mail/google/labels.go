package google

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Extension effects the adapter ships, registered from Factory.Extensions.
const (
	// ExtensionLabels adds Gmail labels to one message and removes labels
	// from it, with users.messages.modify (addLabelIds, removeLabelIds).
	// Its data is {"add": [label IDs], "remove": [label IDs]}: at least one
	// ID overall, each ID once across both lists, at most 100 per list
	// (Gmail's limit per update). Fields the adapter does not know are
	// ignored and never sent. The message is the effect's Target when it
	// names a provider message of the channel, else the route's ParentID
	// (the message the peer wrote); an effect with neither is rejected.
	// uma's labels stay local and canonical: the effect is authorized as
	// model.ActionConversationLabel on the conversation and reads or
	// writes no uma label. It needs ScopeModify.
	ExtensionLabels = string(Type) + ".labels"
	// ExtensionVersion is the version of the effect.
	ExtensionVersion = 1
)

// maxLabelsPerList is Gmail's limit on addLabelIds and on removeLabelIds
// per users.messages.modify call.
const maxLabelsPerList = 100

// labelsAction are the fields of an ExtensionLabels action.
type labelsAction struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// Extensions returns the extension effects the adapter ships, for the host
// to register with the factory.
func (f *Factory) Extensions() []adapter.ExtensionDefinition {
	return append([]adapter.ExtensionDefinition{{
		Name: ExtensionLabels, Version: ExtensionVersion, Kind: model.ExtensionKindEffect,
		InputSchema:        model.Extension{Name: ExtensionLabels + ".input", Version: ExtensionVersion},
		RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: model.ActionConversationLabel,
	}, replyDraftDefinition(), unsubscribeDefinition(), forwardDefinition()}, mailboxExtensions()...)
}

// ValidateAction checks the fields of an extension effect without I/O
// (adapter.ActionValidator): the rules Execute applies at the attempt, so
// an action Gmail would refuse is a model.Validation error at the call
// with nothing stored. An action of another name or version is not judged
// here; core refuses it by its registration and Execute by its name.
func (f *Factory) ValidateAction(_ model.Audience, action model.Extension) []model.ValidationIssue {
	if action.Name == ExtensionUnsubscribe {
		return validateUnsubscribe(action)
	}
	if action.Name == ExtensionForward {
		if action.Version != ExtensionVersion {
			return nil
		}
		if _, issue := checkForward(action); issue != nil {
			return []model.ValidationIssue{*issue}
		}
		return nil
	}
	if action.Name == string(ExtensionMailboxCreate) {
		return validateMailboxCreate(action)
	}
	if action.Version != ExtensionVersion {
		return nil
	}
	switch action.Name {
	case ExtensionLabels:
	case ExtensionReplyDraft:
		if _, issue := checkReplyDraft(action); issue != nil {
			return []model.ValidationIssue{*issue}
		}
		return nil
	default:
		return nil
	}
	if _, issue := checkLabels(action); issue != nil {
		return []model.ValidationIssue{*issue}
	}
	return nil
}

// checkLabels is the one set of field rules of the labels effect. It
// returns the action's fields when they are valid, otherwise the first
// issue, whose message is safe to show.
func checkLabels(action model.Extension) (labelsAction, *model.ValidationIssue) {
	refuse := func(field, msg string) (labelsAction, *model.ValidationIssue) {
		return labelsAction{}, &model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
	}
	var a labelsAction
	if data := bytes.TrimSpace(action.Data); len(data) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
			return refuse("data", "action data must be a JSON object")
		}
		for _, l := range []struct {
			field string
			dst   *[]string
		}{{"add", &a.Add}, {"remove", &a.Remove}} {
			raw, set := fields[l.field]
			if !set || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				continue
			}
			if err := json.Unmarshal(raw, l.dst); err != nil {
				return refuse("data."+l.field, l.field+" must be an array of label IDs")
			}
		}
	}
	if len(a.Add)+len(a.Remove) == 0 {
		return refuse("data", "a label change names at least one label to add or remove")
	}
	if len(a.Add) > maxLabelsPerList || len(a.Remove) > maxLabelsPerList {
		return refuse("data", "at most 100 labels can be added, and 100 removed, per change")
	}
	seen := make(map[string]bool, len(a.Add)+len(a.Remove))
	for _, id := range append(append([]string(nil), a.Add...), a.Remove...) {
		switch {
		case id == "":
			return refuse("data", "a label ID is empty")
		case seen[id]:
			return refuse("data", "label "+id+" appears more than once")
		}
		seen[id] = true
	}
	return a, nil
}

// Execute runs the labels effect on the message the effect targets:
// users.messages.modify with the action's lists. The call is idempotent
// (a label already present or already absent is left as it is), so a lost
// answer or a 5xx is transient and the engine repeats the attempt; it is
// never outcome_unknown. A 404 (the message is gone) is a final rejected
// outcome with not_found; a 403 without a rate-limit reason (the token
// lacks ScopeModify) is unauthorized and never retried.
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
	if req.Action.Name == ExtensionForward {
		return i.forwardEffect(ctx, req)
	}
	if req.Action.Name != ExtensionLabels && req.Action.Name != ExtensionReplyDraft {
		return rejected(model.Unsupported, "unknown "+string(Type)+" action"), nil
	}
	if req.Action.Version != ExtensionVersion {
		return rejected(model.SchemaMismatch, "unsupported action version"), nil
	}
	if req.Action.Name == ExtensionReplyDraft {
		return i.executeReplyDraft(ctx, req)
	}
	a, issue := checkLabels(req.Action)
	if issue != nil {
		return rejected(model.Validation, issue.Message), nil
	}
	id, ok := i.targetMessage(req.Effect)
	if !ok {
		return rejected(model.Validation, "no provider message to label: the effect names none and the route has no parent message"), nil
	}
	s := i.c.session()
	resp, err := s.do(ctx, request{
		method: "POST", url: i.c.api("/messages/"+url.PathEscape(id)+"/modify", nil),
		body: encode(gModify{AddLabelIDs: a.Add, RemoveLabelIDs: a.Remove}),
	})
	if err != nil {
		return rejectedOr(err)
	}
	var m gMessage
	if err := decode(resp.body, &m); err != nil || m.ID == "" {
		// Applied, but the answer does not name the message: keep the ID
		// the request named.
		m.ID = id
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMessageRef(i.ch.Ref, m.ID)}}, nil
}

// targetMessage is the Gmail message an effect acts on: Target when set,
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
