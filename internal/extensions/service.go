package extensions

import (
	"context"
	"errors"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Service is the Extensions service of one client.
type Service struct{ c *engine.Client }

// New returns the Extensions service bound to c.
func New(c *engine.Client) Service { return Service{c} }

// Query runs a registered read on target's channel and returns its result.
// The read is authorized under its definition's policy action with the
// request's audience, which the adapter also receives. The adapter must
// declare extension_query and the definition's capability, and report both
// available. The result must carry the definition's result schema name and
// version and valid JSON within the structured payload limit; anything else
// is rejected, never passed through. Nothing is stored.
func (s Service) Query(ctx context.Context, req model.QueryExtension) (model.Extension, error) {
	e := s.c.Engine()
	if e.IsClosed() {
		return model.Extension{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Extension{}, err
	}
	d, err := definition(e, req.Query, model.ExtensionKindRead)
	if err != nil {
		return model.Extension{}, err
	}
	caps := capabilities(model.CapabilityExtensionQuery, d)
	t, err := s.target(ctx, req.Target, caps)
	if err != nil {
		return model.Extension{}, err
	}
	err = s.c.Authorize(ctx, policy.Authorization{
		Action: d.AuthorizeAs, Resource: req.Target, Audience: req.Audience,
	}, t.owner)
	if err != nil {
		return model.Extension{}, err
	}
	done, err := e.Begin()
	if err != nil {
		return model.Extension{}, err
	}
	defer done()
	out, err := read(ctx, e, t.channel, caps, cloneQuery(req))
	if err != nil {
		return model.Extension{}, err
	}
	return checkResult(d, out)
}

// read opens an instance of ch's adapter, checks caps are available and
// runs the query on it.
func read(ctx context.Context, e *engine.Engine, ch model.Channel, caps []model.Capability, req model.QueryExtension) (model.Extension, error) {
	inst, desc, _, err := runtime.OpenOutbound(ctx, e, ch)
	if err != nil {
		return model.Extension{}, adapterError(ctx, err)
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }()
	for _, c := range caps {
		if f := runtime.CapabilityReady(ctx, desc, inst, c); f != nil {
			return model.Extension{}, f
		}
	}
	reader, ok := inst.(adapter.ExtensionReader)
	if !ok {
		return model.Extension{}, &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter cannot query", Retry: model.NoRetry}
	}
	out, err := reader.Query(ctx, req)
	if err != nil {
		return model.Extension{}, adapterError(ctx, err)
	}
	return out.Clone(), nil
}

// checkResult admits a read result only under the declared result schema.
func checkResult(d adapter.ExtensionDefinition, out model.Extension) (model.Extension, error) {
	name, version := resultSchema(d)
	if out.Name != name || out.Version != version {
		return model.Extension{}, mismatch("extension result does not match the declared result schema")
	}
	if err := out.Validate(); err != nil {
		return model.Extension{}, &model.Error{Code: model.Permanent, SafeMessage: "extension result is invalid", Retry: model.NoRetry}
	}
	return out, nil
}

// Execute queues a registered effect on target's channel. It is a
// mutation: the operation key is (tenant, caller, policy action, target,
// operation ID), the fingerprint covers target, audience, action and
// expected revision, and the intent commits with pending work before any
// provider call; Runtime.Run executes it, rechecking the definition,
// policy and capabilities as the originating actor. Retrying with the same
// operation ID and payload returns the stored operation with Replayed set;
// a different payload conflicts. An expected revision, when set, must
// match the target's current revision.
//
// Core refuses an action that is unregistered, of another version or kind,
// or whose data is not valid JSON within the payload limit, before anything
// is stored. The action's fields are the adapter's (adapter.ExtensionExecutor):
// an adapter implementing adapter.ActionValidator refuses them here, a
// model.Validation error with nothing stored; any adapter validates them
// when it executes the stored effect.
func (s Service) Execute(ctx context.Context, req model.ExecuteExtension) (model.Acceptance, error) {
	e := s.c.Engine()
	if e.IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	d, err := definition(e, req.Action, model.ExtensionKindEffect)
	if err != nil {
		return model.Acceptance{}, err
	}
	t, err := s.target(ctx, req.Target, capabilities(model.CapabilityExtensionExecute, d))
	if err != nil {
		return model.Acceptance{}, err
	}
	if err := validateAction(e, t.channel.Type, req.Audience, req.Action); err != nil {
		return model.Acceptance{}, err
	}
	stored, err := intent{Target: req.Target, Audience: req.Audience, Action: req.Action.Clone()}.encode()
	if err != nil {
		return model.Acceptance{}, err
	}
	expected := req.ExpectedRevision
	return operations.Submit(ctx, s.c, operations.Submission{
		Kind: d.AuthorizeAs, Target: req.Target, Owner: t.owner, Command: req.Command, Audience: req.Audience,
		Fingerprint: fingerprint(d.AuthorizeAs, req), Intent: stored,
		Precheck: func(ctx context.Context, tx store.Tx) error {
			if expected == "" {
				return nil
			}
			cur, err := resolve(ctx, tx, req.Target)
			if err != nil {
				return err
			}
			if cur.revision != expected {
				return model.NewError(model.Conflict, "target revision changed")
			}
			return nil
		},
	})
}

// target resolves ref in the client's tenant and checks its channel's
// adapter declares caps. A foreign or missing target is not found.
func (s Service) target(ctx context.Context, ref model.Ref, caps []model.Capability) (target, error) {
	if ref.Tenant != s.c.Principal().Tenant {
		return target{}, engine.ErrNotFound()
	}
	e := s.c.Engine()
	var t target
	err := e.Store().View(ctx, ref.Tenant, func(r store.Reader) error {
		var err error
		if t, err = resolve(ctx, r, ref); err != nil {
			return err
		}
		return declared(e, t.channel.Type, caps)
	})
	if model.IsCode(err, model.NotFound) {
		return target{}, engine.ErrNotFound()
	}
	return t, err
}

// adapterError keeps context errors and safe typed errors; anything else
// becomes model.Unavailable without the adapter's text.
func adapterError(ctx context.Context, err error) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	return runtime.AsFailure(err)
}

func cloneQuery(req model.QueryExtension) model.QueryExtension {
	req.Query = req.Query.Clone()
	return req
}

// validateAction asks the channel adapter's factory, when it implements
// adapter.ActionValidator, whether it accepts the action's fields, without
// I/O. The first issue is the error.
func validateAction(e *engine.Engine, t model.TypeKey, aud model.Audience, action model.Extension) error {
	f, _, ok := e.Adapter(t)
	if !ok {
		return nil
	}
	v, ok := f.(adapter.ActionValidator)
	if !ok {
		return nil
	}
	issues := v.ValidateAction(aud, action.Clone())
	if len(issues) == 0 {
		return nil
	}
	i := issues[0]
	code := model.Validation
	switch model.ErrorCode(i.Code) {
	case model.Unsupported, model.SchemaMismatch:
		code = model.ErrorCode(i.Code)
	}
	msg := "action." + i.Field
	if i.Message != "" {
		msg += ": " + i.Message
	}
	return model.NewError(code, msg)
}
