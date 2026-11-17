package relay

import (
	"bytes"
	"encoding/json"
	"strings"

	"hop.top/uma/model"
)

// PayloadStatus names the payload of a status envelope: the update as the
// relay reported it (see StatusPayload).
const PayloadStatus = "relay.messages.status"

// Status values of a status update, in the order a message moves through
// them: accepted, sent, delivered, read. failed is the other end: a
// message that did not reach its recipient. The value is matched ignoring
// case.
const (
	StatusAccepted  = "accepted"
	StatusSent      = "sent"
	StatusDelivered = "delivered"
	StatusRead      = "read"
	StatusFailed    = "failed"
)

// maxCodeBytes bounds a failure code.
const maxCodeBytes = 64

// statusState is the delivery state each status value reports. The engine
// distinguishes provider_accepted, delivered, failed and rejected: it has no
// separate states for accepted and sent, nor a read state, so those pairs
// share one and the status value itself travels in the payload.
var statusState = map[string]model.DeliveryState{
	StatusAccepted:  model.ProviderAccepted,
	StatusSent:      model.ProviderAccepted,
	StatusDelivered: model.Delivered,
	StatusRead:      model.Delivered,
	StatusFailed:    model.Failed,
}

// StatusPayload is the PayloadStatus payload (version 1) of a status
// envelope: delivery evidence for the reply the update names.
type StatusPayload struct {
	// Status is the relay's value, lower-cased: accepted, sent, delivered,
	// read or failed.
	Status string `json:"status"`
	// State is the delivery state it evidences.
	State model.DeliveryState `json:"state"`
	// Read is set for read.
	Read bool `json:"read,omitempty"`
	// Operation and Attempt are the operation and attempt the update
	// names; the envelope then also references model.EchoRef, so the engine
	// applies the report to that attempt. Both or neither.
	Operation model.ID `json:"operation,omitempty"`
	Attempt   model.ID `json:"attempt,omitempty"`
	// MessageID is the relay's id for the message.
	MessageID string `json:"message_id,omitempty"`
	// Code is the relay's reason code of a failure.
	Code string `json:"code,omitempty"`
	// Failure summarizes a failed update.
	Failure *model.DeliveryFailure `json:"failure,omitempty"`
}

// statusEnvelope normalizes the fields of a status update.
//
// An update needs a status value this adapter knows, the line the reply left
// from (the destination an inbox binds) and something to tell which reply it
// is about: the operation_id and attempt_id the callback carried, both or
// neither, and the relay's message_id. An update naming no operation and
// attempt is kept as a status event and settles nothing: the engine never
// guesses an attempt from a message id, which is the relay's own. An update
// naming an operation or attempt the engine does not hold is accepted and
// ignored by the engine in the same way.
//
// The event ID is bound to the update's content: the line, the operation,
// the attempt, the message id, the status value and a failure's code. The
// same update sent again is one event; a different status or a different
// code of the same message is another, so a relay that reuses its ids
// cannot make a later update look like a redelivery of an earlier one. The
// time of the update is not part of it: the relay's retry of one update is
// the same event whenever it is sent.
func (i *Instance) statusEnvelope(fields map[string]json.RawMessage) (model.Envelope, error) {
	f := i.cfg.status
	raw, ok := stringOf(fields, f.Status, validStatusWord)
	if !ok {
		return model.Envelope{}, errBadMessage
	}
	word := strings.ToLower(raw)
	state, ok := statusState[word]
	if !ok {
		return model.Envelope{}, errBadMessage
	}
	line, ok := stringOf(fields, f.From, validAddress)
	if !ok {
		return model.Envelope{}, errBadMessage
	}
	line = i.handle(line)
	opID, attemptID, ok := statusTag(fields, f)
	if !ok {
		return model.Envelope{}, errBadMessage
	}
	msgID, found, ok := optionalString(fields, f.MessageID)
	if !ok || (found && !validMessageID(msgID)) {
		return model.Envelope{}, errBadMessage
	}
	if opID == "" && !found {
		return model.Envelope{}, errBadMessage
	}
	p := StatusPayload{Status: word, State: state, Read: word == StatusRead, Operation: opID, Attempt: attemptID, MessageID: msgID}
	now := i.clock.Now().UTC()
	report := model.DeliveryReport{State: state}
	if p.Read {
		// The engine records the first confirmation as the operation's read
		// time and keeps it delivered.
		report.ReadAt = &now
	}
	code := ""
	if state == model.Failed {
		// The code means something only on a failure; elsewhere it is not
		// read, so a relay that always sends one is not refused.
		if code, ok = codeOf(fields, f.Code); !ok {
			return model.Envelope{}, errBadMessage
		}
		msg := "relay reported the delivery failed"
		if code != "" {
			msg += " (code " + code + ")"
		}
		p.Code = code
		p.Failure = &model.DeliveryFailure{Code: model.Permanent, Retry: model.NoRetry, SafeMessage: msg}
		report.Failure = p.Failure
	}
	data, err := json.Marshal(p)
	if err != nil {
		return model.Envelope{}, errBadMessage
	}
	var refs []model.ExternalRef
	if msgID != "" {
		refs = append(refs, model.ProviderMessageRef(i.channel.Ref, msgID))
	}
	if opID != "" {
		refs = append(refs, model.EchoRef(i.channel.Ref, model.OperationRef{Tenant: i.channel.Ref.Tenant, ID: opID}, attemptID))
	}
	return model.Envelope{
		EventID: digest("status", line, string(opID), string(attemptID), msgID, word, code), MessageID: msgID,
		Destination: line, Kind: model.EnvelopeStatus, Assurance: "verified",
		References: refs,
		Audience:   model.Audience{Visibility: model.Public},
		OccurredAt: now,
		Payload:    model.Extension{Name: PayloadStatus, Version: 1, Data: data},
		Delivery:   &report,
	}, nil
}

// statusTag reads the operation and attempt an update names: both valid IDs
// or neither present. ok is false for one without the other or an invalid one.
func statusTag(fields map[string]json.RawMessage, f StatusFields) (op, attempt model.ID, ok bool) {
	o, haveOp, ok1 := optionalString(fields, f.OperationID)
	a, haveAttempt, ok2 := optionalString(fields, f.AttemptID)
	if !ok1 || !ok2 {
		return "", "", false
	}
	if !haveOp && !haveAttempt {
		return "", "", true
	}
	if !haveOp || !haveAttempt {
		return "", "", false
	}
	var err1, err2 error
	op, err1 = model.ParseID(o)
	attempt, err2 = model.ParseID(a)
	if err1 != nil || err2 != nil {
		return "", "", false
	}
	return op, attempt, true
}

// codeOf reads the optional failure code: a string, or a number kept as its
// JSON text, of at most maxCodeBytes letters, digits and . _ : -.
func codeOf(fields map[string]json.RawMessage, names []string) (string, bool) {
	raw, found := pick(fields, names)
	if !found {
		return "", true
	}
	raw = bytes.TrimSpace(raw)
	if string(raw) == "null" {
		return "", true
	}
	var code string
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &code); err != nil {
			return "", false
		}
	} else {
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return "", false
		}
		code = n.String()
	}
	if len(code) > maxCodeBytes {
		return "", false
	}
	for _, c := range []byte(code) {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-'
		if !ok {
			return "", false
		}
	}
	return code, true
}

// validStatusWord bounds what is looked up: a status value is a short word.
func validStatusWord(s string) bool { return s != "" && len(s) <= 16 && printable(s) }
