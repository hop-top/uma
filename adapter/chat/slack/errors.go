package slack

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"regexp"
	"slices"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// callKind is what a provider call does: a read, a write that changes the
// workspace (a message, a reaction), or opening a direct message, a write
// that is safe to repeat.
type callKind int

const (
	readCall callKind = iota
	writeCall
	openCall
)

// Slack error codes by what they mean for the effect. The codes are those
// the methods' reference pages list (https://docs.slack.dev/reference/methods/);
// a code in none of the sets is refused as permanent.
var (
	// authCodes: the credential, not the effect, is refused.
	authCodes = []string{"not_authed", "invalid_auth", "token_expired", "token_revoked", "account_inactive"}
	// deniedCodes: the account may not do this to this target.
	deniedCodes = []string{
		"missing_scope", "no_permission", "not_in_channel", "restricted_action",
		"restricted_action_read_only_channel", "restricted_action_thread_only_channel",
		"restricted_action_non_threadable_channel", "restricted_action_thread_locked",
		"cant_update_message", "cant_delete_message", "edit_window_closed", "is_archived", "is_inactive",
		"posting_to_channel_denied", "not_allowed_token_type", "team_access_not_granted", "access_denied",
		"ekm_access_denied", "channel_is_limited_access", "thread_locked", "external_channel_migrating",
		"enterprise_is_restricted", "accesslimited", "org_login_required", "team_added_to_org",
		"two_factor_setup_required", "streaming_state_conflict",
	}
	// missingCodes: the target is not there for the account.
	missingCodes = []string{"channel_not_found", "message_not_found", "thread_not_found", "user_not_found", "invalid_channel_id"}
	// rateCodes: too many calls; Retry-After says when.
	rateCodes = []string{"ratelimited", "rate_limited"}
	// ambiguousCodes: Slack failed, and "part of the operation may have
	// succeeded" (reactions.add reference).
	ambiguousCodes = []string{"internal_error", "fatal_error", "service_unavailable", "request_timeout"}
	// invalidCodes: the request itself is refused.
	invalidCodes = []string{
		"no_text", "msg_too_long", "msg_blocks_too_long", "invalid_blocks", "invalid_blocks_format", "block_mismatch",
		"too_many_attachments", "invalid_attachments", "markdown_text_conflict", "invalid_arguments", "invalid_arg_name",
		"invalid_array_arg", "invalid_name", "bad_timestamp", "no_item_specified", "too_many_emoji", "too_many_reactions",
		"not_reactable", "cannot_reply_to_message", "cant_broadcast_message", "no_dual_broadcast_content_update",
		"invalid_metadata_format", "invalid_metadata_schema", "metadata_too_large", "metadata_must_be_sent_from_app",
		"invalid_ts_oldest", "invalid_ts_latest", "message_limit_exceeded",
	}
)

// codeToken reports whether a Slack code is a plain lowercase token, safe
// to put in an error message.
var codeToken = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func rateLimited(wait time.Duration) *model.Error {
	if wait == 0 {
		wait = defaultRateWait
	}
	return &model.Error{Code: model.RateLimited, SafeMessage: "Slack rate limit", Retry: model.RetrySameOperation, RetryAfter: wait}
}

// rejected is a final, definite refusal of the effect.
func rejected(code model.ErrorCode, msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Rejected, Failure: final(code, msg)}
}

// failure maps a failed provider call. A write changes the workspace, so an
// ambiguous failure of one is outcome unknown, reconciled before any retry;
// a read is simply retried. A definite refusal of an effect is an Outcome
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
	if ae, ok := errors.AsType[*apiError](err); ok {
		return apiFailure(ae.code, kind)
	}
	if kind == writeCall && !dialFailure(err) {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Slack answer lost; reconcile before retrying")
	}
	return adapter.Outcome{}, model.NewError(model.Transient, "Slack unreachable")
}

func statusFailure(se *statusError, kind callKind) (adapter.Outcome, error) {
	switch {
	case se.status == http.StatusTooManyRequests:
		return adapter.Outcome{}, rateLimited(se.retryAfter)
	case se.status >= 500 && kind == writeCall:
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Slack server error; reconcile before retrying")
	case se.status >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, "Slack server error")
	}
	return adapter.Outcome{}, final(model.Permanent, "unexpected Slack answer")
}

// apiFailure maps the code of an "ok": false answer. For a read, a refusal
// is the call's error; for a write or a direct message opening, a definite
// refusal of the effect is a rejected outcome.
func apiFailure(code string, kind callKind) (adapter.Outcome, error) {
	refuse := func(c model.ErrorCode, msg string) (adapter.Outcome, error) {
		if kind == readCall {
			return adapter.Outcome{}, final(c, msg)
		}
		return rejected(c, msg), nil
	}
	switch {
	case slices.Contains(authCodes, code):
		return adapter.Outcome{}, final(model.Unauthorized, "Slack refused the credential")
	case slices.Contains(rateCodes, code):
		return adapter.Outcome{}, rateLimited(0)
	case slices.Contains(ambiguousCodes, code):
		if kind == writeCall {
			return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Slack failed; reconcile before retrying")
		}
		return adapter.Outcome{}, model.NewError(model.Transient, "Slack failed")
	case slices.Contains(deniedCodes, code):
		return refuse(model.Unauthorized, "Slack denied the operation: "+code)
	case slices.Contains(missingCodes, code):
		return refuse(model.NotFound, "target not found or inaccessible: "+code)
	case slices.Contains(invalidCodes, code):
		return refuse(model.Validation, "Slack refused the request: "+code)
	case codeToken.MatchString(code):
		return refuse(model.Permanent, "Slack refused the request: "+code)
	}
	return refuse(model.Permanent, "Slack refused the request")
}

// dialFailure reports a connection that was never established, by a
// failed dial or TLS handshake: the request provably never reached Slack.
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
