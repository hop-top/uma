package services

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Labels is the Labels service of one client: the tenant's label
// definitions. Definitions emit no events.
type Labels struct{ c *engine.Client }

// NewLabels returns the Labels service bound to c.
func NewLabels(c *engine.Client) Labels { return Labels{c} }

// Configure creates a label (empty Ref.ID) or replaces one at
// ExpectedRevision, as a committed operation authorized as
// model.ActionLabelConfigure: on model.CreationTarget(tenant,
// model.KindLabel) when creating, which is also the operation target, and
// on the label otherwise. A name another label of the tenant holds under
// model.LabelNameKey is model.Conflict. Renaming keeps every assignment.
// Retrying with the same operation ID and payload returns the stored
// operation; a different payload conflicts.
func (s Labels) Configure(ctx context.Context, req model.ConfigureLabel) (model.Acceptance, error) {
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
	if req.Label.Ref.Tenant != tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	label := req.Label
	create := label.Ref.ID == ""
	target := model.CreationTarget(tenant, model.KindLabel)
	ids, err := allocate(ctx, e, 4)
	if err != nil {
		return model.Acceptance{}, err
	}
	if create {
		label.Ref.ID = ids[0]
	} else {
		if err := view(ctx, c, func(r store.Reader) error {
			got, err := r.GetLabel(ctx, label.Ref)
			return exactRef(err, got.Ref == label.Ref)
		}); err != nil {
			return model.Acceptance{}, err
		}
		target = label.Ref.Ref()
	}
	result := label.Ref.Ref()
	if req.Hold != nil && e.Held().LabelConfigure {
		return holdLabelConfigure(ctx, c, req, label, create, target, result)
	}
	label.Revision = model.Revision(ids[1])
	expected := req.ExpectedRevision
	return commit(ctx, c, localCommand{
		kind: model.ActionLabelConfigure, target: target, resource: target, owner: target,
		command: req.Command, fingerprint: labelFingerprint(req, target), result: result,
		apply: func(ctx context.Context, tx store.Tx, _ model.OperationRef) error {
			if !create {
				stored, err := tx.GetLabel(ctx, label.Ref)
				if err := exactRef(err, stored.Ref == label.Ref); err != nil {
					return err
				}
			}
			return tx.PutLabel(ctx, label, expected)
		},
	}, ids[2], model.Revision(ids[3]))
}

func labelFingerprint(req model.ConfigureLabel, target model.Ref) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionLabelConfigure))
	f.Set("target", operations.Field().Ref(target))
	l := req.Label
	f.Set("label", operations.Field().Str(string(l.Ref.ID)).Str(l.Name).Str(l.Description).Str(l.Color))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

// Get returns a label the principal may read (model.ActionLabelRead). A
// label of another tenant is not found.
func (s Labels) Get(ctx context.Context, req model.Lookup[model.LabelRef]) (model.Label, error) {
	c := s.c
	done, err := c.Engine().Begin()
	if err != nil {
		return model.Label{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Label{}, err
	}
	if req.Ref.Tenant != c.Principal().Tenant {
		return model.Label{}, engine.ErrNotFound()
	}
	var l model.Label
	if err := view(ctx, c, func(r store.Reader) (err error) {
		l, err = r.GetLabel(ctx, req.Ref)
		return exactRef(err, l.Ref == req.Ref)
	}); err != nil {
		return model.Label{}, err
	}
	if err := c.Authorize(ctx, labelRead(l), l.Ref.Ref()); err != nil {
		return model.Label{}, err
	}
	return l, nil
}

func labelRead(l model.Label) policy.Authorization {
	return policy.Authorization{Action: model.ActionLabelRead, Resource: l.Ref.Ref(), Revision: l.Revision}
}

// List pages the tenant's labels the principal may read, in recording
// order; the others are skipped without trace. It honours no filter field.
func (s Labels) List(ctx context.Context, req model.ListRequest) (model.Page[model.Label], error) {
	return list(ctx, s.c, listQuery[model.Label]{
		name: "labels", read: store.Reader.Labels,
		within: func(model.Filter, model.Label) bool { return true },
		admit: func(ctx context.Context, c *engine.Client, l model.Label) error {
			return c.Authorize(ctx, labelRead(l), l.Ref.Ref())
		},
		clone: func(l model.Label) model.Label { return l },
	}, req)
}

// ByName returns the tenant's label whose name equals Key under
// model.LabelNameKey, when the principal may read it.
func (s Labels) ByName(ctx context.Context, req model.LookupKey) (model.Label, error) {
	return byKey(ctx, s.c, req, store.Reader.LabelByName, labelRead, func(l model.Label) model.Label { return l })
}

// Delete deletes a label definition at ExpectedRevision, as a committed
// operation authorized as model.ActionLabelConfigure on the label. A label
// any conversation holds is model.Conflict. Retrying with the same
// operation ID returns the stored operation, even once the label is gone.
func (s Labels) Delete(ctx context.Context, req model.DeleteLabel) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Label.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if req.Hold != nil && e.Held().LabelDelete {
		return holdLabelDelete(ctx, c, req)
	}
	ids, err := allocate(ctx, e, 2)
	if err != nil {
		return model.Acceptance{}, err
	}
	res := req.Label.Ref()
	return commit(ctx, c, localCommand{
		kind: model.ActionLabelConfigure, target: res, resource: res, owner: res,
		command: req.Command, fingerprint: deleteLabelFingerprint(req), result: res,
		apply: func(ctx context.Context, tx store.Tx, _ model.OperationRef) error {
			stored, err := tx.GetLabel(ctx, req.Label)
			if err := exactRef(err, stored.Ref == req.Label); err != nil {
				return err
			}
			return tx.DeleteLabel(ctx, req.Label, req.ExpectedRevision)
		},
	}, ids[0], model.Revision(ids[1]))
}

func deleteLabelFingerprint(req model.DeleteLabel) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionLabelConfigure))
	f.Set("target", operations.Field().Ref(req.Label.Ref()))
	f.Set("delete", operations.Field().Str("true"))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}
