package conformance

import (
	"context"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// matrixRead is one paged read the filter and cursor matrices exercise.
type matrixRead struct {
	name string
	// honours names the filter fields the read honours, as the store
	// package documents them. Every other field is model.Validation. A read
	// that takes no filter is not in the filter matrix.
	honours  []string
	filtered bool
	// read pages the read once. Reads that take no filter ignore f.
	read func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error)
	// add commits one more item the unfiltered read returns.
	add func(t *testing.T, e *env, w *matrixWorld)
}

// readView is the part of a page every paged read shares: the key of each
// item, in the order returned.
type readView struct {
	keys     []string
	next     model.Cursor
	complete bool
}

func viewOf[V any](p model.Page[V], err error, key func(V) model.ID) (readView, error) {
	if err != nil {
		return readView{}, err
	}
	v := readView{next: p.Next, complete: p.Complete}
	for _, it := range p.Items {
		v.keys = append(v.keys, string(key(it)))
	}
	return v, nil
}

func listReq(f model.Filter, p model.PageRequest) model.ListRequest {
	return model.ListRequest{Filter: f, Page: p}
}

// filteredReads is every read that takes a model.Filter. honours mirrors
// the filter matrix in the store package documentation.
var filteredReads = []matrixRead{
	{name: "Channels", filtered: true,
		read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
			pg, err := r.Channels(w.ctx, listReq(f, p))
			return viewOf(pg, err, func(v model.Channel) model.ID { return v.Ref.ID })
		},
		add: func(t *testing.T, e *env, _ *matrixWorld) {
			ref := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
			e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, e.channelRecord(ref, "r1"), "") })
		}},
	{name: "Labels", filtered: true,
		read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
			pg, err := r.Labels(w.ctx, listReq(f, p))
			return viewOf(pg, err, func(v model.Label) model.ID { return v.Ref.ID })
		},
		add: func(t *testing.T, e *env, _ *matrixWorld) { e.putLabel(t, "matrix-added-"+string(e.id(t))) }},
	{name: "Inboxes", filtered: true, honours: []string{"inboxes"},
		read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
			pg, err := r.Inboxes(w.ctx, listReq(f, p))
			return viewOf(pg, err, func(v model.Inbox) model.ID { return v.Ref.ID })
		},
		add: func(t *testing.T, e *env, _ *matrixWorld) { e.putInbox(t) }},
	{name: "Contacts", filtered: true, honours: []string{"kinds"},
		read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
			pg, err := r.Contacts(w.ctx, listReq(f, p))
			return viewOf(pg, err, func(v model.Contact) model.ID { return v.Ref.ID })
		},
		add: func(t *testing.T, e *env, _ *matrixWorld) { e.putContact(t) }},
	{name: "Identities", filtered: true, honours: []string{"inboxes", "contact"},
		read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
			pg, err := r.Identities(w.ctx, listReq(f, p))
			return viewOf(pg, err, func(v model.Identity) model.ID { return v.Ref.ID })
		},
		add: func(t *testing.T, e *env, _ *matrixWorld) { e.provisional(t) }},
	{name: "Conversations", filtered: true, honours: []string{"inboxes", "statuses", "labels", "assignee", "snoozed", "snooze_ended", "linked", "unread"},
		read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
			pg, err := r.Conversations(w.ctx, listReq(f, p))
			return viewOf(pg, err, func(v model.Conversation) model.ID { return v.Ref.ID })
		},
		add: func(t *testing.T, e *env, w *matrixWorld) { w.putConv(t, e, w.i1, model.ConversationOpen, nil, nil) }},
	{name: "Messages", filtered: true,
		honours: []string{"inboxes", "conversation", "labels", "task", "text", "since", "before", "attachments"},
		read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
			pg, err := r.Messages(w.ctx, listReq(f, p))
			return viewOf(pg, err, func(v model.Message) model.ID { return v.Ref.ID })
		},
		add: func(t *testing.T, e *env, w *matrixWorld) {
			e.putSearchMessage(t, w.cv1, e.at(50), textParts("added")...)
		}},
	{name: "WorkItems", filtered: true, honours: []string{"inboxes", "conversation", "statuses"},
		read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
			pg, err := r.WorkItems(w.ctx, listReq(f, p))
			return viewOf(pg, err, func(v model.WorkItem) model.ID { return v.Ref.ID })
		},
		add: func(t *testing.T, e *env, w *matrixWorld) {
			// A conversation holds at most one work item.
			conv := w.putConv(t, e, w.i1, model.ConversationOpen, nil, nil)
			w.putWork(t, e, conv, "open")
		}},
	{name: "Tasks", filtered: true, honours: []string{"inboxes", "conversation", "statuses", "identity"},
		read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
			pg, err := r.Tasks(w.ctx, listReq(f, p))
			return viewOf(pg, err, func(v model.Task) model.ID { return v.Ref.ID })
		},
		add: func(t *testing.T, e *env, w *matrixWorld) { e.putTask(t, w.cv1, model.TaskWorking, e.at(100)) }},
	{name: "Interactions", filtered: true, honours: []string{"inboxes", "conversation", "identity"},
		read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
			pg, err := r.Interactions(w.ctx, listReq(f, p))
			return viewOf(pg, err, func(v model.Interaction) model.ID { return v.Ref.ID })
		},
		add: func(t *testing.T, e *env, w *matrixWorld) { w.putInteraction(t, e, w.cv1, w.idA.Ref) }},
	{name: "Events", filtered: true, honours: []string{"inboxes", "conversation", "kinds", "since"},
		read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
			pg, err := r.Events(w.ctx, f, p.After, p.Limit)
			return viewOf(pg, err, func(v model.Event) model.ID { return v.ID })
		},
		add: func(t *testing.T, e *env, _ *matrixWorld) {
			e.mustTx(t, func(tx store.Tx) error {
				return tx.AppendEvents(e.ctx, []model.Event{e.event(t, "matrix.added")})
			})
		}},
	operationsRead,
}

// matrixRow is one filter value and the IDs a read honouring it returns.
type matrixRow struct {
	filter model.Filter
	want   []model.ID
}

// matrixWorld is one tenant seeded so every honoured filter field splits
// every read's items into a non-empty proper subset.
type matrixWorld struct {
	ctx           context.Context
	i1, i2        model.InboxRef
	idA, idB      model.Identity
	l1, l2        model.LabelRef
	cv1, cv2, cv3 model.ConversationRef
	tk1           model.TaskRef
	att           model.AttachmentRef
	rows          map[string]map[string]matrixRow
}

// row records that read, filtered by field to filter, returns want.
func (w *matrixWorld) row(read, field string, f model.Filter, want ...model.ID) {
	if w.rows[read] == nil {
		w.rows[read] = map[string]matrixRow{}
	}
	w.rows[read][field] = matrixRow{filter: f, want: want}
}

// seedMatrixWorld stores the world in tenantA. The fixture inbox is I1.
func (e *env) seedMatrixWorld(t *testing.T) *matrixWorld {
	t.Helper()
	w := &matrixWorld{ctx: e.ctx, rows: map[string]map[string]matrixRow{}}
	w.i1 = e.inbox
	w.i2 = e.putInbox(t)
	w.l1, w.l2 = e.putLabel(t, "matrix-one"), e.putLabel(t, "matrix-two")

	claim := func(inbox model.InboxRef, source, kind string) model.Identity {
		key := model.IdentityKey{Inbox: inbox, Namespace: "conformance.user", SourceID: source}
		id := e.identity(t, key)
		id.Ref.Inbox, id.Kind = inbox, kind
		e.mustTx(t, func(tx store.Tx) error {
			if _, _, err := tx.ClaimIdentity(e.ctx, key, id); err != nil {
				return err
			}
			return tx.PutContact(e.ctx, model.Contact{Ref: id.Contact, Kind: kind, Revision: "c1"}, "")
		})
		return id
	}
	w.idA = claim(w.i1, "matrix-a", model.ContactKindPerson)
	w.idB = claim(w.i2, "matrix-b", model.ContactKindBot)
	spare := model.ContactRef{Tenant: tenantA, ID: e.id(t)}
	e.mustTx(t, func(tx store.Tx) error {
		return tx.PutContact(e.ctx, model.Contact{Ref: spare, Kind: model.ContactKindAgent, Revision: "c1"}, "")
	})
	second := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, e.channelRecord(second, "r1"), "") })

	w.cv1 = w.putConv(t, e, w.i1, model.ConversationOpen, &w.idA.Ref, []model.LabelRef{w.l1})
	w.cv2 = w.putConv(t, e, w.i1, model.ConversationClosed, nil, nil)
	w.endSnooze(t, e, w.cv1)
	w.assignAndSnooze(t, e, w.cv2)
	w.linkTo(t, e, w.cv2, model.ConversationLink{Conversation: &w.cv1})
	w.cv3 = w.putConv(t, e, w.i2, model.ConversationOpen, &w.idB.Ref, []model.LabelRef{w.l2})

	w.tk1 = e.putTask(t, w.cv1, model.TaskSubmitted, e.at(1))
	tk4 := e.putTask(t, w.cv1, model.TaskCompleted, e.at(2))
	tk2 := e.putTask(t, w.cv2, model.TaskWorking, e.at(3))
	tk3 := e.putTask(t, w.cv3, model.TaskWorking, e.at(4))

	w.att = e.putSearchAttachment(t, "image/png")
	put := func(conv model.ConversationRef, at int, task *model.TaskRef, parts ...model.Part) model.ID {
		ref := model.MessageRef{Conversation: conv, ID: e.id(t)}
		m := e.searchMessage(ref, e.at(at), parts...)
		m.Task = task
		e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, m, "") })
		return ref.ID
	}
	m1 := put(w.cv1, 10, nil, textParts("alpha")...)
	m2 := put(w.cv2, 11, nil, textParts("beta")...)
	m3 := put(w.cv3, 12, nil, textParts("gamma")...)
	m4 := put(w.cv1, 13, &w.tk1, textParts("delta")...)
	m5 := put(w.cv2, 14, nil, attachmentParts(w.att)...)

	wi1 := w.putWork(t, e, w.cv1, "open")
	wi2 := w.putWork(t, e, w.cv2, "closed")
	wi3 := w.putWork(t, e, w.cv3, "open")

	ix1 := w.putInteraction(t, e, w.cv1, w.idA.Ref)
	ix2 := w.putInteraction(t, e, w.cv2, model.IdentityRef{Inbox: w.i1, ID: e.id(t)})
	ix3 := w.putInteraction(t, e, w.cv3, w.idB.Ref)

	ev1, ev2, ev3 := e.event(t, "matrix.one"), e.event(t, "matrix.two"), e.event(t, "matrix.one")
	ev2.Inbox, ev3.Inbox = w.i2.ID, w.i2.ID
	ev1.Resource, ev2.Resource, ev3.Resource = w.cv1.Ref(), w.cv3.Ref(), w.cv3.Ref()
	// RecordedAt does not follow the log: ev3 was recorded before ev2.
	ev1.RecordedAt, ev2.RecordedAt, ev3.RecordedAt = e.at(1), e.at(3), e.at(2)
	e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, []model.Event{ev1, ev2, ev3}) })

	in2 := model.Filter{Inboxes: []model.InboxRef{w.i2}}
	w.row("Inboxes", "inboxes", in2, w.i2.ID)
	w.row("Contacts", "kinds", model.Filter{Kinds: []string{model.ContactKindBot}}, w.idB.Contact.ID)
	w.row("Identities", "inboxes", in2, w.idB.Ref.ID)
	w.row("Identities", "contact", model.Filter{Contact: &w.idA.Contact}, w.idA.Ref.ID)
	w.row("Conversations", "inboxes", in2, w.cv3.ID)
	w.row("Conversations", "statuses", model.Filter{Statuses: []string{model.ConversationClosed}}, w.cv2.ID)
	w.row("Conversations", "labels", model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{w.l1}}}, w.cv1.ID)
	w.row("Conversations", "assignee", model.Filter{Assignee: &model.AssigneeFilter{Actor: matrixAssignee}}, w.cv2.ID)
	snoozed := true
	w.row("Conversations", "snoozed", model.Filter{Snoozed: &snoozed}, w.cv2.ID)
	w.row("Conversations", "snooze_ended", model.Filter{SnoozeEnded: &snoozed}, w.cv1.ID)
	w.row("Conversations", "linked", model.Filter{Linked: &snoozed}, w.cv2.ID)
	// matrixAssignee has read everything in cv1 and cv3, nothing in cv2.
	e.mustTx(t, func(tx store.Tx) error {
		return firstErr(
			tx.PutReadMarker(e.ctx, model.ReadMarker{Conversation: w.cv1, Actor: matrixAssignee, ReadThrough: e.at(1000), Revision: "m1"}, ""),
			tx.PutReadMarker(e.ctx, model.ReadMarker{Conversation: w.cv3, Actor: matrixAssignee, ReadThrough: e.at(1000), Revision: "m1"}, ""),
		)
	})
	w.row("Conversations", "unread", model.Filter{Unread: &model.UnreadFilter{Actor: matrixAssignee}}, w.cv2.ID)
	w.row("Messages", "inboxes", in2, m3)
	w.row("Messages", "conversation", model.Filter{Conversation: &w.cv2}, m2, m5)
	w.row("Messages", "labels", model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{w.l1}}}, m1, m4)
	w.row("Messages", "task", model.Filter{Task: &w.tk1}, m4)
	w.row("Messages", "text", model.Filter{Text: "gamma"}, m3)
	since, before := e.at(13), e.at(11)
	w.row("Messages", "since", model.Filter{Since: &since}, m4, m5)
	w.row("Messages", "before", model.Filter{Before: &before}, m1)
	yes := true
	w.row("Messages", "attachments", model.Filter{Attachments: &model.AttachmentFilter{Has: &yes}}, m5)
	w.row("WorkItems", "inboxes", in2, wi3)
	w.row("WorkItems", "conversation", model.Filter{Conversation: &w.cv2}, wi2)
	w.row("WorkItems", "statuses", model.Filter{Statuses: []string{"closed"}}, wi2)
	_ = wi1
	w.row("Tasks", "inboxes", in2, tk3.ID)
	w.row("Tasks", "conversation", model.Filter{Conversation: &w.cv2}, tk2.ID)
	w.row("Tasks", "statuses", model.Filter{Statuses: []string{model.TaskWorking}}, tk2.ID, tk3.ID)
	w.row("Tasks", "identity", model.Filter{Identity: &w.idA.Ref}, w.tk1.ID, tk4.ID)
	w.row("Interactions", "inboxes", in2, ix3)
	w.row("Interactions", "conversation", model.Filter{Conversation: &w.cv2}, ix2)
	w.row("Interactions", "identity", model.Filter{Identity: &w.idA.Ref}, ix1)
	w.row("Events", "inboxes", in2, ev2.ID, ev3.ID)
	w.row("Events", "kinds", model.Filter{Kinds: []string{"matrix.one"}}, ev1.ID, ev3.ID)
	w.row("Events", "conversation", model.Filter{Conversation: &w.cv3}, ev2.ID, ev3.ID)
	evSince := e.at(3)
	w.row("Events", "since", model.Filter{Since: &evSince}, ev2.ID)
	w.seedOperations(t, e, in2)
	return w
}

// putConv stores an agent conversation, so tasks may hang off it.
func (w *matrixWorld) putConv(t *testing.T, e *env, inbox model.InboxRef, status string, owner *model.IdentityRef, labels []model.LabelRef) model.ConversationRef {
	t.Helper()
	ref := model.ConversationRef{Inbox: inbox, ID: e.id(t)}
	c := e.agentConversation(ref, owner, "r1")
	c.Status = status
	if len(labels) > 0 {
		c.Labels = sortedLabels(labels...)
	}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, c, "") })
	return ref
}

// assignAndSnooze assigns conv (stored at r1) to matrixAssignee and snoozes
// it for a day of store-clock time, set after every message the world
// records.
func (w *matrixWorld) assignAndSnooze(t *testing.T, e *env, conv model.ConversationRef) {
	t.Helper()
	e.mustTx(t, func(tx store.Tx) error {
		c, err := tx.GetConversation(e.ctx, conv)
		if err != nil {
			return err
		}
		desk := matrixAssignee
		c.Assignee = &desk
		c.Snooze = &model.Snooze{Until: e.fx.Now().Add(24 * time.Hour).UTC(), At: e.at(1000), By: desk}
		c.Revision = "r2"
		return tx.PutConversation(e.ctx, c, "r1")
	})
}

// endSnooze gives conv (stored at r1) a snooze that ended an hour of
// store-clock time ago.
func (w *matrixWorld) endSnooze(t *testing.T, e *env, conv model.ConversationRef) {
	t.Helper()
	e.mustTx(t, func(tx store.Tx) error {
		c, err := tx.GetConversation(e.ctx, conv)
		if err != nil {
			return err
		}
		c.Snooze = &model.Snooze{Until: e.fx.Now().Add(-time.Hour).UTC(), At: e.at(0), By: matrixAssignee}
		c.Revision = "r2"
		return tx.PutConversation(e.ctx, c, "r1")
	})
}

// linkTo adds link to conv (stored at r2).
func (w *matrixWorld) linkTo(t *testing.T, e *env, conv model.ConversationRef, link model.ConversationLink) {
	t.Helper()
	e.mustTx(t, func(tx store.Tx) error {
		c, err := tx.GetConversation(e.ctx, conv)
		if err != nil {
			return err
		}
		c.Links = []model.ConversationLink{link}
		c.Revision = "r3"
		return tx.PutConversation(e.ctx, c, "r2")
	})
}

func (w *matrixWorld) putWork(t *testing.T, e *env, conv model.ConversationRef, status string) model.ID {
	t.Helper()
	ref := model.WorkItemRef{Conversation: conv, ID: e.id(t)}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutWorkItem(e.ctx, e.workItem(ref, status, "r1"), "") })
	return ref.ID
}

func (w *matrixWorld) putInteraction(t *testing.T, e *env, conv model.ConversationRef, actor model.IdentityRef) model.ID {
	t.Helper()
	rec := e.interactionRecord(t, conv, "")
	rec.Interaction.Actor.Identity = actor
	e.mustTx(t, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, rec) })
	return rec.Interaction.Ref.ID
}

// readAll pages read under f with the given page size to its end.
func (w *matrixWorld) readAll(t *testing.T, e *env, rd matrixRead, f model.Filter, limit int) []string {
	t.Helper()
	var keys []string
	var after model.Cursor
	for range 200 {
		var v readView
		e.mustView(t, func(r store.Reader) error {
			var err error
			v, err = rd.read(w, r, f, model.PageRequest{Limit: limit, After: after})
			return err
		})
		keys = append(keys, v.keys...)
		if v.complete {
			return keys
		}
		if v.next == "" || v.next == after {
			t.Fatalf("%s: incomplete page after %q returned next %q, which does not advance", rd.name, after, v.next)
		}
		after = v.next
	}
	t.Fatalf("%s: paging did not complete", rd.name)
	return nil
}

func sortedKeys(keys []string) []string {
	out := slices.Clone(keys)
	slices.Sort(out)
	return out
}

func idStrings(ids []model.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}
