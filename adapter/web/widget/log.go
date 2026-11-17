package widget

import (
	"context"
	"sync"
	"time"

	"hop.top/uma/model"
)

// Event types (Event.Type). EventReset is sent by the event stream only:
// the events after the client's cursor are no longer retained. A client
// ignores an event type it does not know.
const (
	EventMessage       = "message"
	EventMessageEdit   = "message.edit"
	EventMessageDelete = "message.delete"
	EventReaction      = "reaction"
	EventReceipt       = "receipt"
	EventEphemeral     = "ephemeral"
	EventStreamStart   = "stream.start"
	EventStreamUpdate  = "stream.update"
	EventStreamFinish  = "stream.finish"
	EventStreamCancel  = "stream.cancel"
	EventSignal        = "signal"
	EventReset         = "reset"
	// EventInteractionResponse answers an interaction the visitor sent;
	// see Event.
	EventInteractionResponse = "interaction.response"
)

// LogKey addresses one visitor's log on one channel: the identity
// namespace and source ID the visitor sends under.
type LogKey struct {
	Channel   model.ChannelRef
	Namespace string
	Visitor   string
}

// Event is one visitor-facing event, as the event stream sends it.
//
// Message is the provider message ID of a message, ephemeral reply or
// native stream (the stream's ID is its message ID), and the message an
// edit, a delete or a reaction applies to; it may be a message the visitor
// sent. A stream update and a finish carry the whole content so far,
// replacing the previous one, and the stream sequence. An edit carries the
// whole new content, replacing the message's. A delete carries no content.
// A reaction names Emoji, the host's reaction on the message, and Removed
// when the host withdrew it. Kind is a signal's kind, such as "typing", and
// a receipt's kind, model.ReceiptDelivered or model.ReceiptRead, the receipt
// being for the visitor's message named in Message.
//
// An interaction response names the interaction it answers in Interaction,
// the ID the receiver returned when it accepted the visitor's action or
// submission (AcceptedInteraction), and Mode, a model.ResponseMode* value.
// A message response is a new message, Message its ID. An ephemeral
// response is shown to this visitor only until the event's expires_at,
// Message its ID. An update replaces the whole content of the message the
// control sat on, Message its ID, like an edit. FollowUp marks a response
// after the initial one.
type Event struct {
	Type        string `json:"type"`
	Message     string `json:"message,omitempty"`
	Sequence    uint64 `json:"sequence,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Emoji       string `json:"emoji,omitempty"`
	Removed     bool   `json:"removed,omitempty"`
	Interaction string `json:"interaction,omitempty"`
	Mode        string `json:"mode,omitempty"`
	FollowUp    bool   `json:"follow_up,omitempty"`
	Parts       []Part `json:"parts,omitempty"`
}

// Part is one content part shown to a visitor. Attachment is an attachment
// ID the host serves; the widget never reads attachment bytes.
type Part struct {
	Kind       string           `json:"kind"`
	Text       string           `json:"text,omitempty"`
	Attachment model.ID         `json:"attachment,omitempty"`
	Structured *model.Extension `json:"structured,omitempty"`
}

// Entry is one event in a visitor's log.
type Entry struct {
	// Seq is the entry's position in its log, assigned by Append.
	Seq   uint64
	Event Event
	// Session limits the entry to the event streams of one session; empty
	// reaches every session of the visitor.
	Session string
	// Operation is the engine operation the entry delivers; zero for
	// signals.
	Operation model.OperationRef
	// ExpiresAt, when set, is the instant from which the entry is neither
	// sent nor replayed.
	ExpiresAt time.Time
	// Interaction is the engine interaction an interaction response
	// answers; zero for any other entry. It stays server side: the event
	// the visitor receives names the interaction by the ID the receiver
	// gave it.
	Interaction model.InteractionRef
}

// Log keeps visitor logs and wakes event streams. Implementations are safe
// for concurrent use. A host running receivers, workers and event streams
// in several processes supplies one shared Log.
//
// Append stores e under key with the next sequence, 1, 2, 3 and so on per
// key without gaps, wakes the key's watchers and returns the sequence; the
// Seq of e is ignored. Its errors decide the outbound outcome: a
// *model.Error with code Transient, Unavailable or RateLimited means
// nothing was stored and a retry is safe (RetryAfter is honoured); code
// OutcomeUnknown, an untyped error or a cancelled context means the entry
// may have been stored; any other *model.Error refuses the event for good.
//
// Read returns the entries of key with a sequence above after, in order, at
// most limit. A log may drop old entries: the first entry returned then
// has a sequence above after+1.
//
// Watch returns a channel that receives after every Append to key, with
// wake-ups coalesced, and a func that stops the watch.
//
// # Privacy erasure
//
// A visitor's log holds what was delivered to a person, so erasing the
// person's contact must erase it. uma does that only for a log it can
// reach: the storelog subpackage keeps each visitor log as a store recipient
// log addressed by the visitor's identity (channel, Namespace, Visitor), and
// Privacy.Redact with Cascade deletes it in the erasure's own unit. Any
// other Log, MemoryLog included, is outside uma's transaction and unknown to
// the engine: the host deletes the visitor's entries itself when it erases
// the contact. MemoryLog.Erase is that for MemoryLog.
type Log interface {
	Append(ctx context.Context, key LogKey, e Entry) (uint64, error)
	Read(ctx context.Context, key LogKey, after uint64, limit int) ([]Entry, error)
	Watch(key LogKey) (<-chan struct{}, func())
}

// DefaultRetain is the number of entries a MemoryLog keeps per visitor when
// NewMemoryLog is given zero.
const DefaultRetain = 256

// MemoryLog is an in-process Log keeping the latest entries of each
// visitor. It never forgets a visitor, so a long-running host bounds it
// itself or supplies its own Log. Its entries die with the process: use it
// for development and single-process hosts that accept losing visitor
// history on restart; see the package documentation, Durable delivery, and
// the storelog subpackage for a Log kept in the host's store.
type MemoryLog struct {
	retain int

	mu   sync.Mutex
	logs map[LogKey]*visitorLog
}

type visitorLog struct {
	next     uint64
	entries  []Entry
	watchers map[chan struct{}]struct{}
}

// NewMemoryLog returns an empty MemoryLog keeping retain entries per
// visitor; zero or less keeps DefaultRetain.
func NewMemoryLog(retain int) *MemoryLog {
	if retain <= 0 {
		retain = DefaultRetain
	}
	return &MemoryLog{retain: retain, logs: map[LogKey]*visitorLog{}}
}

func (m *MemoryLog) log(key LogKey) *visitorLog {
	l, ok := m.logs[key]
	if !ok {
		l = &visitorLog{watchers: map[chan struct{}]struct{}{}}
		m.logs[key] = l
	}
	return l
}

// Append stores e and wakes the key's watchers. It fails only when ctx has
// ended.
func (m *MemoryLog) Append(ctx context.Context, key LogKey, e Entry) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.log(key)
	l.next++
	e.Seq = l.next
	e.Event.Parts = cloneParts(e.Event.Parts)
	l.entries = append(l.entries, e)
	if over := len(l.entries) - m.retain; over > 0 {
		l.entries = append([]Entry(nil), l.entries[over:]...)
	}
	for w := range l.watchers {
		select {
		case w <- struct{}{}:
		default:
		}
	}
	return e.Seq, nil
}

// Read returns the retained entries after the cursor.
func (m *MemoryLog) Read(ctx context.Context, key LogKey, after uint64, limit int) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.logs[key]
	if !ok {
		return nil, nil
	}
	var out []Entry
	for _, e := range l.entries {
		if e.Seq <= after {
			continue
		}
		if limit > 0 && len(out) == limit {
			break
		}
		e.Event.Parts = cloneParts(e.Event.Parts)
		out = append(out, e)
	}
	return out, nil
}

// Erase deletes the retained entries of key and returns how many it
// deleted. The log keeps counting, so the next Append takes the next
// sequence and a stream holding an older cursor finds the gap. It is how a
// host erases a visitor from a MemoryLog: privacy erasure in uma cannot
// reach a log outside its store (see Log).
func (m *MemoryLog) Erase(key LogKey) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.logs[key]
	if !ok {
		return 0
	}
	n := len(l.entries)
	l.entries = nil
	return n
}

// Watch registers a watcher of key.
func (m *MemoryLog) Watch(key LogKey) (<-chan struct{}, func()) {
	w := make(chan struct{}, 1)
	m.mu.Lock()
	m.log(key).watchers[w] = struct{}{}
	m.mu.Unlock()
	var once sync.Once
	return w, func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.log(key).watchers, w)
			m.mu.Unlock()
		})
	}
}

func cloneParts(ps []Part) []Part {
	if ps == nil {
		return nil
	}
	out := make([]Part, len(ps))
	for i, p := range ps {
		if p.Structured != nil {
			s := p.Structured.Clone()
			p.Structured = &s
		}
		out[i] = p
	}
	return out
}

var _ Log = (*MemoryLog)(nil)
