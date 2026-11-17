package runtime_test

import (
	"context"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

// reportedStatuses is a send instance reporting a fixed status list.
type reportedStatuses []model.CapabilityStatus

func (r reportedStatuses) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return r, nil
}
func (reportedStatuses) Send(context.Context, adapter.SendRequest) (adapter.Outcome, error) {
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}
func (reportedStatuses) Close(context.Context) error { return nil }

// The capability gate admits a declared capability only on exactly one
// explicit available status: a missing status is unavailable, and
// duplicate or contradictory statuses are invalid adapter output.
func TestCapabilityGateFailsClosed(t *testing.T) {
	send, edit := model.CapabilitySend, model.CapabilityEdit
	d := adapter.Descriptor{Capabilities: []model.Capability{send, edit}}
	avail := func(c model.Capability) model.CapabilityStatus {
		return model.CapabilityStatus{Name: c, Available: true}
	}
	cases := []struct {
		name     string
		statuses reportedStatuses
		code     model.ErrorCode
		message  string
	}{
		{"available", reportedStatuses{avail(send)}, "", ""},
		{"no status", nil, model.Unsupported, "send unavailable"},
		{"another capability only", reportedStatuses{avail(edit)}, model.Unsupported, "send unavailable"},
		{"reported twice", reportedStatuses{avail(send), avail(send)}, model.Permanent, "invalid capability statuses"},
		{"contradictory", reportedStatuses{avail(send), {Name: send, Reason: model.Unauthorized}}, model.Permanent, "invalid capability statuses"},
		{"available with a reason", reportedStatuses{{Name: send, Available: true, Reason: model.Disabled}}, model.Permanent, "invalid capability statuses"},
		{"another capability twice", reportedStatuses{avail(send), avail(edit), avail(edit)}, model.Permanent, "invalid capability statuses"},
		{"withdrawn", reportedStatuses{{Name: send, Reason: model.Unauthorized}}, model.Unauthorized, "withdrawn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := runtime.CapabilityReady(t.Context(), d, c.statuses, send)
			if c.code == "" {
				if f != nil {
					t.Fatalf("gate refused: %v", f)
				}
				return
			}
			if f == nil {
				t.Fatal("gate admitted send")
			}
			requireCode(t, f, c.code, c.message)
			if f.Retry != model.NoRetry {
				t.Fatalf("retry advice %s, want none", f.Retry)
			}
		})
	}
}
