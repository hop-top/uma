package uma

import (
	"context"
	"net/http"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/attachments"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/events"
	"hop.top/uma/internal/extensions"
	"hop.top/uma/internal/interactions"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/privacy"
	"hop.top/uma/internal/retention"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/internal/services"
	"hop.top/uma/internal/streams"
	"hop.top/uma/model"
)

// New composes an engine from cfg. It validates the composition only: it
// performs no I/O, starts no goroutines, opens no adapter instance and
// consults no policy. A rejected composition returns a *model.Error with
// code model.Validation naming the Config field.
//
// Registration rejects, with no override and no last-wins rule: a nil
// factory; a descriptor type failing model.ValidateTypeKey (grammar,
// reserved-unimplemented maintained keys, reserved prefixes) or registered
// twice; a contract major other than adapter.ContractMajor; a capability
// that is neither builtin nor a namespaced name some extension definition
// requires; respond_follow_up declared without respond; an event that is
// neither builtin nor a namespaced kind with an event-kind definition;
// duplicate capabilities or events; a configuration
// schema registered by two adapters or named like an extension definition;
// and an extension definition with an invalid or reserved name, zero
// version, unknown kind, missing or invalid policy action or capability,
// invalid schema names, a duplicate name and version, a kind or policy
// action other than another version of its name has, or an effect
// authorizing as a builtin read-only or core-only action (see
// adapter.ExtensionDefinition).
func New(cfg Config) (Engine, error) {
	services.RegisterHeld()
	e, err := engine.New(engine.Config{
		Store:                cfg.Store,
		Authorizer:           cfg.Authorizer,
		IdentityVerifiers:    cfg.IdentityVerifiers,
		Selectors:            cfg.Selectors,
		InteractionValidator: cfg.InteractionValidator,
		CascadePolicy:        cfg.CascadePolicy,
		Adapters:             cfg.Adapters,
		Extensions:           cfg.Extensions,
		Credentials:          cfg.Credentials,
		HTTP:                 cfg.HTTP,
		Clock:                cfg.Clock,
		IDs:                  cfg.IDs,
		Blobs:                cfg.Blobs,
		Observer:             cfg.Observer,
		Waiter:               cfg.Waiter,
		Retry:                engine.RetryPolicy(cfg.Retry),
		Retention:            engine.RetentionPolicy(cfg.Retention),
		Work:                 (*engine.WorkPolicy)(cfg.Work),
		RequireDurable:       cfg.RequireDurable,
		WebhookBodyLimit:     cfg.WebhookBodyLimit,
		AttachmentSizeLimit:  cfg.AttachmentSizeLimit,
		AttachmentMediaTypes: cfg.AttachmentMediaTypes,
		Held:                 engine.HeldPolicy(cfg.Held),
	})
	if err != nil {
		return nil, err
	}
	return &composed{e: e, rt: runtime.New(e, extensions.Effector(), streams.StreamEffector(), interactions.EphemeralEffector(), interactions.ResponseEffector(), privacy.CascadeEffector())}, nil
}

// composed binds the public interfaces to the engine implementation.
type composed struct {
	e  *engine.Engine
	rt *runtime.Runtime
}

func (c *composed) For(p model.Principal) (Client, error) {
	cl, err := c.e.For(p)
	if err != nil {
		return nil, err
	}
	return client{cl, c.rt}, nil
}

func (c *composed) Runtime() Runtime                { return engineRuntime{c.rt, c.e} }
func (c *composed) Describe() []adapter.Descriptor  { return c.e.Describe() }
func (c *composed) Close(ctx context.Context) error { return c.e.Close(ctx) }

func (c *composed) ValidateChannel(t model.TypeKey, cfg model.Extension) []model.ValidationIssue {
	return c.e.ValidateChannel(t, cfg)
}

type client struct {
	c  *engine.Client
	rt *runtime.Runtime
}

func (c client) Configuration() Configuration { return services.NewConfiguration(c.c) }
func (c client) Contacts() Contacts           { return services.NewContacts(c.c) }
func (c client) Conversations() Conversations { return services.NewConversations(c.c) }
func (c client) Labels() Labels               { return services.NewLabels(c.c) }
func (c client) Messages() Messages           { return services.NewMessages(c.c) }
func (c client) WorkItems() WorkItems         { return services.NewWorkItems(c.c) }
func (c client) Tasks() Tasks                 { return services.NewTasks(c.c) }
func (c client) Operations() Operations       { return reconcilingOperations{operations.New(c.c), c.c, c.rt} }
func (c client) Events() Events               { return events.New(c.c) }
func (c client) Attachments() Attachments     { return attachments.New(c.c) }
func (c client) Extensions() Extensions       { return extensions.New(c.c) }
func (c client) Interactions() Interactions   { return interactions.New(c.c) }
func (c client) Streams() Streams             { return streams.New(c.c) }
func (c client) Privacy() Privacy             { return privacy.New(c.c) }

type engineRuntime struct {
	rt *runtime.Runtime
	e  *engine.Engine
}

func (r engineRuntime) Webhook(ctx context.Context, call RuntimeCall, ref model.ChannelRef) (http.Handler, error) {
	return r.rt.Webhook(ctx, runtime.Call(call), ref)
}

func (r engineRuntime) Receive(ctx context.Context, call RuntimeCall, ref model.ChannelRef, opts ...ReceiveOption) error {
	var o runtime.ReceiveOptions
	for _, opt := range opts {
		if opt.owner != "" {
			o.Owner = opt.owner
		}
		if opt.spreadSet {
			o.StartSpread = opt.spread
		}
	}
	return r.rt.Receive(ctx, runtime.Call(call), ref, o)
}

func (r engineRuntime) ReceiverStatus(ctx context.Context, call RuntimeCall, ref model.ChannelRef) (ReceiverStatus, error) {
	st, err := r.rt.ReceiverStatus(ctx, runtime.Call(call), ref)
	return receiverStatus(st), err
}

func (r engineRuntime) ReceiverState(call RuntimeCall, ref model.ChannelRef) (ReceiverStatus, bool) {
	st, ok := r.rt.ReceiverState(runtime.Call(call), ref)
	return receiverStatus(st), ok
}

func receiverStatus(st runtime.ReceiverStatus) ReceiverStatus {
	return ReceiverStatus{
		Owner: st.Owner, Until: st.Until, Phase: ReceiverPhase(st.Phase), Since: st.Since,
		Failures: st.Failures, Reason: st.Reason, RetryAt: st.RetryAt, Link: ReceiverLink(st.Link), Detail: st.Detail,
	}
}

func (r engineRuntime) Run(ctx context.Context, call RuntimeCall, opts WorkerOptions) error {
	return r.rt.Run(ctx, runtime.Call(call), runtime.WorkerOptions(opts))
}

func (r engineRuntime) Step(ctx context.Context, call RuntimeCall, opts WorkerOptions) (int, error) {
	return r.rt.Step(ctx, runtime.Call(call), runtime.WorkerOptions(opts))
}

func (r engineRuntime) Drain(ctx context.Context, call RuntimeCall) error {
	return r.rt.Drain(ctx, runtime.Call(call))
}

func (r engineRuntime) Purge(ctx context.Context, call RuntimeCall) (PurgeReport, error) {
	rep, err := retention.Purge(ctx, r.e, call.Principal)
	return PurgeReport(rep), err
}

func (r engineRuntime) Import(ctx context.Context, call RuntimeCall, req ImportRequest) (ImportReport, error) {
	rep, err := r.rt.Import(ctx, runtime.Call(call), runtime.ImportRequest(req))
	return ImportReport(rep), err
}

// reconcilingOperations is the Operations service with Reconcile, which
// runs on the engine's runtime.
type reconcilingOperations struct {
	operations.Service
	c  *engine.Client
	rt *runtime.Runtime
}

func (o reconcilingOperations) List(ctx context.Context, req model.ListRequest) (model.Page[model.Operation], error) {
	return services.ListOperations(ctx, o.c, req)
}

func (o reconcilingOperations) Reconcile(ctx context.Context, req model.Lookup[model.OperationRef]) (model.Operation, error) {
	return o.rt.Reconcile(ctx, o.c, req)
}

// Trust counters: the internal service's primitives, on the channel the
// request names. Trust and Trusts are authorized for trust.read, the resets
// for trust.reset, ImportTrust for trust.write, all on the channel.

func (o reconcilingOperations) Trust(ctx context.Context, req model.TrustLookup) (model.TrustEntry, error) {
	if err := o.trustCall(req.Call); err != nil {
		return model.TrustEntry{}, err
	}
	return operations.New(o.c).Trust(ctx, req.Channel, req.Key)
}

// trustCall refuses a closed engine, then an invalid call.
func (o reconcilingOperations) trustCall(c model.Call) error {
	if o.c.Engine().IsClosed() {
		return engine.ErrClosed()
	}
	return c.Validate()
}

// trustsPage is the default and the largest page of Trusts.
const (
	trustsPageDefault = 100
	trustsPageMax     = 500
)

func (o reconcilingOperations) Trusts(ctx context.Context, req model.TrustFilter) (model.Page[model.TrustEntry], error) {
	if err := o.trustCall(req.Call); err != nil {
		return model.Page[model.TrustEntry]{}, err
	}
	all, err := operations.New(o.c).Trusts(ctx, req.Channel, req)
	if err != nil {
		return model.Page[model.TrustEntry]{}, err
	}
	limit := req.Page.Limit
	if limit <= 0 {
		limit = trustsPageDefault
	}
	limit = min(limit, trustsPageMax)
	var out model.Page[model.TrustEntry]
	for _, e := range all {
		if req.Page.After != "" && e.Key <= string(req.Page.After) {
			continue
		}
		if len(out.Items) == limit {
			out.Next = model.Cursor(out.Items[len(out.Items)-1].Key)
			return out, nil
		}
		out.Items = append(out.Items, e)
	}
	out.Complete = true
	return out, nil
}

func (o reconcilingOperations) ResetTrust(ctx context.Context, req model.ResetTrust) (model.TrustEntry, error) {
	if err := o.trustCall(req.Call); err != nil {
		return model.TrustEntry{}, err
	}
	s := operations.New(o.c)
	if err := s.ResetTrust(ctx, req.Channel, req.Key); err != nil {
		return model.TrustEntry{}, err
	}
	e, err := s.Trust(ctx, req.Channel, req.Key)
	if model.IsCode(err, model.NotFound) {
		// A key never counted has nothing to reset.
		return model.TrustEntry{Key: req.Key}, nil
	}
	return e, err
}

func (o reconcilingOperations) ImportTrust(ctx context.Context, req model.ImportTrust) (model.TrustEntry, error) {
	if err := o.trustCall(req.Call); err != nil {
		return model.TrustEntry{}, err
	}
	return operations.New(o.c).ImportTrust(ctx, req)
}

func (o reconcilingOperations) ResetTrustSet(ctx context.Context, req model.ResetTrustSet) ([]model.TrustEntry, error) {
	if err := o.trustCall(req.Call); err != nil {
		return nil, err
	}
	s := operations.New(o.c)
	if err := s.ResetTrustSet(ctx, req.Channel, req.Keys); err != nil {
		return nil, err
	}
	var out []model.TrustEntry
	for _, k := range req.Keys {
		e, err := s.Trust(ctx, req.Channel, k)
		if model.IsCode(err, model.NotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
