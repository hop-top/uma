package fakeprovider_test

import (
	"errors"
	"fmt"
	"testing"

	"hop.top/uma/model"
)

// stoppedByOwnDrain reports whether err, a runtime worker's result after the
// test drained its own runtime, is a clean stop: none, or the refusal uma
// gives a Run or Receive that registers on a drained tenant. A drain can
// reach the runtime before the goroutine running the worker has registered;
// the late worker is then refused for good, which is the stop the test asked
// for. Any other error is a real failure.
func stoppedByOwnDrain(err error) bool {
	return err == nil || isDrainingRefusal(err)
}

// isDrainingRefusal matches exactly the refusal: Unavailable, not
// retryable, with the engine's message. A different Unavailable error is
// not a drain.
func isDrainingRefusal(err error) bool {
	var me *model.Error
	return errors.As(err, &me) && me.Code == model.Unavailable &&
		me.Retry == model.NoRetry && me.SafeMessage == "runtime draining"
}

func TestStoppedByOwnDrain(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"none", nil, true},
		{"draining refusal", &model.Error{Code: model.Unavailable, Retry: model.NoRetry, SafeMessage: "runtime draining"}, true},
		{"wrapped refusal", fmt.Errorf("run: %w", &model.Error{Code: model.Unavailable, Retry: model.NoRetry, SafeMessage: "runtime draining"}), true},
		{"unavailable, other message", &model.Error{Code: model.Unavailable, Retry: model.NoRetry, SafeMessage: "store down"}, false},
		{"unavailable, retryable", &model.Error{Code: model.Unavailable, Retry: model.RetrySameOperation, SafeMessage: "runtime draining"}, false},
		{"other code", &model.Error{Code: model.Permanent, Retry: model.NoRetry, SafeMessage: "runtime draining"}, false},
		{"plain error", errors.New("runtime draining"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := stoppedByOwnDrain(c.err); got != c.want {
				t.Fatalf("stoppedByOwnDrain(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
