package twilio_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
)

var ch = model.ChannelRef{Tenant: "t1", ID: "00000000-0000-7000-8000-000000000001"}

func TestSendOutcomeOfMessages(t *testing.T) {
	code := 30007
	for _, c := range []struct {
		status string
		want   model.DeliveryState
	}{
		{"queued", model.ProviderAccepted}, {"accepted", model.ProviderAccepted}, {"scheduled", model.ProviderAccepted},
		{"sending", model.ProviderAccepted}, {"sent", model.ProviderAccepted}, {"delivered", model.Delivered},
		{"read", model.Delivered}, {"failed", model.Failed}, {"undelivered", model.Failed}, {"canceled", model.Failed},
		{"some-future-status", model.ProviderAccepted},
	} {
		out, err := twilio.SendOutcome(ch, twilio.Message{SID: messageSID, Status: c.status, ErrorCode: &code}, nil)
		if err != nil || out.State != c.want {
			t.Errorf("%s: %+v, %v; want %s", c.status, out, err, c.want)
			continue
		}
		if len(out.References) != 1 || out.References[0] != model.ProviderMessageRef(ch, messageSID) {
			t.Errorf("%s: references %v", c.status, out.References)
		}
		if (out.Failure != nil) != (c.want == model.Failed) {
			t.Errorf("%s: failure %v", c.status, out.Failure)
		}
	}
	if _, err := twilio.SendOutcome(ch, twilio.Message{Status: "queued"}, nil); model.CodeOf(err) != model.OutcomeUnknown {
		t.Errorf("answer without SID = %v, want outcome_unknown", err)
	}
}

func TestSendOutcomeOfErrors(t *testing.T) {
	for _, c := range []struct {
		name      string
		err       error
		state     model.DeliveryState
		code      model.ErrorCode
		retry     model.RetryAdvice
		wantAfter time.Duration
	}{
		{"429 with Retry-After", &twilio.APIError{Status: 429, Code: 20429, RetryAfter: 10 * time.Minute}, "", model.RateLimited, model.RetrySameOperation, 10 * time.Minute},
		{"20429 code", &twilio.APIError{Status: 400, Code: 20429}, "", model.RateLimited, model.RetrySameOperation, 0},
		{"500", &twilio.APIError{Status: 500}, "", model.OutcomeUnknown, model.ReconcileFirst, 0},
		{"502", &twilio.APIError{Status: 502}, "", model.OutcomeUnknown, model.ReconcileFirst, 0},
		{"503", &twilio.APIError{Status: 503, Code: 20503}, "", model.OutcomeUnknown, model.ReconcileFirst, 0},
		{"504", &twilio.APIError{Status: 504}, "", model.OutcomeUnknown, model.ReconcileFirst, 0},
		{"401", &twilio.APIError{Status: 401, Code: 20003}, "", model.Unauthorized, model.NoRetry, 0},
		{"403", &twilio.APIError{Status: 403}, "", model.Unauthorized, model.NoRetry, 0},
		{"400 invalid To", &twilio.APIError{Status: 400, Code: 21211}, model.Rejected, model.Permanent, model.NoRetry, 0},
		{"400 unsubscribed", &twilio.APIError{Status: 400, Code: 21610}, model.Rejected, model.Permanent, model.NoRetry, 0},
		{"404", &twilio.APIError{Status: 404, Code: 20404}, "", model.Permanent, model.NoRetry, 0},
		{"not sent", &twilio.RequestError{Err: errors.New("dial")}, "", model.Transient, model.RetrySameOperation, 0},
		{"timeout after send", &twilio.RequestError{Sent: true, Err: context.DeadlineExceeded}, "", model.OutcomeUnknown, model.ReconcileFirst, 0},
		{"reset after send", &twilio.RequestError{Sent: true, Err: errors.New("EOF")}, "", model.OutcomeUnknown, model.ReconcileFirst, 0},
		{"model error passes", model.NewError(model.Unsupported, "kind"), "", model.Unsupported, model.NoRetry, 0},
		{"unknown error", errors.New("?"), "", model.OutcomeUnknown, model.ReconcileFirst, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := twilio.SendOutcome(ch, twilio.Message{}, c.err)
			f := out.Failure
			if c.state == "" {
				var me *model.Error
				if !errors.As(err, &me) {
					t.Fatalf("outcome %+v, error %v; want a model error", out, err)
				}
				f = me
			} else if err != nil || out.State != c.state {
				t.Fatalf("outcome %+v, %v; want state %s", out, err, c.state)
			}
			if f == nil || f.Code != c.code || f.Retry != c.retry || f.RetryAfter != c.wantAfter {
				t.Fatalf("failure %+v, want %s/%s after %v", f, c.code, c.retry, c.wantAfter)
			}
		})
	}
}
