package zendesk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// updateRequest is the body of PUT /api/v2/tickets/{id}.
type updateRequest struct {
	Ticket map[string]json.RawMessage `json:"ticket"`
}

// updateResponse is the part of its answer the adapter reads.
type updateResponse struct {
	Audit *audit `json:"audit"`
}

// Send posts a public reply on the ticket the route names, as a public
// comment. The outcome of every ticket update (reply, internal note,
// update) maps as follows:
//
//   - 2xx: provider_accepted, with the comment's provider message reference
//     (model.ProviderMessageRef of the Comment audit event's ID) and the
//     audit's reference;
//   - 429: rate_limited with the stated Retry-After, nothing applied;
//   - a dial or TLS handshake failure: transient, the request never left;
//   - 400, 413, 422: rejected, nothing applied;
//   - 404: rejected not_found, the ticket is deleted or inaccessible;
//   - 401, 403: failed unauthorized, never retried;
//   - every 5xx, a 409 collision, a transport failure after the request was
//     written or an unreadable 2xx answer: outcome unknown, so the worker
//     reconciles before any second write. Zendesk takes no idempotency
//     key for ticket updates (its Idempotency-Key header covers ticket
//     creation only), so nothing else makes a second write safe.
//
// Effects that the adapter refuses itself (a non-public reply, a public
// internal note, an action of the wrong shape) are rejected with no call.
func (i *Instance) Send(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error) {
	x := i.x
	if err := x.ready(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	if req.Audience.Visibility != model.Public {
		return rejected(model.Validation, "a reply is public; internal notes use "+ExtensionInternalNote), nil
	}
	text, ok := textOf(req.Content)
	if !ok {
		return rejected(model.Unsupported, "zendesk replies carry text only"), nil
	}
	c, _ := json.Marshal(map[string]any{"body": text, "public": true}) // always encodes
	return x.update(ctx, req.Effect, effectReply, map[string]json.RawMessage{"comment": c})
}

// SendNote delivers an internal note recorded in uma, from an inbox that
// opted in to note delivery, as a private comment ("public": false) on the
// route's ticket. A public audience or non-text content is rejected with
// no call. Its echo joins the note (model.EchoRef), like a reply's.
func (i *Instance) SendNote(ctx context.Context, req adapter.NoteRequest) (adapter.Outcome, error) {
	x := i.x
	if err := x.ready(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	if req.Audience.Visibility == model.Public {
		return rejected(model.Validation, "an internal note never has a public audience"), nil
	}
	text, ok := textOf(req.Content)
	if !ok {
		return rejected(model.Unsupported, "zendesk notes carry text only"), nil
	}
	c, _ := json.Marshal(map[string]any{"body": text, "public": false}) // always encodes
	return x.update(ctx, req.Effect, effectMessageNote, map[string]json.RawMessage{"comment": c})
}

// Execute runs an extension effect on the route's ticket.
func (i *Instance) Execute(ctx context.Context, req adapter.ExecuteRequest) (adapter.Outcome, error) {
	x := i.x
	if err := x.ready(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	if req.Action.Version != ExtensionVersion {
		return rejected(model.SchemaMismatch, "unsupported action version"), nil
	}
	fields, issue := checkAction(req.Audience, req.Action)
	if issue != nil {
		return rejected(model.Validation, issue.Message), nil
	}
	switch req.Action.Name {
	case ExtensionInternalNote:
		fields["public"] = json.RawMessage("false")
		c, _ := json.Marshal(fields) // raw values always encode
		return x.update(ctx, req.Effect, effectNote, map[string]json.RawMessage{"comment": c})
	case ExtensionUpdate:
		return x.update(ctx, req.Effect, effectUpdate, fields)
	}
	return rejected(model.Unsupported, "unknown zendesk action"), nil
}

// update sends one ticket update carrying the attempt's echo marker.
func (x *instance) update(ctx context.Context, eff adapter.Effect, kind string, ticketFields map[string]json.RawMessage) (adapter.Outcome, error) {
	if !isTicketRoute(x.channel.Ref, eff.Route) {
		return rejected(model.Validation, "route is not a ticket of this channel"), nil
	}
	m, _ := json.Marshal(newMarker(eff, kind)) // always encodes
	ticketFields["metadata"] = m
	var resp updateResponse
	err := x.api.do(ctx, http.MethodPut, ticketPath(eff.Route.ID), nil, updateRequest{Ticket: ticketFields}, &resp)
	if err != nil {
		return failed(err)
	}
	return accepted(x.references(eff.Route.ID, resp.Audit)), nil
}

// references are the provider references of an applied update.
func (x *instance) references(ticketID string, a *audit) []model.ExternalRef {
	if a == nil {
		return nil
	}
	var out []model.ExternalRef
	if c, ok := a.commentEvent(); ok && c.ID != "" {
		out = append(out, model.ProviderMessageRef(x.channel.Ref, string(c.ID)))
	}
	if a.ID != "" {
		out = append(out, auditRef(x.channel.Ref, ticketID, a.ID))
	}
	return out
}

// maxReconcilePages bounds the audits one reconciliation reads.
const maxReconcilePages = 10

// auditPage is one page of GET /api/v2/tickets/{id}/audits.
type auditPage struct {
	Audits []audit `json:"audits"`
	Meta   struct {
		HasMore     bool   `json:"has_more"`
		AfterCursor string `json:"after_cursor"`
	} `json:"meta"`
}

// Reconcile reads the ticket's audits, newest first, for the one carrying
// the attempt's marker: found, the effect applied; every audit read
// without it, the effect was not applied and a new attempt is safe; the
// read budget spent first, still unknown. Comments carry no custom
// metadata, so the audits, which hold each comment as an event, are the
// record read.
func (i *Instance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	eff := req.Effect
	x := i.x
	if err := x.ready(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	if !isTicketRoute(x.channel.Ref, eff.Route) {
		return rejected(model.Validation, "route is not a ticket of this channel"), nil
	}
	after := ""
	for range maxReconcilePages {
		q := url.Values{"sort_order": {"desc"}, "page[size]": {"100"}}
		if after != "" {
			q.Set("page[after]", after)
		}
		var page auditPage
		if err := x.api.do(ctx, http.MethodGet, ticketPath(eff.Route.ID)+"/audits", q, nil, &page); err != nil {
			return adapter.Outcome{}, readFailure(err)
		}
		for _, a := range page.Audits {
			if a.Metadata == nil {
				continue
			}
			if m, ok := decodeMarker(a.Metadata.Custom); ok && m.of(eff) {
				return accepted(x.references(eff.Route.ID, &a)), nil
			}
		}
		if !page.Meta.HasMore || page.Meta.AfterCursor == "" {
			return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.Transient, "no zendesk audit holds the attempt")}, nil
		}
		after = page.Meta.AfterCursor
	}
	return adapter.Outcome{State: model.Unknown}, nil
}

func (x *instance) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if x.isClosed() {
		return errClosed()
	}
	return nil
}

func accepted(refs []model.ExternalRef) adapter.Outcome {
	return adapter.Outcome{State: model.ProviderAccepted, References: refs}
}

func rejected(code model.ErrorCode, msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Rejected, Failure: &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}}
}

// failed maps a failed update: answers proving nothing applied that are
// final become rejected outcomes; everything else is returned as a typed
// error for the engine to classify.
func failed(err error) (adapter.Outcome, error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		return adapter.Outcome{}, err
	}
	switch ae.err.Code {
	case model.Permanent, model.NotFound:
		return adapter.Outcome{State: model.Rejected, Failure: ae.err}, nil
	}
	return adapter.Outcome{}, ae.err
}

// readFailure maps a failed read. A read changes nothing, so an unknown
// outcome is just unavailability.
func readFailure(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	f := asFailure(err)
	if f.Code == model.OutcomeUnknown {
		return model.NewError(model.Unavailable, "zendesk read failed")
	}
	return f
}

func object(data json.RawMessage) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return nil, errors.New("not an object")
	}
	return m, nil
}

func hasText(raw json.RawMessage) bool {
	var s string
	return raw != nil && json.Unmarshal(raw, &s) == nil && s != ""
}
