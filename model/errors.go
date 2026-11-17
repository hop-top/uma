package model

import (
	"errors"
	"time"
)

// ErrorCode is the stable, branchable class of a public error. Callers branch
// on the code, never on message text.
type ErrorCode string

const (
	Validation      ErrorCode = "validation"
	NotFound        ErrorCode = "not_found"
	Unauthorized    ErrorCode = "unauthorized"
	Conflict        ErrorCode = "conflict"
	Unsupported     ErrorCode = "unsupported"
	Disabled        ErrorCode = "disabled"
	Unavailable     ErrorCode = "unavailable"
	RateLimited     ErrorCode = "rate_limited"
	Transient       ErrorCode = "transient"
	Permanent       ErrorCode = "permanent"
	OutcomeUnknown  ErrorCode = "outcome_unknown"
	CursorExpired   ErrorCode = "cursor_expired"
	SchemaMismatch  ErrorCode = "schema_mismatch"
	DeadlineExpired ErrorCode = "deadline_expired"
)

// Valid reports whether c is one of the declared error codes.
func (c ErrorCode) Valid() bool {
	switch c {
	case Validation, NotFound, Unauthorized, Conflict, Unsupported, Disabled, Unavailable,
		RateLimited, Transient, Permanent, OutcomeUnknown, CursorExpired, SchemaMismatch,
		DeadlineExpired:
		return true
	}
	return false
}

// DefaultRetry returns the retry advice that matches the caller action for c:
// correct input or fail closed (NoRetry); retry the same command, honouring
// RetryAfter (RetrySameOperation); or reconcile state before deciding again
// (ReconcileFirst). Unknown codes get NoRetry.
func (c ErrorCode) DefaultRetry() RetryAdvice {
	switch c {
	case Unavailable, RateLimited, Transient:
		return RetrySameOperation
	case Conflict, OutcomeUnknown, CursorExpired:
		return ReconcileFirst
	}
	return NoRetry
}

// RetryAdvice tells the caller whether and how a failed command may be retried.
type RetryAdvice string

const (
	NoRetry            RetryAdvice = "none"
	RetrySameOperation RetryAdvice = "same_operation"
	ReconcileFirst     RetryAdvice = "reconcile_first"
)

// Valid reports whether a is one of the declared retry advices.
func (a RetryAdvice) Valid() bool {
	switch a {
	case NoRetry, RetrySameOperation, ReconcileFirst:
		return true
	}
	return false
}

// ErrorReason is a stable sub-class of an ErrorCode, set when callers must
// tell apart failures that share a code. Callers branch on it, never on
// message text. A reason is optional; most errors carry none.
type ErrorReason string

const (
	// StaleTarget: a held operation's target changed since it was held.
	StaleTarget ErrorReason = "stale_target"
	// StaleRevision: the caller's expected revision of a held operation no
	// longer matches.
	StaleRevision ErrorReason = "stale_revision"
	// TooLarge: what was asked for is over the size bound of its read, and
	// the read refuses it whole rather than truncating (Messages.Raw).
	TooLarge ErrorReason = "too_large"
)

// Error is the single public error type. SafeMessage and Correlation may be
// logged; provider raw errors, secrets and content never appear in it.
// Reason refines Code and is empty when there is none. RetryAfter is set when the source states a wait, typically with RateLimited.
type Error struct {
	Code        ErrorCode     `json:"code"`
	SafeMessage string        `json:"safe_message"`
	Correlation CorrelationID `json:"correlation"`
	Retry       RetryAdvice   `json:"retry"`
	Reason      ErrorReason   `json:"reason"`
	RetryAfter  time.Duration `json:"retry_after_ms"`
}

// NewError returns an error with code, safe message and the code's default
// retry advice.
func NewError(code ErrorCode, safeMessage string) *Error {
	return &Error{Code: code, SafeMessage: safeMessage, Retry: code.DefaultRetry()}
}

// NewReasonError returns NewError(code, safeMessage) carrying reason.
func NewReasonError(code ErrorCode, reason ErrorReason, safeMessage string) *Error {
	e := NewError(code, safeMessage)
	e.Reason = reason
	return e
}

// Error returns the code and safe message only.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.SafeMessage == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.SafeMessage
}

// CodeOf returns the code of the first *Error in err's chain, or "" when there
// is none. Context cancellation and deadline errors stay standard and have no
// code.
func CodeOf(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsCode reports whether err's chain holds an *Error with code.
func IsCode(err error, code ErrorCode) bool {
	return err != nil && CodeOf(err) == code
}

// ReasonOf returns the reason of the first *Error in err's chain, or "" when
// there is none.
func ReasonOf(err error) ErrorReason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// IsReason reports whether the first *Error in err's chain carries reason.
func IsReason(err error, reason ErrorReason) bool {
	return reason != "" && ReasonOf(err) == reason
}

// invalidField returns a validation error naming field. Reasons are fixed text
// and never echo caller input.
func invalidField(field, reason string) *Error {
	return &Error{Code: Validation, SafeMessage: field + ": " + reason, Retry: NoRetry}
}

// fieldPath joins a parent field path and a child name.
func fieldPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}

// firstError returns the first non-nil error.
func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
