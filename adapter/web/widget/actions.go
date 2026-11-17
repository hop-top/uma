package widget

import (
	"net/http"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// ActionRequest is the JSON body of a control press or a command: the
// client-chosen ID, unique per visitor (a retried request with the same ID
// is a duplicate, never a second interaction), and what the visitor did.
//
// Kind is model.InteractionAction, the default, or model.InteractionCommand.
// An action names the control's Action ID and may carry the Value the
// visitor selected; a command names Command and may carry the raw
// Arguments typed after it. Message is the ID of the message the control
// sits on, as the event stream delivered it, when there is one. Fields are
// the state of the form the control belongs to, when it carries any.
type ActionRequest struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind,omitempty"`
	Action    string          `json:"action,omitempty"`
	Command   string          `json:"command,omitempty"`
	Value     string          `json:"value,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Message   string          `json:"message,omitempty"`
	Fields    model.Extension `json:"fields,omitzero"`
}

// SubmissionRequest is the JSON body of a form submission: the
// client-chosen ID, as for an ActionRequest, and the submitted values under
// the form's schema (Fields.Name, a registry key, with its version). The
// engine and the host's interaction handlers judge the values; the
// receiver only checks the shape. Message is the ID of the message the form
// sits on, when there is one.
type SubmissionRequest struct {
	ID      string          `json:"id"`
	Message string          `json:"message,omitempty"`
	Fields  model.Extension `json:"fields"`
}

// AcceptedInteraction is the JSON answer to an accepted action, command or
// submission. Interaction is the ID the interaction has from now on: the
// provider interaction ID the engine recorded, and the ID an
// interaction.response event names. Duplicate is set when the engine had
// the interaction already.
type AcceptedInteraction struct {
	Interaction string `json:"interaction"`
	Duplicate   bool   `json:"duplicate"`
}

func (x *Instance) serveAction(w http.ResponseWriter, r *http.Request, sink adapter.Sink, c claims) {
	var req ActionRequest
	if !decode(w, r, &req) {
		return
	}
	in := model.InteractionInput{Kind: req.Kind, Fields: req.Fields}
	if in.Kind == "" {
		in.Kind = model.InteractionAction
	}
	switch in.Kind {
	case model.InteractionAction:
		in.Action, in.Arguments = req.Action, req.Value
	case model.InteractionCommand:
		in.Command, in.Arguments = req.Command, req.Arguments
	}
	ok := req.ID != "" && (in.Kind == model.InteractionAction && req.Action != "" && req.Command == "" && req.Arguments == "" ||
		in.Kind == model.InteractionCommand && req.Command != "" && req.Action == "" && req.Value == "")
	x.accept(w, r, sink, c, in, req.ID, req.Message, ok)
}

func (x *Instance) serveSubmission(w http.ResponseWriter, r *http.Request, sink adapter.Sink, c claims) {
	var req SubmissionRequest
	if !decode(w, r, &req) {
		return
	}
	in := model.InteractionInput{Kind: model.InteractionSubmission, Fields: req.Fields}
	x.accept(w, r, sink, c, in, req.ID, req.Message, req.Fields.Name != "")
}

// accept completes in with what the session establishes, never what the
// body says: the actor, the room and the destination come from the token.
// It answers 202 only after the sink accepted the interaction, so the
// browser's success means the engine holds it durably. wellFormed is the
// caller's verdict on the body; the shared checks are made here.
func (x *Instance) accept(w http.ResponseWriter, r *http.Request, sink adapter.Sink, c claims, in model.InteractionInput, clientID, message string, wellFormed bool) {
	noFields := in.Fields.Name == "" && in.Fields.Version == 0 && len(in.Fields.Data) == 0
	if !wellFormed || !validClientID(clientID) || !noFields && in.Fields.Validate() != nil {
		status(w, http.StatusBadRequest)
		return
	}
	ch := x.ref()
	if message != "" {
		if !isAnyMessageID(message) {
			status(w, http.StatusBadRequest)
			return
		}
		ref := MessageRef(ch, message)
		in.Message = &ref
	}
	room := c.room(ch)
	in.ID, in.Actor, in.Context = inboundID("interaction", c, clientID), c.sender(ch), []model.ExternalRef{room}
	now := x.now()
	in.Deadline = now.Add(x.f.interactionTimeout)
	receipt, err := sink.Accept(r.Context(), model.Ingress{Envelope: model.Envelope{
		EventID: in.ID, Destination: c.Site, Kind: model.EnvelopeInteraction,
		Sender: in.Actor, Assurance: c.Assurance, References: []model.ExternalRef{room},
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: now.UTC(), Interaction: &in,
	}})
	if err != nil {
		sinkError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, AcceptedInteraction{Interaction: in.ID, Duplicate: receipt.Duplicate})
}
