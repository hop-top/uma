package memory

import (
	"context"
	"slices"

	"hop.top/uma/model"
)

func (r reader) GetLabel(ctx context.Context, ref model.LabelRef) (model.Label, error) {
	return lookup(ctx, r.h, &r.h.st.labels, ref, ref, same[model.Label])
}

func (r reader) LabelByName(ctx context.Context, name string) (model.Label, error) {
	if err := r.h.enter(ctx); err != nil {
		return model.Label{}, err
	}
	defer r.h.mu.Unlock()
	if err := required("name", name); err != nil {
		return model.Label{}, err
	}
	ref, ok := r.h.st.labelNames.get(model.LabelNameKey(name))
	if !ok {
		return model.Label{}, notFound()
	}
	e, ok := r.h.st.labels.get(ref.v)
	if !ok {
		return model.Label{}, notFound()
	}
	return e.v, nil
}

func (r reader) Labels(ctx context.Context, req model.ListRequest) (model.Page[model.Label], error) {
	return listTable(ctx, r.h, &r.h.st.labels, req, filterSupport{},
		func(model.Filter, model.Label) bool { return true }, same[model.Label])
}

func (t tx) PutLabel(ctx context.Context, v model.Label, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(v.Ref), v.Validate()); err != nil {
		return err
	}
	key := model.LabelNameKey(v.Name)
	if holder, held := h.st.labelNames.get(key); held && holder.v != v.Ref {
		return conflict("name already names another label")
	}
	old, found := h.st.labels.get(v.Ref)
	if err := put(h, &h.st.labels, v.Ref, v, labelRev, expected); err != nil {
		return err
	}
	if found {
		h.st.labelNames.del(model.LabelNameKey(old.v.Name))
	}
	h.st.labelNames.set(key, entry[model.LabelRef]{v: v.Ref})
	return nil
}

func (t tx) DeleteLabel(ctx context.Context, ref model.LabelRef, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(ref), required("expected revision", string(expected))); err != nil {
		return err
	}
	cur, found := h.st.labels.get(ref)
	if !found {
		return notFound()
	}
	if err := cas(found, cur.v.Revision, expected); err != nil {
		return err
	}
	holders := h.st.conversations.sorted(func(c model.Conversation) bool { return slices.Contains(c.Labels, ref) })
	if len(holders) > 0 {
		return conflict("label is assigned; remove it first")
	}
	h.st.labels.del(ref)
	h.st.labelNames.del(model.LabelNameKey(cur.v.Name))
	return nil
}

// checkLabels requires a conversation's labels to be valid and stored.
func (h *handle) checkLabels(v model.Conversation) error {
	if err := v.ValidateLabels(); err != nil {
		return err
	}
	for _, l := range v.Labels {
		if _, ok := h.st.labels.get(l); !ok {
			return notFound()
		}
	}
	return nil
}

// matchLabels reports whether held, a conversation's labels, satisfies the
// filter's label selector: every non-empty part, Any by at least one label,
// All by each, None by none.
func matchLabels(f model.Filter, held []model.LabelRef) bool {
	s := f.Labels
	if s == nil {
		return true
	}
	if len(s.Any) > 0 && !slices.ContainsFunc(s.Any, func(l model.LabelRef) bool { return slices.Contains(held, l) }) {
		return false
	}
	for _, l := range s.All {
		if !slices.Contains(held, l) {
			return false
		}
	}
	return !slices.ContainsFunc(s.None, func(l model.LabelRef) bool { return slices.Contains(held, l) })
}

func labelRev(v model.Label) model.Revision { return v.Revision }
