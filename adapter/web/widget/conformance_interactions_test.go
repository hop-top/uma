package widget_test

import (
	"encoding/json"

	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// visitorRequest is the request the visitor's browser makes for ev: a
// message, or an interaction posted to the route of its kind, with the
// event ID as client ID.
func visitorRequest(ev conformance.InboundEvent) (path string, body []byte) {
	in := ev.Interaction
	switch {
	case in == nil:
		body, _ = json.Marshal(widget.MessageRequest{ID: ev.EventID, Text: ev.Text})
		return "/widget/messages", body
	case in.Kind == model.InteractionSubmission:
		body, _ = json.Marshal(widget.SubmissionRequest{ID: ev.EventID, Message: ev.MessageID, Fields: in.Fields})
		return "/widget/submissions", body
	case in.Kind == model.InteractionCommand:
		body, _ = json.Marshal(widget.ActionRequest{ID: ev.EventID, Kind: in.Kind, Command: in.Command, Arguments: in.Arguments, Message: ev.MessageID, Fields: in.Fields})
		return "/widget/actions", body
	}
	body, _ = json.Marshal(widget.ActionRequest{ID: ev.EventID, Action: in.Action, Value: in.Arguments, Message: ev.MessageID, Fields: in.Fields})
	return "/widget/actions", body
}
