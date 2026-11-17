package runtime

import (
	"context"
	"errors"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
)

// openInbound opens an instance of the sink's channel for a webhook binding
// or a receiver run. It goes through the same Open path as outbound
// execution, so an instance that does not match its descriptor fails here,
// and then requires capability c to be declared and currently available. An
// undeclared capability is refused before any I/O.
func openInbound(ctx context.Context, e *engine.Engine, s *ingress.Sink, c model.Capability) (adapter.Instance, error) {
	if err := declares(s.Descriptor(), c); err != nil {
		return nil, err
	}
	inst, desc, _, err := OpenOutbound(ctx, e, s.Channel())
	if err != nil {
		return nil, safeAdapterError(ctx, err, "adapter instance unavailable")
	}
	if me := capabilityReady(ctx, desc, inst, c); me != nil {
		_ = inst.Close(context.WithoutCancel(ctx))
		return nil, me
	}
	return inst, nil
}

// declares requires the descriptor to declare c.
func declares(d adapter.Descriptor, c model.Capability) error {
	if slices.Contains(d.Capabilities, c) {
		return nil
	}
	return &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter does not declare " + string(c), Retry: model.NoRetry}
}

// safeAdapterError keeps context errors and typed *model.Error values,
// whose safe message is the adapter's to choose, and replaces anything else
// with an unavailable error carrying fallback, so provider internals never
// leave the engine.
func safeAdapterError(ctx context.Context, err error, fallback string) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	var me *model.Error
	if errors.As(err, &me) && me.Code.Valid() {
		return cloneError(me)
	}
	return model.NewError(model.Unavailable, fallback)
}
