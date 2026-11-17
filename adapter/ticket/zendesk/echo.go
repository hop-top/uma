package zendesk

import (
	"bytes"
	"encoding/json"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Echo correlation.
//
// Every ticket update the adapter makes carries a marker in the update's
// custom audit metadata (the request's ticket.metadata, which Zendesk keeps
// as metadata.custom on the audit it creates): the uma operation and
// attempt, the effect kind and the effect's audience. Comments do not
// carry custom metadata themselves; their audit does. When a comment the
// adapter may have written comes back inbound (authored by staff on a
// webhook, created through the API on a poll), the adapter reads the
// comment's audit:
//
//   - a public reply's echo, and a delivered internal note's, is handed on
//     with model.EchoRef naming the attempt, so core joins it to the
//     uma-authored message even when it arrives before the send's answer;
//   - an internal note or ticket update made through an extension effect
//     has no uma message to join: its echo is acknowledged and dropped,
//     never recorded as a new message.
//
// A comment without a marker is ordinary inbound traffic. Body and sender
// never correlate anything: the sender only decides whether the audit is
// worth reading.

// Effect kinds (marker.Effect).
const (
	effectReply = "reply"
	// effectMessageNote is an internal note uma recorded and delivered
	// (Instance.SendNote): like a reply, its echo joins a uma message.
	effectMessageNote = "message_note"
	effectNote        = "note"
	effectUpdate      = "update"
)

// marker is the echo marker of one effect attempt.
type marker struct {
	Operation  string `json:"uma_operation"`
	Attempt    string `json:"uma_attempt"`
	Effect     string `json:"uma_effect"`
	Visibility string `json:"uma_visibility"`
	PolicyRef  string `json:"uma_policy_ref,omitempty"`
}

func newMarker(eff adapter.Effect, kind string) marker {
	return marker{
		Operation: string(eff.Operation.ID), Attempt: string(eff.Attempt), Effect: kind,
		Visibility: string(eff.Audience.Visibility), PolicyRef: eff.Audience.PolicyRef,
	}
}

// decodeMarker reads a marker from an audit's custom metadata. Anything
// that is not a complete marker with valid IDs is no marker.
func decodeMarker(custom json.RawMessage) (marker, bool) {
	if len(bytes.TrimSpace(custom)) == 0 {
		return marker{}, false
	}
	var m marker
	if err := json.Unmarshal(custom, &m); err != nil {
		return marker{}, false
	}
	if _, err := model.ParseID(m.Operation); err != nil {
		return marker{}, false
	}
	if _, err := model.ParseID(m.Attempt); err != nil {
		return marker{}, false
	}
	switch m.Effect {
	case effectReply, effectMessageNote, effectNote, effectUpdate:
		return m, true
	}
	return marker{}, false
}

// of reports whether the marker names the effect's attempt.
func (m marker) of(eff adapter.Effect) bool {
	return m.Operation == string(eff.Operation.ID) && m.Attempt == string(eff.Attempt)
}

// echoRef is the envelope reference of a reply echo.
func (m marker) echoRef(ch model.ChannelRef) model.ExternalRef {
	return model.EchoRef(ch, model.OperationRef{Tenant: ch.Tenant, ID: model.ID(m.Operation)}, model.ID(m.Attempt))
}
