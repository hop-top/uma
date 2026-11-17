package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"hop.top/uma"
	"hop.top/uma/model"
)

// operatorAPI is the operator view: JSON over HTTP on the public services,
// as the authenticated operator. Every read is an authorized query, so the
// policy decides what an operator sees; replies are queued operations and
// notes never leave the inbox.
//
//	GET  /operator/conversations[?inbox=ID&after=CURSOR&limit=N]
//	GET  /operator/inboxes/{inbox}/conversations/{conversation}/messages
//	POST /operator/inboxes/{inbox}/conversations/{conversation}/replies {"text": "...", "in_reply_to": "MESSAGE"}
//	POST /operator/inboxes/{inbox}/conversations/{conversation}/notes   {"text": "..."}
//	GET  /operator/operations/{operation}
//
// A reply naming the message it answers threads under it where the channel
// threads (email In-Reply-To and References). POSTs take an Idempotency-Key header as the operation ID: resending a
// command with the same key returns the first acceptance instead of
// replying twice.
func (h *Host) operatorAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /operator/conversations", h.listConversations)
	mux.HandleFunc("GET /operator/inboxes/{inbox}/conversations/{conversation}/messages", h.listMessages)
	mux.HandleFunc("POST /operator/inboxes/{inbox}/conversations/{conversation}/replies", h.reply)
	mux.HandleFunc("POST /operator/inboxes/{inbox}/conversations/{conversation}/notes", h.note)
	mux.HandleFunc("GET /operator/operations/{operation}", h.operation)
	return h.operatorAuth(mux)
}

// operatorAuth admits requests bearing the operator token. A real host
// authenticates its staff here and maps each to their own actor.
func (h *Host) operatorAuth(next http.Handler) http.Handler {
	want := []byte("Bearer " + h.cfg.OperatorToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="operator"`)
			writeError(w, model.NewError(model.Unauthorized, "operator token required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Host) operator() (uma.Client, error) {
	return h.engine.For(model.Principal{Tenant: h.cfg.Tenant, Actor: operatorActor})
}

func call(r *http.Request) model.Call {
	id := r.Header.Get("X-Request-Id")
	if id == "" {
		id = "operator"
	}
	return model.Call{Correlation: model.CorrelationID(id)}
}

func (h *Host) listConversations(w http.ResponseWriter, r *http.Request) {
	c, err := h.operator()
	if err != nil {
		writeError(w, err)
		return
	}
	req := model.ListRequest{Call: call(r), Page: page(r)}
	if id := r.URL.Query().Get("inbox"); id != "" {
		req.Filter.Inboxes = []model.InboxRef{{Tenant: h.cfg.Tenant, ID: model.ID(id)}}
	}
	p, err := c.Conversations().List(r.Context(), req)
	writeResult(w, http.StatusOK, p, err)
}

func (h *Host) listMessages(w http.ResponseWriter, r *http.Request) {
	c, err := h.operator()
	if err != nil {
		writeError(w, err)
		return
	}
	conv := h.conversation(r)
	p, err := c.Messages().List(r.Context(), model.ListRequest{
		Call: call(r), Filter: model.Filter{Conversation: &conv}, Page: page(r),
	})
	writeResult(w, http.StatusOK, p, err)
}

// commandBody is the JSON body of a reply or note.
type commandBody struct {
	Text      string   `json:"text"`
	InReplyTo model.ID `json:"in_reply_to,omitempty"`
}

func (h *Host) reply(w http.ResponseWriter, r *http.Request) {
	h.command(w, r, func(c uma.Client, cmd model.Command, conv model.ConversationRef, body commandBody) (model.Acceptance, error) {
		req := model.Reply{Command: cmd, Conversation: conv, Content: text(body.Text)}
		if body.InReplyTo != "" {
			req.InReplyTo = &model.MessageRef{Conversation: conv, ID: body.InReplyTo}
		}
		return c.Messages().Reply(r.Context(), req)
	})
}

func (h *Host) note(w http.ResponseWriter, r *http.Request) {
	h.command(w, r, func(c uma.Client, cmd model.Command, conv model.ConversationRef, body commandBody) (model.Acceptance, error) {
		if body.InReplyTo != "" {
			return model.Acceptance{}, model.NewError(model.Validation, "a note answers no message")
		}
		return c.Messages().AddInternalNote(r.Context(), model.InternalNote{Command: cmd, Conversation: conv, Content: text(body.Text)})
	})
}

func text(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

// command decodes a text body and runs one conversation command under the
// request's idempotency key, answering 202 with the acceptance.
func (h *Host) command(w http.ResponseWriter, r *http.Request,
	run func(uma.Client, model.Command, model.ConversationRef, commandBody) (model.Acceptance, error)) {
	c, err := h.operator()
	if err != nil {
		writeError(w, err)
		return
	}
	var body commandBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		writeError(w, model.NewError(model.Validation, `body must be {"text": "..."}`))
		return
	}
	op := model.OperationID(r.Header.Get("Idempotency-Key"))
	if op == "" {
		if op, err = h.newOperation(r.Context()); err != nil {
			writeError(w, err)
			return
		}
	}
	acc, err := run(c, model.Command{Call: call(r), Operation: op}, h.conversation(r), body)
	writeResult(w, http.StatusAccepted, acc, err)
}

func (h *Host) operation(w http.ResponseWriter, r *http.Request) {
	c, err := h.operator()
	if err != nil {
		writeError(w, err)
		return
	}
	op, err := c.Operations().Get(r.Context(), model.Lookup[model.OperationRef]{
		Call: call(r), Ref: model.OperationRef{Tenant: h.cfg.Tenant, ID: model.ID(r.PathValue("operation"))},
	})
	writeResult(w, http.StatusOK, op, err)
}

func (h *Host) conversation(r *http.Request) model.ConversationRef {
	return model.ConversationRef{
		Inbox: model.InboxRef{Tenant: h.cfg.Tenant, ID: model.ID(r.PathValue("inbox"))},
		ID:    model.ID(r.PathValue("conversation")),
	}
}

func page(r *http.Request) model.PageRequest {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit")) // zero takes the default page size
	return model.PageRequest{Limit: limit, After: model.Cursor(q.Get("after"))}
}

func writeResult(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, v)
}

// writeError answers with uma's error code and safe message only; the
// error's other detail stays in the process.
func writeError(w http.ResponseWriter, err error) {
	code := model.CodeOf(err)
	msg := "request failed"
	var me *model.Error
	if errors.As(err, &me) && me.SafeMessage != "" {
		msg = me.SafeMessage
	}
	writeJSON(w, httpStatus(code), map[string]string{"error": string(code), "message": msg})
}

func httpStatus(code model.ErrorCode) int {
	switch code {
	case model.Validation:
		return http.StatusBadRequest
	case model.Unauthorized:
		return http.StatusUnauthorized
	case model.NotFound:
		return http.StatusNotFound
	case model.Conflict:
		return http.StatusConflict
	case model.Unsupported:
		return http.StatusNotImplemented
	case model.RateLimited:
		return http.StatusTooManyRequests
	case model.CursorExpired:
		return http.StatusGone
	case model.Unavailable, model.Transient:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
