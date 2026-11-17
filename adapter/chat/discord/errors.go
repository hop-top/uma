package discord

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// callKind is what a provider call does: a read, or a write that changes
// what Discord holds (a message, a reaction, a membership).
type callKind int

const (
	readCall callKind = iota
	writeCall
	// openCall opens a direct message channel: a write Discord answers with
	// the existing channel when there is one, so it is safe to repeat. A
	// refusal is the effect's; any other failure is retried.
	openCall
)

// JSON error codes ("JSON Error Codes",
// https://docs.discord.com/developers/topics/opcodes-and-status-codes) the
// adapter tells apart; any other refusal is mapped by its HTTP status.
const (
	codeUnknownChannel     = 10003
	codeUnknownMessage     = 10008
	codeUnknownUser        = 10013
	codeUnknownEmoji       = 10014
	codeUnknownWebhook     = 10015
	codeUnknownInteraction = 10062
	codeMissingAccess      = 50001
	codeEditOthers         = 50005
	codeEmptyMessage       = 50006
	codeCannotDMUser       = 50007
	codeMissingPermissions = 50013
	codeInvalidFormBody    = 50035
	codeArchivedThread     = 50083
	codeAlreadyAcked       = 40060
	codeReactionBlocked    = 90001
)

// rejected is a final, definite refusal of the effect.
func rejected(code model.ErrorCode, msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Rejected, Failure: final(code, msg)}
}

// failure maps a failed provider call. A write changes what Discord holds,
// so an ambiguous failure of one is outcome unknown, reconciled before any
// retry; a read is simply retried. A definite refusal of an effect is an
// Outcome with state rejected; every other failure is a *model.Error.
func failure(ctx context.Context, err error, kind callKind) (adapter.Outcome, error) {
	if me, ok := errors.AsType[*model.Error](err); ok {
		return adapter.Outcome{}, me
	}
	if ctx.Err() != nil {
		return adapter.Outcome{}, ctx.Err()
	}
	if ae, ok := errors.AsType[*apiError](err); ok {
		return statusFailure(ae, kind)
	}
	if kind == writeCall && !dialFailure(err) {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Discord answer lost; reconcile before retrying")
	}
	return adapter.Outcome{}, model.NewError(model.Transient, "Discord unreachable")
}

// statusFailure maps an answer other than 2xx. For a read, a refusal is the
// call's error; for a write, a definite refusal is a rejected outcome.
func statusFailure(ae *apiError, kind callKind) (adapter.Outcome, error) {
	refuse := func(c model.ErrorCode, msg string) (adapter.Outcome, error) {
		if kind == readCall {
			return adapter.Outcome{}, final(c, msg)
		}
		return rejected(c, msg), nil
	}
	code := ""
	if ae.code != 0 {
		code = " (code " + strconv.Itoa(ae.code) + ")"
	}
	switch {
	case ae.status == http.StatusUnauthorized:
		return adapter.Outcome{}, final(model.Unauthorized, "Discord refused the bot token")
	case ae.status >= 500 && kind == writeCall:
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Discord server error; reconcile before retrying")
	case ae.status >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, "Discord server error")
	case ae.code == codeUnknownEmoji:
		return refuse(model.Unsupported, "Discord has no such emoji"+code)
	case ae.status == http.StatusNotFound || ae.code == codeUnknownChannel || ae.code == codeUnknownMessage ||
		ae.code == codeUnknownUser || ae.code == codeUnknownWebhook || ae.code == codeUnknownInteraction:
		return refuse(model.NotFound, "target not found or inaccessible"+code)
	case ae.status == http.StatusForbidden || ae.code == codeEditOthers || ae.code == codeCannotDMUser ||
		ae.code == codeMissingAccess || ae.code == codeMissingPermissions || ae.code == codeArchivedThread || ae.code == codeReactionBlocked:
		return refuse(model.Unauthorized, "Discord denied the operation"+code)
	case ae.status == http.StatusBadRequest || ae.code == codeInvalidFormBody || ae.code == codeEmptyMessage:
		return refuse(model.Validation, "Discord refused the request"+code)
	}
	return refuse(model.Permanent, "unexpected Discord answer"+code)
}

// dialFailure reports a connection that was never established, by a
// failed dial or TLS handshake: the request provably never reached Discord.
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

// isCode reports whether err is a Discord answer of the JSON error code.
func isCode(err error, code int) bool {
	ae, ok := errors.AsType[*apiError](err)
	return ok && ae.code == code
}

// isStatus reports whether err is a Discord answer of the HTTP status.
func isStatus(err error, status int) bool {
	ae, ok := errors.AsType[*apiError](err)
	return ok && ae.status == status
}
