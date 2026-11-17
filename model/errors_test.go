package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var allErrorCodes = []ErrorCode{
	Validation, SchemaMismatch, Unauthorized, NotFound, Conflict, Unsupported, Disabled,
	Unavailable, RateLimited, Transient, Permanent, OutcomeUnknown, CursorExpired, DeadlineExpired,
}

func TestErrorCodeWireValues(t *testing.T) {
	want := []string{
		"validation", "schema_mismatch", "unauthorized", "not_found", "conflict", "unsupported",
		"disabled", "unavailable", "rate_limited", "transient", "permanent", "outcome_unknown",
		"cursor_expired", "deadline_expired",
	}
	for i, c := range allErrorCodes {
		if string(c) != want[i] {
			t.Fatalf("code %d = %q, want %q", i, c, want[i])
		}
		if !c.Valid() {
			t.Fatalf("%q not Valid", c)
		}
	}
	for _, c := range []ErrorCode{"", "Validation", "internal"} {
		if c.Valid() {
			t.Fatalf("%q reported Valid", c)
		}
	}
}

func TestDefaultRetry(t *testing.T) {
	want := map[ErrorCode]RetryAdvice{
		Validation:      NoRetry,
		SchemaMismatch:  NoRetry,
		Unauthorized:    NoRetry,
		NotFound:        NoRetry,
		Conflict:        ReconcileFirst,
		Unsupported:     NoRetry,
		Disabled:        NoRetry,
		Unavailable:     RetrySameOperation,
		RateLimited:     RetrySameOperation,
		Transient:       RetrySameOperation,
		Permanent:       NoRetry,
		OutcomeUnknown:  ReconcileFirst,
		CursorExpired:   ReconcileFirst,
		DeadlineExpired: NoRetry,
		"bogus":         NoRetry,
	}
	if len(want) != len(allErrorCodes)+1 {
		t.Fatal("retry table does not cover every code")
	}
	for code, advice := range want {
		if got := code.DefaultRetry(); got != advice {
			t.Errorf("%q.DefaultRetry() = %q, want %q", code, got, advice)
		}
		if !advice.Valid() {
			t.Errorf("advice %q not Valid", advice)
		}
	}
	if RetryAdvice("").Valid() || RetryAdvice("always").Valid() {
		t.Fatal("unknown retry advice reported Valid")
	}
}

func TestNewErrorAndAs(t *testing.T) {
	for _, code := range allErrorCodes {
		t.Run(string(code), func(t *testing.T) {
			e := NewError(code, "safe detail")
			if e.Code != code || e.Retry != code.DefaultRetry() || e.SafeMessage != "safe detail" {
				t.Fatalf("NewError = %+v", e)
			}
			wrapped := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", e))
			var got *Error
			if !errors.As(wrapped, &got) || got != e {
				t.Fatal("errors.As did not find *model.Error through wrapping")
			}
			if CodeOf(wrapped) != code {
				t.Fatalf("CodeOf = %q", CodeOf(wrapped))
			}
			if !IsCode(wrapped, code) {
				t.Fatal("IsCode = false")
			}
		})
	}
	if CodeOf(errors.New("plain")) != "" || CodeOf(nil) != "" || IsCode(nil, NotFound) {
		t.Fatal("CodeOf/IsCode matched a non-model error")
	}
	if CodeOf(context.Canceled) != "" {
		t.Fatal("context errors must stay standard")
	}
}

func TestErrorString(t *testing.T) {
	cases := []struct {
		err  *Error
		want string
	}{
		{&Error{Code: NotFound, SafeMessage: "conversation not found"}, "not_found: conversation not found"},
		{&Error{Code: Transient}, "transient"},
		{nil, "<nil>"},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
	e := &Error{Code: RateLimited, SafeMessage: "slow down", Correlation: "c-1", RetryAfter: 3 * time.Second}
	if got := e.Error(); got != "rate_limited: slow down" {
		t.Fatalf("Error() leaks fields beyond code and safe message: %q", got)
	}
}

func TestRateLimitedRetryAfter(t *testing.T) {
	e := NewError(RateLimited, "provider throttled")
	e.RetryAfter = 2 * time.Second
	var got *Error
	if !errors.As(fmt.Errorf("send: %w", e), &got) || got.RetryAfter != 2*time.Second || got.Retry != RetrySameOperation {
		t.Fatalf("retry-after lost: %+v", got)
	}
}

func TestErrorReason(t *testing.T) {
	e := NewReasonError(Conflict, StaleTarget, "x")
	if e.Code != Conflict || e.Reason != StaleTarget || e.Retry != ReconcileFirst {
		t.Fatalf("NewReasonError = %+v", e)
	}
	wrapped := fmt.Errorf("wrap: %w", e)
	if !IsReason(wrapped, StaleTarget) || ReasonOf(wrapped) != StaleTarget {
		t.Fatal("reason not found through wrap")
	}
	if !IsCode(wrapped, Conflict) {
		t.Fatal("code lost")
	}
	for _, err := range []error{nil, NewError(Conflict, "x"), errors.New("x"), NewReasonError(Conflict, StaleRevision, "x")} {
		if IsReason(err, StaleTarget) {
			t.Fatalf("IsReason(%v, StaleTarget) = true", err)
		}
	}
	if ReasonOf(nil) != "" || ReasonOf(NewError(Conflict, "x")) != "" {
		t.Fatal("ReasonOf should be empty without a reason")
	}
	if string(StaleTarget) != "stale_target" || string(StaleRevision) != "stale_revision" {
		t.Fatal("reason wire values changed")
	}
}
