package telegram

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// callKind is what a provider call does: a read, or a write that changes
// the chat (a message, a reaction). A write whose answer was lost may have
// been applied, and the Bot API has no call to look it up.
type callKind int

const (
	readCall callKind = iota
	writeCall
)

func rateLimited(wait time.Duration) *model.Error {
	if wait <= 0 {
		wait = defaultRateWait
	}
	return &model.Error{Code: model.RateLimited, SafeMessage: "Telegram flood control", Retry: model.RetrySameOperation, RetryAfter: wait}
}

// rejected is a final, definite refusal of the effect.
func rejected(code model.ErrorCode, msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Rejected, Failure: final(code, msg)}
}

// failure maps a failed provider call. A write changes the chat, so an
// ambiguous failure of one is outcome unknown: Telegram has no idempotency
// key and no lookup, so the adapter cannot tell whether it applied and the
// engine never repeats it on its own. A read is simply retried. A definite
// refusal of an effect is an Outcome with state rejected; every other
// failure is a *model.Error.
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
	if ae, ok := errors.AsType[*apiError](err); ok {
		return apiFailure(ae, kind)
	}
	if kind == writeCall && !dialFailure(err) {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Telegram answer lost; the message may have been applied")
	}
	return adapter.Outcome{}, model.NewError(model.Transient, "Telegram unreachable")
}

func statusFailure(se *statusError, kind callKind) (adapter.Outcome, error) {
	switch {
	case se.status == http.StatusTooManyRequests:
		return adapter.Outcome{}, rateLimited(0)
	case se.status >= 500 && kind == writeCall:
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Telegram server error; the message may have been applied")
	case se.status >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, "Telegram server error")
	}
	return adapter.Outcome{}, final(model.Permanent, "unexpected Telegram answer")
}

// forbidden is a 403 description the adapter may show: "Forbidden: bot was
// blocked by the user", "Forbidden: bot can't initiate conversation with a
// user", "Forbidden: bot was kicked from the group chat". Descriptions are
// undocumented and never hold user content on a 403, but only plain words
// are echoed.
var forbidden = regexp.MustCompile(`^Forbidden: [A-Za-z' ]{1,120}$`)

// apiFailure maps a Bot API error object by its code, and for a 400 by what
// its description says. For a read a refusal is the call's error; for a write
// a definite refusal of the effect is a rejected outcome.
func apiFailure(e *apiError, kind callKind) (adapter.Outcome, error) {
	refuse := func(c model.ErrorCode, msg string) (adapter.Outcome, error) {
		if kind == readCall {
			return adapter.Outcome{}, final(c, msg)
		}
		return rejected(c, msg), nil
	}
	switch {
	case e.code == http.StatusUnauthorized || e.code == http.StatusNotFound:
		// A bad or revoked token is 401; a token that names no bot reads as
		// an unknown method path, 404.
		return adapter.Outcome{}, final(model.Unauthorized, "Telegram refused the bot token")
	case e.code == http.StatusTooManyRequests:
		return adapter.Outcome{}, rateLimited(e.retryAfter)
	case e.code == http.StatusConflict:
		return adapter.Outcome{}, final(model.Conflict, "Telegram holds a webhook for this bot; getUpdates and a webhook exclude each other")
	case e.code >= 500 && kind == writeCall:
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Telegram failed; the message may have been applied")
	case e.code >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, "Telegram failed")
	case e.code == http.StatusForbidden:
		msg := "Telegram forbade it"
		if forbidden.MatchString(e.description) {
			msg = "Telegram forbade it: " + strings.TrimPrefix(e.description, "Forbidden: ")
		}
		return refuse(model.Unauthorized, msg)
	case e.code == http.StatusBadRequest:
		code, msg := badRequest(e)
		return refuse(code, msg)
	}
	return refuse(model.Permanent, "Telegram refused the request")
}

// badRequest tells apart what a 400 covers, from the description Telegram
// gives. The phrases are what the Bot API answers in practice; the reference
// documents none of them, so a phrase it changes degrades to the generic
// validation refusal, never to a retry.
func badRequest(e *apiError) (model.ErrorCode, string) {
	d := strings.ToLower(e.description)
	has := func(s ...string) bool {
		for _, p := range s {
			if strings.Contains(d, p) {
				return true
			}
		}
		return false
	}
	switch {
	case e.migratedTo != 0:
		return model.NotFound, "the group migrated to a supergroup; its chat ID changed"
	case has("chat not found", "user not found", "peer_id_invalid", "message to edit not found", "message to delete not found", "message to react not found", "message not found", "message to be replied not found"):
		return model.NotFound, "target not found or inaccessible"
	case has("can't be edited", "can't be deleted", "not enough rights", "have no rights", "administrator rights", "not allowed", "can't edit", "can't delete"):
		return model.Unauthorized, "Telegram does not let the bot change this message"
	case has("can't parse entities"):
		return model.Validation, "Telegram could not parse the message's markup"
	case has("too long"):
		return model.Validation, "Telegram refused the text as too long"
	case has("reaction_invalid", "reaction_empty"):
		return model.Validation, "Telegram refused the reaction"
	case has("query is too old", "query id is invalid"):
		return model.DeadlineExpired, "Telegram no longer takes an answer to this callback query"
	}
	return model.Validation, "Telegram refused the request"
}

// dialFailure reports a connection that was never established, by a
// failed dial or TLS handshake: the request provably never reached Telegram.
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
