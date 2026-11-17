package github_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// mutationCall maps a comment edit or deletion, or a reaction call, to the
// conformance call it serves. ok is false for any other request; a zero
// Capability in the result drops the request from the record. The lookup a
// reaction removal does first (the account's reactions of a content) is
// part of the removal: it is folded into the call that follows it, and
// only a lookup nothing follows is a reconciliation.
func mutationCall(ch model.ChannelRef, c fakeCall, rest []fakeCall) (conformance.ProviderCall, bool) {
	m, ok := parseMutationPath(strings.TrimPrefix(c.Path, "/"))
	if !ok {
		if c.Method == http.MethodGet && strings.HasPrefix(c.Path, "/repos/") && strings.Contains(c.Path, "/issues/comments/") {
			return conformance.ProviderCall{Capability: model.CapabilityReconcile}, true
		}
		return conformance.ProviderCall{}, false
	}
	repo, number, _ := strings.Cut(c.Issue, "#")
	route := model.ExternalRef{Channel: ch, Namespace: github.Namespace, Kind: model.RefKindWorkItem, ID: number, ParentID: repo}
	pc := conformance.ProviderCall{Route: route, Applied: c.Applied}
	if m.comment != 0 {
		pc.Target = commentRef(ch, m.comment)
	} else {
		n, _ := strconv.Atoi(number)
		pc.Target = model.ProviderMessageRef(ch, "issue:"+strconv.Itoa(3_000_000_000+n))
	}
	switch {
	case !m.reactions && c.Method == http.MethodPatch:
		var req struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal([]byte(c.Body), &req)
		text, op := splitMarker(req.Body)
		pc.Capability, pc.Operation, pc.Attempt = model.CapabilityEdit, model.OperationRef{Tenant: ch.Tenant, ID: op}, attemptOf(req.Body)
		pc.Audience = model.Audience{Visibility: model.Public}
		pc.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}
		if c.Applied {
			pc.References = []model.ExternalRef{pc.Target}
		}
	case !m.reactions && c.Method == http.MethodDelete:
		pc.Capability = model.CapabilityDelete
	case m.reactions && c.Method == http.MethodPost:
		var req struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal([]byte(c.Body), &req)
		pc.Capability, pc.Emoji = model.CapabilityReact, emojiFor(req.Content)
	case m.reactions && c.Method == http.MethodDelete:
		pc.Capability, pc.Remove, pc.Emoji = model.CapabilityReact, true, emojiFor(c.Content)
	case m.reactions && c.Method == http.MethodGet:
		if len(rest) > 0 && rest[0].Method == http.MethodDelete {
			return conformance.ProviderCall{}, true
		}
		pc.Capability = model.CapabilityReconcile
	default:
		return conformance.ProviderCall{}, false
	}
	return pc, true
}

func emojiFor(content string) string {
	for _, rc := range reactionContents {
		if rc.content == content {
			return rc.emoji
		}
	}
	return ""
}

// attemptOf returns the attempt a body's marker names.
func attemptOf(body string) model.ID {
	i := strings.LastIndex(body, "\n\n<!-- uma:")
	if i < 0 {
		return ""
	}
	_, rest, _ := strings.Cut(strings.TrimPrefix(body[i:], "\n\n<!-- uma:"), ":")
	att, _, _ := strings.Cut(rest, " ")
	return model.ID(att)
}
