package imap

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"

	"hop.top/uma/model"
)

// What the server offers decides which state verbs are callable, and where a
// destination role lands: the extensions of the session (CAPABILITY) and the
// special-use attributes of its mailboxes (RFC 6154, LIST).
const (
	// infoTTL is how long what a session learned about the server stands;
	// a failed attempt stands for failedInfoTTL.
	infoTTL       = 10 * time.Minute
	failedInfoTTL = 30 * time.Second
	// discoverTimeout bounds the connection a capability report makes.
	discoverTimeout = 15 * time.Second
)

// serverInfo is what one session learned about the server.
type serverInfo struct {
	at time.Time
	// move: MOVE (RFC 6851); uidplus: UIDPLUS (RFC 4315), which gives COPYUID
	// and UID EXPUNGE; condstore: CONDSTORE (RFC 7162).
	move, uidplus, condstore bool
	// roles maps a destination role to the mailbox that holds it: inbox is
	// the retrieved mailbox, the others come from the special-use
	// attributes \Archive (else \All, where Gmail keeps archived mail),
	// \Trash and \Junk.
	roles map[string]string
	// err is the failure of the attempt, kept for failedInfoTTL.
	err error
}

// canMove reports whether a mailbox move can run safely: MOVE, or a COPY
// followed by a UID EXPUNGE of the one message copied. A plain EXPUNGE would
// remove every message marked \Deleted, so a server with neither is not
// moved on.
func (si serverInfo) canMove() bool { return si.move || si.uidplus }

// roleList returns the roles the adapter resolves, in declaration order.
func (si serverInfo) roleList() []string {
	var out []string
	for _, r := range []string{model.RoleInbox, model.RoleArchive, model.RoleTrash, model.RoleJunk} {
		if _, ok := si.roles[r]; ok {
			out = append(out, r)
		}
	}
	return out
}

// infoCache keeps a serverInfo per channel for the factory's instances, so a
// capability report does not connect each time.
type infoCache struct {
	mu sync.Mutex
	m  map[model.ChannelRef]serverInfo
}

func (c *infoCache) get(ch model.ChannelRef, now time.Time) (serverInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	si, ok := c.m[ch]
	if !ok {
		return serverInfo{}, false
	}
	ttl := infoTTL
	if si.err != nil {
		ttl = failedInfoTTL
	}
	return si, now.Sub(si.at) < ttl
}

func (c *infoCache) put(ch model.ChannelRef, si serverInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[model.ChannelRef]serverInfo{}
	}
	c.m[ch] = si
}

// learn reads what the session s shows about the server.
func (i *Instance) learn(ctx context.Context, s *session) (serverInfo, error) {
	caps := s.c.Caps()
	si := serverInfo{
		at:        i.now(),
		move:      caps.Has(imap.CapMove),
		uidplus:   caps.Has(imap.CapUIDPlus),
		condstore: caps.Has(imap.CapCondStore) || caps.Has(imap.CapQResync),
		roles:     map[string]string{model.RoleInbox: i.cfg.mailbox()},
	}
	opts := &imap.ListOptions{ReturnSpecialUse: caps.Has(imap.CapSpecialUse) || caps.Has(imap.CapIMAP4rev2)}
	var list []*imap.ListData
	if err := s.within(func() error {
		var err error
		list, err = s.c.List("", "*", opts).Collect()
		return err
	}); err != nil {
		return serverInfo{}, lost(ctx, err)
	}
	slices.SortFunc(list, func(a, b *imap.ListData) int { return strings.Compare(a.Mailbox, b.Mailbox) })
	var all string
	for _, d := range list {
		if slices.ContainsFunc(d.Attrs, func(a imap.MailboxAttr) bool { return strings.EqualFold(string(a), string(imap.MailboxAttrNoSelect)) }) {
			continue
		}
		for _, a := range d.Attrs {
			role := ""
			switch {
			case strings.EqualFold(string(a), string(imap.MailboxAttrArchive)):
				role = model.RoleArchive
			case strings.EqualFold(string(a), string(imap.MailboxAttrTrash)):
				role = model.RoleTrash
			case strings.EqualFold(string(a), string(imap.MailboxAttrJunk)):
				role = model.RoleJunk
			case strings.EqualFold(string(a), string(imap.MailboxAttrAll)) && all == "":
				all = d.Mailbox
			}
			if _, taken := si.roles[role]; role != "" && !taken {
				si.roles[role] = d.Mailbox
			}
		}
	}
	if _, ok := si.roles[model.RoleArchive]; !ok && all != "" {
		si.roles[model.RoleArchive] = all
	}
	i.f.info.put(i.ch.Ref, si)
	return si, nil
}

// serverInfo returns what is known of the server: the factory's cache, else
// what the session s shows when one is given, else what a session of its own
// does. A failure to learn is returned and remembered briefly.
func (i *Instance) serverInfo(ctx context.Context, s *session) (serverInfo, error) {
	if si, ok := i.f.info.get(i.ch.Ref, i.now()); ok {
		return si, si.err
	}
	if s == nil {
		if i.deps.Credentials == nil {
			return serverInfo{}, i.noCredentials(PurposeIMAP)
		}
		dctx, cancel := context.WithTimeout(ctx, discoverTimeout)
		defer cancel()
		own, err := i.connect(dctx)
		if err != nil {
			i.f.info.put(i.ch.Ref, serverInfo{at: i.now(), err: err})
			return serverInfo{}, err
		}
		defer own.close()
		s = own
	}
	si, err := i.learn(ctx, s)
	if err != nil {
		i.f.info.put(i.ch.Ref, serverInfo{at: i.now(), err: err})
		return serverInfo{}, err
	}
	return si, nil
}
