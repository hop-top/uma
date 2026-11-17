package teams

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"regexp"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// callKind is what a provider call does: a write that changes a
// conversation (a message), or opening a personal chat, a write that is
// safe to repeat (the Connector returns the existing conversation).
type callKind int

const (
	writeCall callKind = iota
	openCall
	signalCall
)

// codeToken reports whether an error code is a plain token, safe to put in
// an error message.
var codeToken = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func rateLimited(wait time.Duration) *model.Error {
	if wait == 0 {
		wait = defaultRateWait
	}
	return &model.Error{Code: model.RateLimited, SafeMessage: "Teams rate limit", Retry: model.RetrySameOperation, RetryAfter: wait}
}

// rejected is a final, definite refusal of the effect.
func rejected(code model.ErrorCode, msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Rejected, Failure: final(code, msg)}
}

// failure maps a failed provider call. A message write changes a
// conversation, so an ambiguous failure of one is outcome unknown: the
// Connector takes no idempotency key and the adapter has no lookup, so the
// engine never sends it again blindly. A definite refusal is an Outcome
// with state rejected; every other failure is a *model.Error.
func failure(ctx context.Context, err error, kind callKind) (adapter.Outcome, error) {
	if me, ok := errors.AsType[*model.Error](err); ok {
		return adapter.Outcome{}, me
	}
	if ctx.Err() != nil {
		return adapter.Outcome{}, ctx.Err()
	}
	if se, ok := errors.AsType[*statusError](err); ok {
		return statusFailure(se, kind)
	}
	if kind == writeCall && !dialFailure(err) {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Bot Connector answer lost; the message may have been applied")
	}
	return adapter.Outcome{}, model.NewError(model.Transient, "Bot Connector unreachable")
}

// statusFailure maps the status of a Connector answer
// (https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-api-reference#http-status-codes).
func statusFailure(se *statusError, kind callKind) (adapter.Outcome, error) {
	detail := ""
	if codeToken.MatchString(se.code) {
		detail = ": " + se.code
	}
	switch {
	case se.status == http.StatusTooManyRequests:
		return adapter.Outcome{}, rateLimited(se.retryAfter)
	case se.status == http.StatusUnauthorized:
		return adapter.Outcome{}, final(model.Unauthorized, "the Bot Connector refused the credential")
	case se.status == http.StatusForbidden:
		return rejected(model.Unauthorized, "the Bot Connector denied the operation"+detail), nil
	case se.status == http.StatusNotFound || se.status == http.StatusGone:
		return rejected(model.NotFound, "conversation or message not found"+detail), nil
	case se.status == http.StatusBadRequest || se.status == http.StatusRequestEntityTooLarge:
		return rejected(model.Validation, "the Bot Connector refused the request"+detail), nil
	case se.status >= 500 && kind == writeCall:
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Bot Connector server error; the message may have been applied")
	case se.status >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, "Bot Connector server error")
	}
	return rejected(model.Permanent, "unexpected Bot Connector answer"+detail), nil
}

// dialFailure reports a connection that was never established, by a
// failed dial or TLS handshake: the request provably never reached the
// Connector.
func dialFailure(err error) bool {
	if op, ok := errors.AsType[*net.OpError](err); ok && op.Op == "dial" {
		return true
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return true
	}
	_, ok := errors.AsType[x509.CertificateInvalidError](err)
	return ok
}
