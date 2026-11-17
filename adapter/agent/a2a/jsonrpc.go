package a2a

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"hop.top/uma/model"
)

// JSON-RPC and A2A error codes (A2A specification section 5.4, JSON-RPC 2.0).
const (
	codeParse             = -32700
	codeInvalidRequest    = -32600
	codeMethodNotFound    = -32601
	codeInvalidParams     = -32602
	codeInternal          = -32603
	codeTaskNotFound      = -32001
	codeTaskNotCancelable = -32002
	codePushUnsupported   = -32003
	codeUnsupportedOp     = -32004
	codeVersion           = -32009
)

// A2A-defined errors carry this ErrorInfo domain, whatever Options.ErrorDomain
// says.
const a2aDomain = "a2a-protocol.org"

const (
	typeErrorInfo  = "type.googleapis.com/google.rpc.ErrorInfo"
	typeBadRequest = "type.googleapis.com/google.rpc.BadRequest"
)

// request is a JSON-RPC 2.0 request with its id kept as sent.
type request struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// rpcError is a JSON-RPC error object. Data is the A2A detail array.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    []any  `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

type errorInfo struct {
	Type     string            `json:"@type"`
	Reason   string            `json:"reason"`
	Domain   string            `json:"domain"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type fieldViolation struct {
	Field       string `json:"field"`
	Description string `json:"description"`
}

type badRequest struct {
	Type       string           `json:"@type"`
	Violations []fieldViolation `json:"fieldViolations"`
}

// a2aError builds an error the A2A specification defines, with its
// ErrorInfo reason.
func a2aError(code int, message, reason string, meta map[string]string) *rpcError {
	return &rpcError{Code: code, Message: message, Data: []any{
		errorInfo{Type: typeErrorInfo, Reason: reason, Domain: a2aDomain, Metadata: meta},
	}}
}

func taskNotFound(taskID string) *rpcError {
	return a2aError(codeTaskNotFound, "Task not found", "TASK_NOT_FOUND", map[string]string{"taskId": taskID})
}

func unsupportedOperation(reason string) *rpcError {
	return a2aError(codeUnsupportedOp, "Unsupported operation", reason, nil)
}

// invalidParams is -32602 with the fields at fault.
func invalidParams(field, description string) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: "Invalid parameters", Data: []any{
		badRequest{Type: typeBadRequest, Violations: []fieldViolation{{Field: field, Description: description}}},
	}}
}

// umaError is an error uma defines, ErrorInfo domain d.
func umaError(d string, code int, message, reason string, meta map[string]string) *rpcError {
	return &rpcError{Code: code, Message: message, Data: []any{
		errorInfo{Type: typeErrorInfo, Reason: reason, Domain: d, Metadata: meta},
	}}
}

// internalError maps a uma error that has no A2A meaning to -32603 with
// its code as the reason. The message never carries detail, so a denial
// reveals nothing about what it denied.
func internalError(d string, err error) *rpcError {
	reason := strings.ToUpper(string(model.CodeOf(err)))
	if !model.CodeOf(err).Valid() {
		reason = "INTERNAL"
	}
	return umaError(d, codeInternal, "Internal error", reason, nil)
}

// parseRequest reads one JSON-RPC request. A batch, a wrong version or a
// request without a method is -32600; malformed JSON is -32700. The id is
// returned whenever it could be read, null otherwise.
func parseRequest(body []byte) (request, *rpcError) {
	body = bytes.TrimSpace(body)
	null := json.RawMessage("null")
	if !json.Valid(body) {
		return request{ID: null}, &rpcError{Code: codeParse, Message: "Invalid JSON payload"}
	}
	if len(body) > 0 && body[0] == '[' {
		return request{ID: null}, &rpcError{Code: codeInvalidRequest, Message: "Request payload validation error: batches are not supported"}
	}
	var raw struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return request{ID: null}, &rpcError{Code: codeInvalidRequest, Message: "Request payload validation error"}
	}
	id := null
	if len(raw.ID) > 0 && validID(raw.ID) {
		id = raw.ID
	}
	req := request{ID: id, Method: raw.Method, Params: raw.Params}
	if raw.JSONRPC != "2.0" || raw.Method == "" || len(raw.ID) == 0 || !validID(raw.ID) {
		return req, &rpcError{Code: codeInvalidRequest, Message: "Request payload validation error"}
	}
	return req, nil
}

// validID reports whether id is a JSON string, number or null.
func validID(id json.RawMessage) bool {
	id = bytes.TrimSpace(id)
	if len(id) == 0 {
		return false
	}
	switch id[0] {
	case '"':
		var s string
		return json.Unmarshal(id, &s) == nil
	case 'n':
		return string(id) == "null"
	case '{', '[', 't', 'f':
		return false
	}
	var n json.Number
	return json.Unmarshal(id, &n) == nil
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// writeResult answers HTTP 200 with the JSON-RPC result or error.
func writeResponse(w http.ResponseWriter, id json.RawMessage, result any, rerr *rpcError) {
	if id == nil {
		id = json.RawMessage("null")
	}
	body, err := json.Marshal(response{JSONRPC: "2.0", ID: id, Result: result, Error: rerr})
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// decodeParams reads params into v; absent or null params are an empty
// object. Unknown fields are ignored (forward compatibility).
func decodeParams(raw json.RawMessage, v any) *rpcError {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] != '{' {
		return invalidParams("params", "must be an object")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) {
			return invalidParams(ute.Field, "has the wrong type")
		}
		return invalidParams("params", "is malformed")
	}
	return nil
}
