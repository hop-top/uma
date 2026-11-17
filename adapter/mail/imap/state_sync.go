package imap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Inbound state. The receiver follows the messages it delivered: it keeps,
// for each, the mailbox it sits in and its flags, and reports a change as a
// conversation.state envelope. It sees three things, none of which is a
// retrieval write (the session stays an EXAMINE):
//
//   - a flag change, by CONDSTORE (RFC 7162): FETCH ... (CHANGEDSINCE n)
//     returns only the messages changed since the mod-sequence stored with
//     the mailbox. A server without CONDSTORE is compared in full, at most
//     every Config.StateSyncIntervalMS.
//   - a message that left the mailbox, found by a UID SEARCH of the UIDs
//     held. Its Message-ID is looked up in the other mailboxes: found in
//     one, it was moved there and the report names that mailbox (and its
//     role, so archived and trashed follow); found nowhere, it was
//     expunged (or moved to a mailbox the adapter does not look in) and the
//     adapter reports the observation ObservationExpunged and forgets it.
//   - a copy of a message already known arriving in the mailbox (moved
//     back): the report names the mailbox it is in now.
//
// A report covers one message, so it is never Complete: the adapter cannot
// know the other messages of the conversation. The conversation is named by
// the message's Message-ID reference, which a stored message keeps whatever
// location references a move added.
const (
	ledgerKey     = "mailstate"
	ledgerSchema  = string(Type) + ".mailbox_state"
	ledgerVersion = 1
	// trackedLimit bounds the messages followed per mailbox: past it the
	// lowest UIDs, the oldest, are dropped.
	trackedLimit = 5000
	// ObservationExpunged is the observation kind reported for a message the
	// receiver followed that left its mailbox and was found in no other.
	ObservationExpunged = "imap.mail.message_expunged"
)

// entry is one message followed in a mailbox.
type entry struct {
	MessageID string `json:"id"`
	Seen      bool   `json:"seen,omitempty"`
	Flagged   bool   `json:"flagged,omitempty"`
}

// followed is the messages followed in one mailbox, under one UIDVALIDITY.
type followed struct {
	UIDValidity uint32 `json:"uid_validity"`
	// ModSeq is the HIGHESTMODSEQ the flags were last compared at; SyncedAt
	// is when a server without CONDSTORE was last compared.
	ModSeq   uint64           `json:"mod_seq,omitempty"`
	SyncedAt time.Time        `json:"synced_at,omitzero"`
	Messages map[uint32]entry `json:"messages"`
}

// ledger is what the receiver follows, by mailbox.
type ledger struct {
	Mailboxes map[string]*followed `json:"mailboxes"`
	dirty     bool
}

// memLedger is the factory-local ledger of one channel.
type memLedger struct {
	mu  sync.Mutex
	raw []byte
}

// box returns the followed set of mailbox under uidValidity, a new one
// when there is none or the UIDVALIDITY changed (every UID held is void).
func (l *ledger) box(mailbox string, uidValidity uint32) *followed {
	f := l.Mailboxes[mailbox]
	if f == nil || f.UIDValidity != uidValidity {
		if l.Mailboxes == nil {
			l.Mailboxes = map[string]*followed{}
		}
		f = &followed{UIDValidity: uidValidity, Messages: map[uint32]entry{}}
		l.Mailboxes[mailbox] = f
		l.dirty = true
	}
	return f
}

// set records e for uid, dropping the lowest UIDs past trackedLimit.
func (f *followed) set(uid uint32, e entry) {
	f.Messages[uid] = e
	if len(f.Messages) <= trackedLimit {
		return
	}
	uids := f.uids()
	for _, u := range uids[:len(uids)-trackedLimit] {
		delete(f.Messages, u)
	}
}

func (f *followed) uids() []uint32 {
	out := make([]uint32, 0, len(f.Messages))
	for u := range f.Messages {
		out = append(out, u)
	}
	slices.Sort(out)
	return out
}

// loadLedger returns the stored ledger, empty when there is none or it is
// unreadable (the receiver then follows what it delivers from now on).
func (i *Instance) loadLedger(ctx context.Context) (*ledger, error) {
	var raw []byte
	if i.deps.Protocol == nil {
		i.mem.led.mu.Lock()
		raw = i.mem.led.raw
		i.mem.led.mu.Unlock()
	} else {
		ext, _, err := i.deps.Protocol.Load(ctx, ledgerKey)
		switch {
		case model.IsCode(err, model.NotFound):
		case err != nil:
			return nil, err
		case ext.Name == ledgerSchema:
			raw = ext.Data
		}
	}
	l := &ledger{}
	if len(raw) == 0 || json.Unmarshal(raw, l) != nil {
		l = &ledger{}
	}
	return l, nil
}

// saveLedger persists l when it changed.
func (i *Instance) saveLedger(ctx context.Context, l *ledger) error {
	if !l.dirty {
		return nil
	}
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if i.deps.Protocol == nil {
		i.mem.led.mu.Lock()
		i.mem.led.raw = data
		i.mem.led.mu.Unlock()
		l.dirty = false
		return nil
	}
	_, rev, err := i.deps.Protocol.Load(ctx, ledgerKey)
	if err != nil && !model.IsCode(err, model.NotFound) {
		return err
	}
	if err := i.deps.Protocol.CompareAndSwap(ctx, ledgerKey, rev, model.Extension{Name: ledgerSchema, Version: ledgerVersion, Data: data}); err != nil {
		return err
	}
	l.dirty = false
	return nil
}

// roleOf is the role of mailbox: inbox for the retrieved one, else the
// role whose special-use mailbox it is, else none.
func (i *Instance) roleOf(si serverInfo, mailbox string) string {
	if mailbox == i.cfg.mailbox() {
		return model.RoleInbox
	}
	for _, r := range []string{model.RoleArchive, model.RoleTrash, model.RoleJunk} {
		if si.roles[r] == mailbox {
			return r
		}
	}
	return ""
}

// reportState sends the state of one message to the sink.
func (i *Instance) reportState(ctx context.Context, sink adapter.Sink, si serverInfo, mailbox string, e entry) error {
	now := i.now().UTC()
	role := i.roleOf(si, mailbox)
	st := model.ProviderConversationState{
		Archived: role == model.RoleArchive, Trashed: role == model.RoleTrash,
		Folder: &model.Folder{ID: mailbox, Name: mailbox, Role: role},
		Read:   &e.Seen, ObservedAt: now,
	}
	if e.Flagged {
		st.Flags = []string{model.FlagStar}
	}
	sum := sha256.Sum256([]byte(mailbox + "\x00" + e.MessageID + "\x00" + strconv.FormatBool(e.Seen) + strconv.FormatBool(e.Flagged) + "\x00" + strconv.FormatInt(now.UnixNano(), 10)))
	dest := i.cfg.destination()
	in := model.Ingress{Envelope: model.Envelope{
		EventID: "state:" + dest + ";" + hex.EncodeToString(sum[:]), Destination: dest,
		Kind: model.EnvelopeConversationState, SystemActor: true, OccurredAt: now,
		State: &model.ProviderStateReport{Conversation: model.ProviderMessageRef(i.ch.Ref, e.MessageID), State: st},
	}}
	actx, cancel := context.WithTimeout(ctx, sinkTimeout)
	defer cancel()
	if _, err := sink.Accept(actx, in); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &sinkError{err: err}
	}
	return nil
}

// syncView is the session during a state sync: where it is, and the
// SELECT answer the round got for the retrieved mailbox, which stands while
// the session stays in it. A second SELECT of the retrieved mailbox would
// make the server count mail that arrived since the round began as already
// reported, and the receiver would sleep through it.
type syncView struct {
	i       *Instance
	s       *session
	si      serverInfo
	primary string
	at      string
	sel     *imap.SelectData
}

// open selects box read-only, reusing the round's answer for the retrieved
// mailbox when the session is still in it. ok is false for a mailbox the
// server does not have.
func (v *syncView) open(ctx context.Context, box string) (*imap.SelectData, bool, error) {
	if box == v.primary && v.at == box && v.sel != nil {
		return v.sel, true, nil
	}
	sel, ok, err := v.i.selectBox(ctx, v.s, v.si, box)
	if err != nil {
		return nil, false, err
	}
	v.at = box
	if ok && box == v.primary {
		v.sel = sel
	}
	return sel, ok, nil
}

// syncState compares what the receiver follows with the server, mailbox by
// mailbox, the retrieved mailbox last. sel is what the round's SELECT of the
// retrieved mailbox answered and lastUID the highest UID delivered. The
// session ends in the retrieved mailbox, where the receiver waits; mail that
// arrived while it was elsewhere wakes the receiver again.
func (i *Instance) syncState(ctx context.Context, s *session, sink adapter.Sink, led *ledger, sel *imap.SelectData, lastUID uint32) error {
	v := &syncView{i: i, s: s, primary: i.cfg.mailbox(), at: i.cfg.mailbox(), sel: sel}
	touched := false
	for _, box := range append(slices.Clone(i.cfg.WatchMailboxes), v.primary) {
		if f := led.Mailboxes[box]; f == nil || len(f.Messages) == 0 {
			continue
		}
		if !touched {
			var err error
			if v.si, err = i.serverInfo(ctx, s); err != nil {
				return err
			}
		}
		touched = true
		if err := i.syncMailbox(ctx, v, sink, led, box); err != nil {
			return err
		}
	}
	if err := i.saveLedger(ctx, led); err != nil {
		return err
	}
	if !touched || v.at == v.primary {
		return nil
	}
	// Back in the retrieved mailbox: anything that arrived meanwhile is not
	// reported to the session, so look at what the server says now.
	now, ok, err := i.selectBox(ctx, s, v.si, v.primary)
	if err != nil {
		return err
	}
	if ok && now.UIDNext != 0 && uint32(now.UIDNext) > lastUID+1 {
		s.signal()
	}
	return nil
}

// selectBox opens mailbox read-only (with CONDSTORE where the server has
// it); ok is false for a mailbox the server does not have.
func (i *Instance) selectBox(ctx context.Context, s *session, si serverInfo, box string) (*imap.SelectData, bool, error) {
	var sel *imap.SelectData
	err := s.within(func() error {
		var err error
		sel, err = s.c.Select(box, &imap.SelectOptions{ReadOnly: true, CondStore: si.condstore}).Wait()
		return err
	})
	var ie *imap.Error
	switch {
	case errors.As(err, &ie) && ctx.Err() == nil:
		return nil, false, nil
	case err != nil:
		return nil, false, lost(ctx, err)
	}
	return sel, true, nil
}

func (i *Instance) syncMailbox(ctx context.Context, v *syncView, sink adapter.Sink, led *ledger, box string) error {
	s, si := v.s, v.si
	sel, ok, err := v.open(ctx, box)
	if err != nil || !ok {
		return err
	}
	f := led.Mailboxes[box]
	if f.UIDValidity != sel.UIDValidity {
		// Every UID held is void. The retrieved mailbox's receiver starts
		// again by itself (see Run); a message followed here is followed
		// again once it is delivered again.
		delete(led.Mailboxes, box)
		led.dirty = true
		return nil
	}
	uids := f.uids()
	set := imap.UIDSetNum(uidsOf(uids)...)
	var found *imap.SearchData
	if err := s.within(func() error {
		var err error
		found, err = s.c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}}, nil).Wait()
		return err
	}); err != nil {
		return lost(ctx, err)
	}
	present := map[uint32]bool{}
	for _, u := range found.AllUIDs() {
		present[uint32(u)] = true
	}
	var gone []uint32
	for _, u := range uids {
		if !present[u] {
			gone = append(gone, u)
		}
	}
	if err := i.syncFlags(ctx, s, sink, led, si, box, sel, f, present); err != nil {
		return err
	}
	return i.syncGone(ctx, v, sink, led, box, f, gone)
}

// syncFlags reports the messages still in the mailbox whose \Seen or
// \Flagged differs from what is held.
func (i *Instance) syncFlags(ctx context.Context, s *session, sink adapter.Sink, led *ledger, si serverInfo, box string, sel *imap.SelectData, f *followed, present map[uint32]bool) error {
	now := i.now()
	opts := &imap.FetchOptions{UID: true, Flags: true}
	switch {
	case si.condstore && f.ModSeq > 0 && sel.HighestModSeq > 0:
		if sel.HighestModSeq == f.ModSeq {
			return nil
		}
		opts.ChangedSince = f.ModSeq
	case si.condstore && sel.HighestModSeq > 0:
		// First comparison: no mod-sequence yet.
	case !f.SyncedAt.IsZero() && now.Sub(f.SyncedAt) < i.cfg.stateSyncInterval():
		return nil
	}
	var here []imap.UID
	for u := range present {
		here = append(here, imap.UID(u))
	}
	if len(here) == 0 {
		return nil
	}
	var msgs []*fetchedFlags
	if err := s.within(func() error {
		var err error
		msgs, err = collectFlagsOpts(s, imap.UIDSetNum(here...), opts)
		return err
	}); err != nil {
		return lost(ctx, err)
	}
	slices.SortFunc(msgs, func(a, b *fetchedFlags) int { return int(a.uid) - int(b.uid) })
	for _, m := range msgs {
		old, ok := f.Messages[m.uid]
		if !ok {
			continue
		}
		next := old
		next.Seen, next.Flagged = slices.Contains(m.flags, imap.FlagSeen), slices.Contains(m.flags, imap.FlagFlagged)
		if next == old {
			continue
		}
		if err := i.reportState(ctx, sink, si, box, next); err != nil {
			return err
		}
		f.Messages[m.uid] = next
		led.dirty = true
	}
	if si.condstore && sel.HighestModSeq > 0 {
		f.ModSeq = sel.HighestModSeq
	}
	f.SyncedAt = now
	led.dirty = true
	return nil
}

// syncGone classifies the messages that left box: moved, when found in
// another mailbox the adapter looks in, else expunged.
func (i *Instance) syncGone(ctx context.Context, v *syncView, sink adapter.Sink, led *ledger, box string, f *followed, gone []uint32) error {
	if len(gone) == 0 {
		return nil
	}
	s, si := v.s, v.si
	where := i.destinations(si, box)
	type place struct {
		box         string
		uidValidity uint32
		uid         uint32
		entry       entry
	}
	places := map[uint32]place{}
	for _, dest := range where {
		var pending []uint32
		for _, u := range gone {
			if _, done := places[u]; !done {
				pending = append(pending, u)
			}
		}
		if len(pending) == 0 {
			break
		}
		sel, ok, err := v.open(ctx, dest)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		for _, u := range pending {
			e := f.Messages[u]
			uids, err := s.uidsByMessageID(e.MessageID)
			if err != nil {
				return lost(ctx, err)
			}
			if len(uids) == 0 {
				continue
			}
			last := uids[len(uids)-1]
			var msgs []*fetchedFlags
			if err := s.within(func() error {
				var err error
				msgs, err = collectFlagsOpts(s, imap.UIDSetNum(last), &imap.FetchOptions{UID: true, Flags: true})
				return err
			}); err != nil {
				return lost(ctx, err)
			}
			if len(msgs) > 0 {
				e.Seen, e.Flagged = slices.Contains(msgs[0].flags, imap.FlagSeen), slices.Contains(msgs[0].flags, imap.FlagFlagged)
			}
			places[u] = place{box: dest, uidValidity: sel.UIDValidity, uid: uint32(last), entry: e}
		}
	}
	for _, u := range gone {
		p, moved := places[u]
		delete(f.Messages, u)
		led.dirty = true
		if !moved {
			i.observe(ctx, ObservationExpunged)
			continue
		}
		if err := i.reportState(ctx, sink, si, p.box, p.entry); err != nil {
			return err
		}
		if p.box == i.cfg.mailbox() || slices.Contains(i.cfg.WatchMailboxes, p.box) {
			led.box(p.box, p.uidValidity).set(p.uid, p.entry)
		}
	}
	return nil
}

// destinations lists the mailboxes a message that left box is looked for in:
// the special-use mailboxes of archive, trash and junk, the retrieved mailbox
// and the watched ones, box itself and the sent-copies mailbox excepted (a
// copy there says nothing about where the message went).
func (i *Instance) destinations(si serverInfo, box string) []string {
	var out []string
	add := func(b string) {
		if b != "" && b != box && b != i.cfg.Sent.Mailbox && !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	for _, r := range []string{model.RoleArchive, model.RoleTrash, model.RoleJunk} {
		add(si.roles[r])
	}
	add(i.cfg.mailbox())
	for _, w := range i.cfg.WatchMailboxes {
		add(w)
	}
	return out
}

// collectFlagsOpts fetches the flags of the UIDs of the selected mailbox with
// opts.
func collectFlagsOpts(s *session, set imap.UIDSet, opts *imap.FetchOptions) ([]*fetchedFlags, error) {
	msgs, err := s.c.Fetch(set, opts).Collect()
	if err != nil {
		return nil, err
	}
	var out []*fetchedFlags
	for _, m := range msgs {
		if m.UID == 0 {
			continue // volunteered data of another change, not the answer
		}
		out = append(out, &fetchedFlags{uid: uint32(m.UID), flags: m.Flags})
	}
	return out, nil
}
