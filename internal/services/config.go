package services

import (
	"context"
	"slices"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// ConfigureChannel creates a channel (empty Ref.ID) or replaces one at
// ExpectedRevision, as a committed operation. The adapter type must be
// registered and its configuration valid (Engine.ValidateChannel); a
// channel never changes type. Configuring opens no adapter instance and
// makes no provider connection. Retrying with the same operation ID and
// payload returns the stored operation; a different payload conflicts.
func (s Configuration) ConfigureChannel(ctx context.Context, req model.ConfigureChannel) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	tenant := c.Principal().Tenant
	if req.Channel.Ref.Tenant != tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if err := issuesError(e.ValidateChannel(req.Channel.Type, req.Channel.Configuration)); err != nil {
		return model.Acceptance{}, err
	}
	ch := req.Channel.Clone()
	create := ch.Ref.ID == ""
	target := model.CreationTarget(tenant, model.KindChannel)
	ids, err := allocate(ctx, e, 4)
	if err != nil {
		return model.Acceptance{}, err
	}
	if create {
		ch.Ref.ID = ids[0]
	} else {
		if err := view(ctx, c, func(r store.Reader) error {
			got, err := r.GetChannel(ctx, ch.Ref)
			return exactRef(err, got.Ref == ch.Ref)
		}); err != nil {
			return model.Acceptance{}, err
		}
		target = ch.Ref.Ref()
	}
	ch.Revision = model.Revision(ids[1])
	resource := ch.Ref.Ref()
	expected := req.ExpectedRevision
	return commit(ctx, c, localCommand{
		kind: model.ActionChannelConfigure, target: target, resource: resource, owner: resource,
		command: req.Command, fingerprint: channelFingerprint(req, target), result: resource,
		apply: func(ctx context.Context, tx store.Tx, _ model.OperationRef) error {
			if !create {
				stored, err := tx.GetChannel(ctx, ch.Ref)
				if err := exactRef(err, stored.Ref == ch.Ref); err != nil {
					return err
				}
				if stored.Type != ch.Type {
					return model.NewError(model.Validation, "channel.type: a channel never changes type")
				}
			}
			if ch.Key != "" {
				held, err := tx.ResolveChannelKey(ctx, ch.Key)
				switch {
				case model.IsCode(err, model.NotFound):
				case err != nil:
					return err
				case held.Ref != ch.Ref:
					return model.NewError(model.Conflict, "channel.key: already names another channel")
				}
			}
			return tx.PutChannel(ctx, ch, expected)
		},
	}, ids[2], model.Revision(ids[3]))
}

// issuesError returns the first validation issue as an error: an
// unregistered adapter type is unsupported, anything else invalid.
func issuesError(issues []model.ValidationIssue) error {
	if len(issues) == 0 {
		return nil
	}
	i := issues[0]
	code := model.Validation
	switch model.ErrorCode(i.Code) {
	case model.Unsupported, model.SchemaMismatch:
		code = model.ErrorCode(i.Code)
	}
	msg := "channel." + i.Field
	if i.Message != "" {
		msg += ": " + i.Message
	}
	return model.NewError(code, msg)
}

func channelFingerprint(req model.ConfigureChannel, target model.Ref) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionChannelConfigure))
	f.Set("target", operations.Field().Ref(target))
	ch := req.Channel
	f.Set("channel", operations.Field().Str(string(ch.Ref.ID)).Str(string(ch.Type)).Extension(ch.Configuration).Str(ch.CredentialRef))
	if ch.Key != "" {
		// Set only when present, so fingerprints of keyless channels keep
		// their version 1 encoding.
		f.Set("key", operations.Field().Str(ch.Key))
	}
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

// ConfigureInbox creates an inbox (empty Ref.ID) or replaces one at
// ExpectedRevision, as a committed operation. It validates the request and
// the selection configuration (model.Selection.Validate: per-contact needs
// an explicit flag and the approved lifecycle rule set, other modes
// neither), requires a registered Policy when one is named, and requires the
// channel to exist in the principal's tenant. Destinations must be distinct
// and bound to no other inbox of the channel. An inbox never moves to
// another channel. Changing the flag of a live inbox leaves existing
// conversations as they are. Retrying with the same operation ID and
// payload returns the stored operation; a different payload conflicts.
func (s Configuration) ConfigureInbox(ctx context.Context, req model.ConfigureInbox) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := engine.FirstErr(req.Validate(), req.Inbox.Selection.Validate(), checkDestinations(req.Inbox.Destinations)); err != nil {
		return model.Acceptance{}, err
	}
	if p := req.Inbox.Selection.Policy; p != "" {
		if !e.HasSelector(p) {
			return model.Acceptance{}, model.NewError(model.Validation, "selection.policy: no selector registered under this name")
		}
	}
	tenant := c.Principal().Tenant
	if req.Inbox.Ref.Tenant != tenant || req.Inbox.Channel.Tenant != tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	inbox := req.Inbox.Clone()
	create := inbox.Ref.ID == ""
	// A new inbox is created under its channel until it has an ID.
	target := inbox.Channel.Ref()
	ids, err := allocate(ctx, e, 4)
	if err != nil {
		return model.Acceptance{}, err
	}
	if create {
		inbox.Ref.ID = ids[0]
	} else {
		if err := view(ctx, c, func(r store.Reader) error {
			got, err := r.GetInbox(ctx, inbox.Ref)
			return exactRef(err, got.Ref == inbox.Ref)
		}); err != nil {
			return model.Acceptance{}, err
		}
		target = inbox.Ref.Ref()
	}
	inbox.Revision = model.Revision(ids[1])
	resource := inbox.Ref.Ref()
	destination := inbox.Channel.Ref()
	expected := req.ExpectedRevision
	return commit(ctx, c, localCommand{
		kind: model.ActionInboxConfigure, target: target, resource: resource, owner: resource,
		destination: &destination, command: req.Command, fingerprint: inboxFingerprint(req, target), result: resource,
		apply: func(ctx context.Context, tx store.Tx, _ model.OperationRef) error {
			if !create {
				stored, err := tx.GetInbox(ctx, inbox.Ref)
				if err := exactRef(err, stored.Ref == inbox.Ref); err != nil {
					return err
				}
				if stored.Channel != inbox.Channel {
					return model.NewError(model.Validation, "inbox.channel: an inbox never moves to another channel")
				}
			}
			ch, err := tx.GetChannel(ctx, inbox.Channel)
			if err := exactRef(err, ch.Ref == inbox.Channel); err != nil {
				return err
			}
			if inbox.DeliverNotes {
				if _, desc, ok := e.Adapter(ch.Type); !ok || !slices.Contains(desc.Capabilities, model.CapabilityInternalNote) {
					return model.NewError(model.Unsupported, "inbox.deliver_notes: channel adapter does not declare internal_note")
				}
			}
			if inbox.Key != "" {
				held, err := tx.ResolveInboxKey(ctx, inbox.Key)
				switch {
				case model.IsCode(err, model.NotFound):
				case err != nil:
					return err
				case held.Ref != inbox.Ref:
					return model.NewError(model.Conflict, "inbox.key: already names another inbox")
				}
			}
			for _, d := range inbox.Destinations {
				bound, err := tx.ResolveDestination(ctx, inbox.Channel, d)
				switch {
				case model.IsCode(err, model.NotFound):
				case err != nil:
					return err
				case bound.Ref != inbox.Ref:
					return model.NewError(model.Conflict, "inbox.destinations: destination already bound to another inbox")
				}
			}
			return tx.PutInbox(ctx, inbox, expected)
		},
	}, ids[2], model.Revision(ids[3]))
}

func checkDestinations(ds []string) error {
	for i, d := range ds {
		if d == "" {
			return model.NewError(model.Validation, "inbox.destinations: must not be empty")
		}
		if slices.Contains(ds[:i], d) {
			return model.NewError(model.Validation, "inbox.destinations: must be distinct")
		}
	}
	return nil
}

func inboxFingerprint(req model.ConfigureInbox, target model.Ref) []byte {
	f := operations.NewFingerprint()
	in := req.Inbox
	f.Set("kind", operations.Field().Str(model.ActionInboxConfigure))
	f.Set("target", operations.Field().Ref(target))
	f.Set("inbox", operations.Field().Str(string(in.Ref.ID)).Ref(in.Channel.Ref()).Str(in.Name).Present(in.Disabled))
	ds := operations.Field().Str(strconv.Itoa(len(in.Destinations)))
	for _, d := range in.Destinations {
		ds.Str(d)
	}
	f.Set("destinations", ds)
	if in.DeliverNotes {
		// Set only when on, so fingerprints of inboxes that never opted
		// in keep their version 1 encoding.
		f.Set("deliver_notes", operations.Field().Present(true))
	}
	if in.Key != "" {
		f.Set("key", operations.Field().Str(in.Key))
	}
	sel := operations.Field().Str(in.Selection.Mode).Str(in.Selection.Policy).Str(in.Selection.LifecycleRules).Present(in.Selection.LockToSingle != nil)
	if in.Selection.LockToSingle != nil {
		sel.Present(*in.Selection.LockToSingle)
	}
	f.Set("selection", sel)
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

// Channels lists the tenant's channels the principal may read, in
// recording order. It takes no filter.
func (s Configuration) Channels(ctx context.Context, req model.ListRequest) (model.Page[model.Channel], error) {
	return list(ctx, s.c, listQuery[model.Channel]{
		name: "channels", read: store.Reader.Channels,
		within: func(model.Filter, model.Channel) bool { return true },
		admit: func(ctx context.Context, c *engine.Client, ch model.Channel) error {
			r := ch.Ref.Ref()
			return c.Authorize(ctx, policy.Authorization{Action: model.ActionChannelRead, Resource: r, Revision: ch.Revision}, r)
		},
		clone: model.Channel.Clone,
	}, req)
}

// Inboxes lists the tenant's inboxes the principal may read, in recording
// order, optionally limited to Filter.Inboxes.
func (s Configuration) Inboxes(ctx context.Context, req model.ListRequest) (model.Page[model.Inbox], error) {
	return list(ctx, s.c, listQuery[model.Inbox]{
		name: "inboxes", fields: filterFields{inboxes: true}, read: store.Reader.Inboxes,
		within: func(f model.Filter, in model.Inbox) bool { return inInboxes(f, in.Ref) },
		admit: func(ctx context.Context, c *engine.Client, in model.Inbox) error {
			r := in.Ref.Ref()
			return c.Authorize(ctx, policy.Authorization{Action: model.ActionInboxRead, Resource: r, Revision: in.Revision}, r)
		},
		clone: model.Inbox.Clone,
	}, req)
}

// ChannelByKey returns the tenant's channel holding the host-set key, when
// the principal may read it. No channel holds an empty key.
func (s Configuration) ChannelByKey(ctx context.Context, req model.LookupKey) (model.Channel, error) {
	return byKey(ctx, s.c, req, store.Reader.ResolveChannelKey, func(ch model.Channel) policy.Authorization {
		return policy.Authorization{Action: model.ActionChannelRead, Resource: ch.Ref.Ref(), Revision: ch.Revision}
	}, model.Channel.Clone)
}

// InboxByKey returns the tenant's inbox holding the host-set key, when the
// principal may read it. No inbox holds an empty key.
func (s Configuration) InboxByKey(ctx context.Context, req model.LookupKey) (model.Inbox, error) {
	return byKey(ctx, s.c, req, store.Reader.ResolveInboxKey, func(in model.Inbox) policy.Authorization {
		return policy.Authorization{Action: model.ActionInboxRead, Resource: in.Ref.Ref(), Revision: in.Revision}
	}, model.Inbox.Clone)
}

// byKey reads the record holding req.Key and authorizes reading it.
func byKey[T any](ctx context.Context, c *engine.Client, req model.LookupKey,
	read func(store.Reader, context.Context, string) (T, error), auth func(T) policy.Authorization, clone func(T) T,
) (T, error) {
	var zero T
	done, err := c.Engine().Begin()
	if err != nil {
		return zero, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return zero, err
	}
	var v T
	if err := view(ctx, c, func(r store.Reader) (err error) {
		v, err = read(r, ctx, req.Key)
		return err
	}); err != nil {
		return zero, err
	}
	a := auth(v)
	if err := c.Authorize(ctx, a, a.Resource); err != nil {
		return zero, err
	}
	return clone(v), nil
}

// Capabilities reports the current capability status of a channel the
// principal may read: every capability its adapter descriptor declares,
// with availability and limits as the opened instance reports them now
// and, when the adapter declares permission probing, as its provider
// permissions allow. A capability no core operation of this version
// executes is reported unavailable (model.Unsupported) whatever the
// adapter reports, so discovery never offers a path core rejects. A
// capability the descriptor does not declare is never reported. This is the one configuration read that opens an adapter
// instance and may perform provider I/O.
func (s Configuration) Capabilities(ctx context.Context, req model.Lookup[model.ChannelRef]) ([]model.CapabilityStatus, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return nil, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if req.Ref.Tenant != c.Principal().Tenant {
		return nil, engine.ErrNotFound()
	}
	var ch model.Channel
	if err := view(ctx, c, func(r store.Reader) (err error) {
		ch, err = r.GetChannel(ctx, req.Ref)
		return err
	}); err != nil {
		return nil, err
	}
	res := ch.Ref.Ref()
	if err := c.Authorize(ctx, policy.Authorization{Action: model.ActionChannelRead, Resource: res, Revision: ch.Revision}, req.Ref.Ref()); err != nil {
		return nil, err
	}
	inst, desc, _, err := runtime.OpenOutbound(ctx, e, ch)
	if err != nil {
		return nil, runtime.AsFailure(err)
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }()
	return currentCapabilities(ctx, desc, inst)
}

// currentCapabilities merges, for each declared capability, the instance's
// report and then the permission probe over a default of unavailable
// (model.Unsupported): a capability the instance does not report is not
// offered. A capability no core operation executes (engine.Executes) is
// then unavailable (model.Unsupported, no limits) however the adapter
// reports it. An invalid report fails as the capability gate does.
func currentCapabilities(ctx context.Context, desc adapter.Descriptor, inst adapter.Instance) ([]model.CapabilityStatus, error) {
	out := make([]model.CapabilityStatus, len(desc.Capabilities))
	for i, name := range desc.Capabilities {
		out[i] = model.CapabilityStatus{Name: name, Reason: model.Unsupported}
	}
	merge := func(statuses []model.CapabilityStatus) {
		for _, st := range statuses {
			if i := slices.Index(desc.Capabilities, st.Name); i >= 0 {
				out[i] = st.Clone()
			}
		}
	}
	reported, err := inst.Capabilities(ctx)
	if err != nil {
		return nil, model.NewError(model.Unavailable, "adapter capabilities unavailable")
	}
	if f := runtime.CheckStatuses(reported); f != nil {
		return nil, f
	}
	merge(reported)
	if p, ok := inst.(adapter.PermissionProber); ok && slices.Contains(desc.Capabilities, model.CapabilityProbePermissions) {
		probed, err := p.Probe(ctx)
		if err != nil {
			return nil, model.NewError(model.Unavailable, "provider permissions unavailable")
		}
		merge(probed)
	}
	for i, name := range desc.Capabilities {
		if !engine.Executes(name) {
			out[i] = model.CapabilityStatus{Name: name, Reason: model.Unsupported}
		}
	}
	return out, nil
}

// view runs fn in a read unit of the principal's tenant. A missing record
// is not found, never distinguished from a foreign one.
func view(ctx context.Context, c *engine.Client, fn func(store.Reader) error) error {
	err := c.Engine().Store().View(ctx, c.Principal().Tenant, fn)
	if model.IsCode(err, model.NotFound) {
		return engine.ErrNotFound()
	}
	return err
}

// exactRef passes a lookup error through and otherwise requires the stored
// record to carry the requested reference; a mismatch is not found.
func exactRef(err error, same bool) error {
	if err != nil {
		return err
	}
	if !same {
		return engine.ErrNotFound()
	}
	return nil
}
