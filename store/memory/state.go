package memory

import (
	"maps"
	"slices"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// entry is a stored value with its insertion sequence, the list order. at
// is the store-clock time of the write Purge judges, for the tables it
// purges.
type entry[V any] struct {
	seq uint64
	at  time.Time
	v   V
}

// table is a map shared with the snapshot it was forked from until the first
// write, which copies it. Stored values are never mutated in place.
type table[K comparable, V any] struct {
	m     map[K]entry[V]
	owned bool
}

func (t *table[K, V]) get(k K) (entry[V], bool) {
	e, ok := t.m[k]
	return e, ok
}

func (t *table[K, V]) own() {
	if !t.owned {
		t.m = maps.Clone(t.m)
		if t.m == nil {
			t.m = map[K]entry[V]{}
		}
		t.owned = true
	}
}

func (t *table[K, V]) set(k K, e entry[V]) {
	t.own()
	t.m[k] = e
}

func (t *table[K, V]) del(k K) {
	t.own()
	delete(t.m, k)
}

// sorted returns entries matching keep in insertion order.
func (t *table[K, V]) sorted(keep func(V) bool) []entry[V] {
	var out []entry[V]
	for _, e := range t.m {
		if keep == nil || keep(e.v) {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b entry[V]) int {
		switch {
		case a.seq < b.seq:
			return -1
		case a.seq > b.seq:
			return 1
		}
		return 0
	})
	return out
}

func (t *table[K, V]) fork() table[K, V] { return table[K, V]{m: t.m} }

// log is an append-only sequence shared with its snapshot until the first
// append, which copies it.
type log[V any] struct {
	items []V
	owned bool
}

func (l *log[V]) append(v ...V) {
	if !l.owned {
		l.items = slices.Clip(l.items)
		l.owned = true
	}
	l.items = append(l.items, v...)
}

// set replaces item i, copying the items on the first write.
func (l *log[V]) set(i int, v V) {
	if !l.owned {
		l.items = slices.Clone(l.items)
		l.owned = true
	}
	l.items[i] = v
}

func (l *log[V]) fork() log[V] { return log[V]{items: l.items} }

// drop removes the first n items.
func (l *log[V]) drop(n int) {
	l.items = slices.Clone(l.items[n:])
	l.owned = true
}

// eventMeta is what the log keeps beside one event: its store-clock write
// time, which retention judges it by; the conversation EventConversation
// resolves for it; and recordedMax, the latest RecordedAt of it and every
// event appended before it, which only grows along the log, so a Since
// filter can start at the first event whose recordedMax reaches Since.
type eventMeta struct {
	at           time.Time
	conversation model.ID
	recordedMax  time.Time
}

// attempt is one stored attempt evidence entry: its position, unique and
// increasing within the tenant, and its write time.
type attempt struct {
	pos uint64
	at  time.Time
	ev  store.AttemptEvidence
}

type claimKey struct {
	sel    store.SelectionKey
	create string
}

// messageClaimKey is a claimed message identity: a provider reference in
// one inbox.
type messageClaimKey struct {
	inbox model.InboxRef
	ref   model.ExternalRef
}

type protocolKey struct {
	channel model.ChannelRef
	key     string
}

type dedupKey struct {
	channel model.ChannelRef
	key     string
}

type evidenceKey struct {
	verifier, reference string
}

// state is one tenant's data. A committed state is immutable; a unit works on
// a fork.
type state struct {
	seq   uint64
	dirty bool

	channels      table[model.ChannelRef, model.Channel]
	inboxes       table[model.InboxRef, model.Inbox]
	contacts      table[model.ContactRef, model.Contact]
	identities    table[model.IdentityRef, model.Identity]
	identityKeys  table[model.IdentityKey, model.IdentityRef]
	conversations table[model.ConversationRef, model.Conversation]
	convClaims    table[claimKey, model.ConversationRef]
	convGroups    table[store.SelectionKey, []model.ConversationRef]
	messages      table[model.MessageRef, model.Message]
	msgClaims     table[messageClaimKey, model.MessageRef]
	workItems     table[model.WorkItemRef, model.WorkItem]
	tasks         table[model.TaskRef, model.Task]
	labels        table[model.LabelRef, model.Label]
	// labelNames maps each label's model.LabelNameKey to the label.
	labelNames    table[string, model.LabelRef]
	taskRefs      table[taskRefKey, model.TaskRef]
	streams       table[model.StreamRef, model.Stream]
	interactions  table[model.InteractionRef, store.InteractionRecord]
	attachments   table[model.AttachmentRef, store.AttachmentRecord]
	staging       table[ports.BlobKey, store.StagingIntent]
	exports       table[model.ExportRef, store.ExportRecord]
	protocol      table[protocolKey, store.ProtocolState]
	recipientLogs table[store.RecipientLogKey, recipientLog]
	ingress       table[model.ID, store.IngressRecord]
	ingressDedup  table[dedupKey, model.ID]
	operations    table[model.OperationRef, store.OperationRecord]
	operationKeys table[store.OperationKey, model.OperationRef]
	attempts      table[model.OperationRef, []attempt]
	subscriptions table[model.SubscriptionRef, store.SubscriptionRecord]
	work          table[model.ID, store.Work]
	quarantine    table[model.ID, struct{}]
	leases        table[model.Ref, store.Lease]
	generations   table[model.Ref, uint64]
	// eventIDs maps each retained event ID to its log position.
	eventIDs  table[model.ID, uint64]
	linkIDs   table[model.ID, struct{}]
	linkHeads table[model.IdentityRef, model.ID]
	evidence  table[evidenceKey, model.ID]
	// readMarkers holds each actor's read marker per conversation.
	readMarkers table[readMarkerKey, model.ReadMarker]

	events log[model.Event]
	// eventMeta holds the write time and the filter columns of each events
	// item.
	eventMeta log[eventMeta]
	// eventBase counts purged events: events.items[0] is at position
	// eventBase+1.
	eventBase uint64
	// eventRecordedMax is the latest RecordedAt of any event ever
	// appended, purged ones included: the running maximum the next append
	// continues.
	eventRecordedMax time.Time
	links            log[store.LinkRecord]
	// linkAt holds the write time of each links item.
	linkAt log[time.Time]
}

// fork returns a copy sharing every table with s until written.
func (s *state) fork() *state {
	return &state{
		seq:              s.seq,
		channels:         s.channels.fork(),
		inboxes:          s.inboxes.fork(),
		contacts:         s.contacts.fork(),
		identities:       s.identities.fork(),
		identityKeys:     s.identityKeys.fork(),
		conversations:    s.conversations.fork(),
		convClaims:       s.convClaims.fork(),
		convGroups:       s.convGroups.fork(),
		messages:         s.messages.fork(),
		msgClaims:        s.msgClaims.fork(),
		workItems:        s.workItems.fork(),
		tasks:            s.tasks.fork(),
		labels:           s.labels.fork(),
		labelNames:       s.labelNames.fork(),
		taskRefs:         s.taskRefs.fork(),
		streams:          s.streams.fork(),
		interactions:     s.interactions.fork(),
		attachments:      s.attachments.fork(),
		staging:          s.staging.fork(),
		exports:          s.exports.fork(),
		protocol:         s.protocol.fork(),
		recipientLogs:    s.recipientLogs.fork(),
		ingress:          s.ingress.fork(),
		ingressDedup:     s.ingressDedup.fork(),
		operations:       s.operations.fork(),
		operationKeys:    s.operationKeys.fork(),
		attempts:         s.attempts.fork(),
		subscriptions:    s.subscriptions.fork(),
		work:             s.work.fork(),
		quarantine:       s.quarantine.fork(),
		leases:           s.leases.fork(),
		generations:      s.generations.fork(),
		eventIDs:         s.eventIDs.fork(),
		linkIDs:          s.linkIDs.fork(),
		linkHeads:        s.linkHeads.fork(),
		evidence:         s.evidence.fork(),
		readMarkers:      s.readMarkers.fork(),
		events:           s.events.fork(),
		eventMeta:        s.eventMeta.fork(),
		eventBase:        s.eventBase,
		eventRecordedMax: s.eventRecordedMax,
		links:            s.links.fork(),
		linkAt:           s.linkAt.fork(),
	}
}

// next returns the next insertion sequence.
func (s *state) next() uint64 {
	s.seq++
	return s.seq
}
