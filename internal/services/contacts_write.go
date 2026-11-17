package services

import (
	"cmp"
	"context"
	"slices"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Create records a contact no identity points at, as a committed operation
// authorized as model.ActionContactWrite on
// model.CreationTarget(tenant, model.KindContact), which is also the
// operation target. The contact takes the requested kind (person when
// unset), display name and attributes, stored sorted by kind and value, and
// a fresh revision; it is unified with identities by Contacts.Link only.
// One contact.created event for the internal audience commits with it.
// Retrying with the same operation ID and payload returns the stored
// operation; a different payload conflicts.
func (s Contacts) Create(ctx context.Context, req model.CreateContact) (model.Acceptance, error) {
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
	if req.Tenant != tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if req.Kind == "" {
		req.Kind = model.ContactKindPerson
	}
	req.Attributes = sortedAttributes(req.Attributes)
	ids, err := allocate(ctx, e, 5)
	if err != nil {
		return model.Acceptance{}, err
	}
	ref := model.ContactRef{Tenant: tenant, ID: ids[0]}
	target := model.CreationTarget(tenant, model.KindContact)
	return commit(ctx, c, localCommand{
		kind: model.ActionContactWrite, target: target, resource: target, owner: target,
		command: req.Command, fingerprint: createContactFingerprint(req, target), result: ref.Ref(),
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			ct := model.Contact{
				Ref: ref, DisplayName: req.DisplayName, Kind: req.Kind,
				Attributes: slices.Clone(req.Attributes), Revision: model.Revision(ids[1]),
			}
			if err := tx.PutContact(ctx, ct, ""); err != nil {
				return err
			}
			return appendContactEvent(ctx, tx, c, ids[2], model.EventContactCreated, model.SchemaContactCreated,
				ref, ct.Revision, req.Correlation, model.ContactCreatedPayload{
					Contact: ref, Kind: ct.Kind, Trigger: model.TriggerCommand, Operation: op,
				})
		},
	}, ids[3], model.Revision(ids[4]))
}

// Update replaces a contact's display name and attributes at
// ExpectedRevision, as a committed operation authorized as
// model.ActionContactWrite on the contact at that revision. The contact's
// kind and its identities are untouched. A stale revision is
// model.Conflict and changes nothing. When neither field differs the call
// commits with no event and the revision unchanged; otherwise the revision
// moves and one contact.updated event for the internal audience commits
// with it, naming the fields that changed. A repeated operation returns its
// stored result even once the revision has moved on.
func (s Contacts) Update(ctx context.Context, req model.UpdateContact) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Contact.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if err := view(ctx, c, func(r store.Reader) error {
		var ct model.Contact
		return loadContact(ctx, r, req.Contact, &ct)
	}); err != nil {
		return model.Acceptance{}, err
	}
	req.Attributes = sortedAttributes(req.Attributes)
	ids, err := allocate(ctx, e, 4)
	if err != nil {
		return model.Acceptance{}, err
	}
	res := req.Contact.Ref()
	return commit(ctx, c, localCommand{
		kind: model.ActionContactWrite, target: res, resource: res, owner: res,
		command: req.Command, fingerprint: updateContactFingerprint(req, res), result: res,
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			var cur model.Contact
			if err := loadContact(ctx, tx, req.Contact, &cur); err != nil {
				return err
			}
			if cur.Revision != req.ExpectedRevision {
				return model.NewError(model.Conflict, "contact revision changed")
			}
			var changes []string
			if cur.DisplayName != req.DisplayName {
				changes = append(changes, model.ContactChangeDisplayName)
			}
			if !slices.Equal(cur.Attributes, req.Attributes) {
				changes = append(changes, model.ContactChangeAttributes)
			}
			if len(changes) == 0 {
				return nil
			}
			next := cur
			next.DisplayName, next.Attributes, next.Revision = req.DisplayName, slices.Clone(req.Attributes), model.Revision(ids[0])
			if err := tx.PutContact(ctx, next, cur.Revision); err != nil {
				return err
			}
			return appendContactEvent(ctx, tx, c, ids[1], model.EventContactUpdated, model.SchemaContactUpdated,
				req.Contact, next.Revision, req.Correlation, model.ContactUpdatedPayload{
					Contact: req.Contact, Changes: changes, Trigger: model.TriggerCommand,
					PreviousRevision: cur.Revision, Operation: op,
				})
		},
	}, ids[2], model.Revision(ids[3]))
}

// appendContactEvent appends one contact event for the internal audience,
// the desk, in the unit of the write it reports.
func appendContactEvent(ctx context.Context, tx store.Tx, c *engine.Client, id model.ID, kind, schema string,
	ref model.ContactRef, rev model.Revision, correlation model.CorrelationID, payload any,
) error {
	ev, err := commandEvent(c, id, kind, schema, ref.Ref(), rev, model.Audience{Visibility: model.Internal}, correlation, payload)
	if err != nil {
		return err
	}
	return tx.AppendEvents(ctx, []model.Event{ev})
}

// sortedAttributes returns attrs sorted by kind then value, the stored
// order, so equal sets compare and fingerprint equal.
func sortedAttributes(attrs []model.Attribute) []model.Attribute {
	if len(attrs) == 0 {
		return nil
	}
	out := slices.Clone(attrs)
	slices.SortFunc(out, func(a, b model.Attribute) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Value, b.Value))
	})
	return out
}

func createContactFingerprint(req model.CreateContact, target model.Ref) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionContactWrite))
	f.Set("target", operations.Field().Ref(target))
	f.Set("create", operations.Field().Str(req.Kind).Str(req.DisplayName))
	f.Set("attributes", attributesField(req.Attributes))
	return f.Sum()
}

func updateContactFingerprint(req model.UpdateContact, target model.Ref) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionContactWrite))
	f.Set("target", operations.Field().Ref(target))
	f.Set("update", operations.Field().Str(req.DisplayName))
	f.Set("attributes", attributesField(req.Attributes))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

func attributesField(attrs []model.Attribute) *operations.Writer {
	w := operations.Field().U32(uint32(len(attrs))) // #nosec G115 -- bounded by model.MaxContactAttributes
	for _, a := range attrs {
		w.Str(string(a.Kind)).Str(a.Value)
	}
	return w
}
