package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Authorized queries.
//
// A list query pages the store in its recording order (the store's insertion
// position, unique per record and unchanged by updates), authorizes every
// scanned item as the client's principal, and exposes only admitted items.
// Inaccessible items are skipped without trace: no totals, no placeholders.
// A page may be short or empty and still carry Next; Complete reports that
// the store found no further eligible item within this query.
//
// Pages are separate store reads, not one snapshot: an item recorded after
// a page was read appears on a later page, never twice, and items already
// paged past are never revisited. Each page reauthorizes, so a policy change
// takes effect on the next page.
const (
	// DefaultPageLimit is the page size of a ListRequest with Limit 0.
	DefaultPageLimit = 50
	// MaxPageLimit caps the page size; a larger Limit is reduced to it.
	MaxPageLimit = 100
	// maxScan bounds the store items one list call scans, admitted or not.
	maxScan = 5 * MaxPageLimit
)

// filterFields names the Filter fields a query honours. Any other set field
// is model.Validation, never silently ignored.
type filterFields struct {
	inboxes, conversation, contact, kinds, statuses, identity, labels, task bool
	// Message search fields.
	text, since, before, attachments bool
	// Conversation workflow fields.
	assignee, snoozed, snoozeEnded, linked, unread bool
	// actor is Filter.Actor, honoured by the operation list only.
	actor bool
}

func (f filterFields) check(in model.Filter) error {
	reject := func(field string, set, ok bool) error {
		if set && !ok {
			return model.NewError(model.Validation, "filter."+field+": not supported by this query")
		}
		return nil
	}
	return engine.FirstErr(
		reject("inboxes", len(in.Inboxes) > 0, f.inboxes),
		reject("conversation", in.Conversation != nil, f.conversation),
		reject("contact", in.Contact != nil, f.contact),
		reject("kinds", len(in.Kinds) > 0, f.kinds),
		reject("statuses", len(in.Statuses) > 0, f.statuses),
		reject("identity", in.Identity != nil, f.identity),
		reject("labels", in.Labels != nil, f.labels),
		reject("task", in.Task != nil, f.task),
		reject("text", in.Text != "", f.text),
		reject("since", in.Since != nil, f.since),
		reject("before", in.Before != nil, f.before),
		reject("attachments", in.Attachments != nil, f.attachments),
		reject("assignee", in.Assignee != nil, f.assignee),
		reject("snoozed", in.Snoozed != nil, f.snoozed),
		reject("snooze_ended", in.SnoozeEnded != nil, f.snoozeEnded),
		reject("linked", in.Linked != nil, f.linked),
		reject("unread", in.Unread != nil, f.unread),
		reject("actor", in.Actor != nil, f.actor),
	)
}

// listQuery describes one authorized list over a store read.
type listQuery[T any] struct {
	// name identifies the query in its cursors.
	name   string
	fields filterFields
	// require, when set, checks the honoured fields together, before any
	// store read or policy call.
	require func(model.Filter) error
	read    func(store.Reader, context.Context, model.ListRequest) (model.Page[T], error)
	// within re-checks that a store item lies inside the filter scope; an
	// item outside it is never exposed, whatever the store returned.
	within func(model.Filter, T) bool
	// admit authorizes one item for the client's principal.
	admit func(context.Context, *engine.Client, T) error
	// show, when set, returns an admitted item as the principal may see it
	// under the filter, or false to leave it out without trace.
	show  func(context.Context, *engine.Client, model.Filter, T) (T, bool, error)
	clone func(T) T
	// sealed drops Next from a page that completes the query: the store
	// position where the scan stopped may lie past items admission hid.
	sealed bool
}

// list runs q for req. The request's tenant scope is the principal's: a
// filter reference in another tenant is model.NotFound.
func list[T any](ctx context.Context, c *engine.Client, q listQuery[T], req model.ListRequest) (model.Page[T], error) {
	done, err := c.Engine().Begin()
	if err != nil {
		return model.Page[T]{}, err
	}
	defer done()
	if err := engine.FirstErr(req.Validate(), q.fields.check(req.Filter)); err != nil {
		return model.Page[T]{}, err
	}
	if q.require != nil {
		if err := q.require(req.Filter); err != nil {
			return model.Page[T]{}, err
		}
	}
	f := req.Filter.Clone()
	if err := inScope(c, f); err != nil {
		return model.Page[T]{}, err
	}
	limit := pageSize(req.Page.Limit)
	binding := bindCursor(c, q.name, f)
	after, err := binding.decode(req.Page.After)
	if err != nil {
		return model.Page[T]{}, err
	}
	page := model.Page[T]{Items: []T{}}
	scanned := 0
	for len(page.Items) < limit && scanned < maxScan {
		// Ask for exactly what the page still needs, so every store page
		// ends where this call stops scanning and no item is skipped.
		need := min(limit-len(page.Items), maxScan-scanned)
		var got model.Page[T]
		err := c.Engine().Store().View(ctx, c.Principal().Tenant, func(r store.Reader) error {
			var err error
			got, err = q.read(r, ctx, model.ListRequest{Call: req.Call, Filter: f, Page: model.PageRequest{Limit: need, After: after}})
			return err
		})
		if err != nil {
			return model.Page[T]{}, err
		}
		scanned += max(len(got.Items), 1)
		// Authorization runs outside the store unit.
		for _, item := range got.Items {
			if !q.within(f, item) {
				continue
			}
			switch err := q.admit(ctx, c, item); {
			case err == nil:
				if q.show != nil {
					shown, ok, err := q.show(ctx, c, f, item)
					if err != nil {
						return model.Page[T]{}, err
					}
					if !ok {
						continue
					}
					item = shown
				}
				page.Items = append(page.Items, q.clone(item))
			case model.IsCode(err, model.Unauthorized), model.IsCode(err, model.NotFound):
			default:
				return model.Page[T]{}, err
			}
		}
		stalled := got.Next == after
		after = got.Next
		if got.Complete {
			page.Complete = true
			break
		}
		if len(got.Items) == 0 || stalled {
			break
		}
	}
	if !page.Complete || !q.sealed {
		page.Next = binding.encode(after)
	}
	return page, nil
}

func pageSize(limit int) int {
	switch {
	case limit == 0:
		return DefaultPageLimit
	case limit > MaxPageLimit:
		return MaxPageLimit
	}
	return limit
}

// inScope requires every filter reference to belong to the principal's
// tenant. Foreign references are not found, never confirmed.
func inScope(c *engine.Client, f model.Filter) error {
	refs := make([]model.Scoped, 0, len(f.Inboxes)+4)
	for _, in := range f.Inboxes {
		refs = append(refs, in)
	}
	if f.Conversation != nil {
		refs = append(refs, *f.Conversation)
	}
	if f.Contact != nil {
		refs = append(refs, *f.Contact)
	}
	if f.Identity != nil {
		refs = append(refs, *f.Identity)
	}
	if f.Task != nil {
		refs = append(refs, *f.Task)
	}
	if model.CheckScope(c.Principal().Tenant, refs...) != nil {
		return engine.ErrNotFound()
	}
	if f.Labels != nil && f.Labels.CheckScope(c.Principal().Tenant) != nil {
		return engine.ErrNotFound()
	}
	return nil
}

func inInboxes(f model.Filter, in model.InboxRef) bool {
	return len(f.Inboxes) == 0 || slices.Contains(f.Inboxes, in)
}

func inConversation(f model.Filter, c model.ConversationRef) bool {
	return f.Conversation == nil || *f.Conversation == c
}

func hasStatus(f model.Filter, s string) bool {
	return len(f.Statuses) == 0 || slices.Contains(f.Statuses, s)
}

// hasLabels reports whether held satisfies Filter.Labels: at least one of
// Any, every one of All and none of None, each part only when set.
func hasLabels(f model.Filter, held []model.LabelRef) bool {
	sel := f.Labels
	if sel == nil {
		return true
	}
	holds := func(l model.LabelRef) bool { return slices.Contains(held, l) }
	return (len(sel.Any) == 0 || slices.ContainsFunc(sel.Any, holds)) &&
		!slices.ContainsFunc(sel.All, func(l model.LabelRef) bool { return !holds(l) }) &&
		!slices.ContainsFunc(sel.None, holds)
}

func inTask(f model.Filter, t *model.TaskRef) bool {
	return f.Task == nil || (t != nil && *t == *f.Task)
}

// Query cursors.
//
// A query cursor wraps the store's position for one query:
//
//	"q1." base64url( u32be(schema) || lp(position) || tag[16] )
//	tag = sha256( lp(domain) || u32be(1) || lp(tenant) || lp(actor) ||
//	              lp(query) || lp(filter) || lp(order) || lp(backend) ||
//	              u32be(schema) || lp(position) )[:16]
//
// where lp is a uvarint length prefix and filter the canonical encoding of
// the query's filter. The tag binds the position to tenant, actor, query,
// filter, order and store schema: a cursor altered, or replayed by another
// actor, for another query or filter, fails with model.Validation. A cursor
// issued under an older store schema is model.CursorExpired: the caller
// restarts the query. The tag is an integrity check, not a secret: every
// page reauthorizes, so a forged position can only reach items its holder
// may read anyway.
const (
	cursorPrefix  = "q1."
	cursorDomain  = "uma.query.cursor"
	cursorOrder   = "recorded"
	cursorTagSize = 16
	// maxStorePosition bounds the store cursor a query cursor may carry.
	maxStorePosition = 1024
)

type cursorBinding struct {
	tenant  model.TenantID
	actor   model.ActorID
	query   string
	filter  []byte
	backend string
	schema  uint32
}

func bindCursor(c *engine.Client, query string, f model.Filter) cursorBinding {
	p := c.Engine().Store().Properties()
	return cursorBinding{
		tenant: c.Principal().Tenant, actor: c.Principal().Actor, query: query,
		filter: canonicalFilter(f), backend: p.Backend, schema: p.SchemaVersion,
	}
}

func (b cursorBinding) tag(schema uint32, pos model.Cursor) []byte {
	var w operations.Writer
	w.Str(cursorDomain).U32(1).Str(string(b.tenant)).Str(string(b.actor)).Str(b.query).Raw(b.filter).
		Str(cursorOrder).Str(b.backend).U32(schema).Str(string(pos))
	sum := sha256.Sum256(w.Bytes())
	return sum[:cursorTagSize]
}

// encode wraps a store position; the empty position is the empty cursor.
func (b cursorBinding) encode(pos model.Cursor) model.Cursor {
	if pos == "" {
		return ""
	}
	var w operations.Writer
	w.U32(b.schema).Str(string(pos))
	raw := append(w.Bytes(), b.tag(b.schema, pos)...)
	return model.Cursor(cursorPrefix + base64.RawURLEncoding.EncodeToString(raw))
}

// decode returns the store position of a cursor this binding issued.
func (b cursorBinding) decode(c model.Cursor) (model.Cursor, error) {
	if c == "" {
		return "", nil
	}
	bad := model.NewError(model.Validation, "page.after: cursor is not valid for this query")
	s, ok := strings.CutPrefix(string(c), cursorPrefix)
	if !ok {
		return "", bad
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(raw) < 4+1+cursorTagSize {
		return "", bad
	}
	schema := binary.BigEndian.Uint32(raw)
	rest := raw[4:]
	n, k := binary.Uvarint(rest)
	if k <= 0 || n == 0 || n > maxStorePosition {
		return "", bad
	}
	size := int(n) // #nosec G115 -- n <= maxStorePosition
	if len(rest)-k != size+cursorTagSize {
		return "", bad
	}
	pos := model.Cursor(rest[k : k+size])
	tag := rest[k+size:]
	if subtle.ConstantTimeCompare(tag, b.tag(schema, pos)) != 1 {
		return "", bad
	}
	if schema != b.schema {
		return "", &model.Error{Code: model.CursorExpired, SafeMessage: "page.after: cursor predates the store schema; restart the query", Retry: model.NoRetry}
	}
	return pos, nil
}

// canonicalFilter encodes a filter independently of set order. Fields added
// after the first version (Identity, then the message search fields, then
// Labels, then Task, then Assignee, Snoozed, Linked and Unread, then
// Actor) are appended last, each only when set and each search field and
// each later field behind its name, so a filter without them encodes
// as it did before they existed and cursors issued for it stay valid.
// Equivalent values encode alike: Text as its sorted token set, times as
// the instant (Unix seconds and nanoseconds, whatever the location), media
// types and each label selector part sorted.
func canonicalFilter(f model.Filter) []byte {
	var w operations.Writer
	sorted := func(vs []string) {
		vs = slices.Clone(vs)
		slices.Sort(vs)
		w.Str(strconv.Itoa(len(vs)))
		for _, v := range vs {
			w.Str(v)
		}
	}
	// Filter references are in the bound tenant (inScope), so IDs suffice.
	inboxes := make([]string, len(f.Inboxes))
	for i, in := range f.Inboxes {
		inboxes[i] = string(in.ID)
	}
	sorted(inboxes)
	w.Present(f.Conversation != nil)
	if f.Conversation != nil {
		w.Ref(f.Conversation.Ref())
	}
	w.Present(f.Contact != nil)
	if f.Contact != nil {
		w.Ref(f.Contact.Ref())
	}
	sorted(f.Kinds)
	sorted(f.Statuses)
	if f.Identity != nil {
		w.Present(true).Ref(f.Identity.Ref())
	}
	instant := func(t time.Time) {
		w.Str(strconv.FormatInt(t.Unix(), 10)).Str(strconv.Itoa(t.Nanosecond()))
	}
	if f.Text != "" {
		w.Str("text")
		sorted(model.SearchTokens(f.Text))
	}
	if f.Since != nil {
		w.Str("since")
		instant(*f.Since)
	}
	if f.Before != nil {
		w.Str("before")
		instant(*f.Before)
	}
	if a := f.Attachments; a != nil {
		has := "-"
		if a.Has != nil {
			has = strconv.FormatBool(*a.Has)
		}
		w.Str("attachments").Str(has)
		sorted(a.MediaTypes)
	}
	if l := f.Labels; l != nil {
		// Selector labels are in the bound tenant (inScope): IDs suffice.
		w.Str("labels")
		for _, part := range [][]model.LabelRef{l.Any, l.All, l.None} {
			ids := make([]string, len(part))
			for i, ref := range part {
				ids[i] = string(ref.ID)
			}
			sorted(ids)
		}
	}
	if f.Task != nil {
		w.Str("task").Ref(f.Task.Ref())
	}
	if a := f.Assignee; a != nil {
		// Self is resolved to the bound actor before binding.
		w.Str("assignee").Str(strconv.FormatBool(a.None)).Str(string(a.Actor))
	}
	if f.Snoozed != nil {
		w.Str("snoozed").Str(strconv.FormatBool(*f.Snoozed))
	}
	if f.SnoozeEnded != nil {
		w.Str("snooze_ended").Str(strconv.FormatBool(*f.SnoozeEnded))
	}
	if f.Linked != nil {
		w.Str("linked").Str(strconv.FormatBool(*f.Linked))
	}
	if f.Unread != nil {
		// An empty actor is resolved to the bound actor before binding.
		w.Str("unread").Str(string(f.Unread.Actor))
	}
	if f.Actor != nil {
		w.Str("actor").Str(string(*f.Actor))
	}
	return bytes.Clone(w.Bytes())
}
