package microsoft

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Inbound provider state. The receiver runs one delta query per watched
// folder (the inbox, Archive and Deleted Items, plus Config.WatchedFolders)
// next to the inbox delta that delivers new mail, and reports each changed
// message's state as a conversation.state envelope: its folder (with the
// archive and trash roles), read state and follow-up flag. The report is
// partial (one message of the conversation, never Complete); ingress
// overlays it.
//
// A message moved from one watched folder to another shows as @removed in
// the first and as a change in the second: both reach the same state, read
// from the message itself, so a move is never read as a delete. A message
// that vanishes from a folder and cannot be read back (404) was deleted, and
// reports nothing: permanent deletion is not conversation state. A message
// moved to a folder that is not watched is read back by ID and reported
// there.
//
// Delta tracking of a folder starts as the inbox's does, from the whole
// folder: messages last modified before the initial sync window are
// skipped, later changes are all seen. A new inbox message the inbox delta
// delivers in the same round is not reported again while its state is the
// default (inbox, unread, unflagged).

// folderState is one watched folder.
type folderState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
	// DeltaLink resumes the folder's delta query: the last
	// @odata.deltaLink, or an @odata.nextLink while a round is paged.
	DeltaLink string `json:"delta_link,omitempty"`
}

// stateDeltaSelect is the $select of a folder state delta.
const stateDeltaSelect = "id,parentFolderId,isRead,isDraft,flag,importance,lastModifiedDateTime"

// watchNames are the folders watched for state: the well-known ones, then
// the configured.
func (i *instance) watchNames() []string {
	names := []string{folderInbox, folderArchive, folderTrash}
	for _, f := range i.cfg.WatchedFolders {
		if !slices.Contains(names, f) {
			names = append(names, f)
		}
	}
	return names
}

// discoverFolders resolves every watched folder and every well-known role
// folder to its ID, once: a folder a receiver already knows keeps its entry
// and delta link. A folder the mailbox does not have is left out.
func (i *instance) discoverFolders(ctx context.Context, s *session, st *receiverState) error {
	if st.Folders == nil {
		st.Folders = map[string]*folderState{}
	}
	for _, name := range append(i.watchNames(), wellKnownFolders()...) {
		if _, ok := st.Folders[name]; ok {
			continue
		}
		f, err := i.folder(ctx, s, name)
		if model.IsCode(err, model.NotFound) {
			continue
		}
		if err != nil {
			return err
		}
		st.Folders[name] = &folderState{ID: f.ID, Name: f.DisplayName, Role: folderRoles[name]}
	}
	// A configured ID that is a well-known folder's has its role.
	for _, f := range st.Folders {
		if f.Role != "" {
			continue
		}
		for name, role := range folderRoles {
			if w, ok := st.Folders[name]; ok && w.ID == f.ID {
				f.Role = role
			}
		}
	}
	return nil
}

// folderByID returns the folder with ID id from the discovered ones, else
// reads it (cached in known for the round).
func (i *instance) folderByID(ctx context.Context, s *session, st *receiverState, known map[string]*folderState, id string) (*folderState, error) {
	for _, f := range st.Folders {
		if f.ID == id {
			return f, nil
		}
	}
	if f, ok := known[id]; ok {
		return f, nil
	}
	f := &folderState{ID: id, Name: id}
	if g, err := i.folder(ctx, s, id); err == nil {
		f.Name = g.DisplayName
	} else if !model.IsCode(err, model.NotFound) {
		return nil, err
	}
	known[id] = f
	return f, nil
}

// syncFolders runs one delta round on each watched folder. fresh holds the
// IDs of the messages the inbox delta delivered this round.
func (i *instance) syncFolders(ctx context.Context, s *session, sink adapter.Sink, st *receiverState, fresh map[string]bool) error {
	if err := i.discoverFolders(ctx, s, st); err != nil {
		return err
	}
	known := map[string]*folderState{}
	reported := map[string]bool{}
	for _, name := range i.watchNames() {
		f, ok := st.Folders[name]
		if !ok {
			continue
		}
		if err := i.syncFolder(ctx, s, sink, st, f, known, fresh, reported); err != nil {
			return err
		}
	}
	return nil
}

// syncFolder runs delta rounds on f until Graph returns a delta link.
func (i *instance) syncFolder(ctx context.Context, s *session, sink adapter.Sink, st *receiverState, f *folderState, known map[string]*folderState, fresh, reported map[string]bool) error {
	since := i.now().Add(-i.cfg.initialSyncWindow())
	initial := f.DeltaLink == "" || !i.c.sameOrigin(f.DeltaLink)
	link := f.DeltaLink
	if initial {
		link = i.stateDeltaURL(f.ID)
	}
	restarted := false
	for {
		resp, err := s.do(ctx, request{
			method: "GET", url: link,
			prefer: []string{preferImmutableID, "odata.maxpagesize=" + strconv.Itoa(i.f.opts.PageSize)},
		})
		if err != nil && resp.status == http.StatusGone && !restarted {
			// The delta token expired: start a new bounded round.
			f.DeltaLink, link, restarted, initial = "", i.stateDeltaURL(f.ID), true, true
			continue
		}
		if model.IsCode(err, model.NotFound) {
			// The folder is gone: drop it, rediscovered next round.
			for name, g := range st.Folders {
				if g == f {
					delete(st.Folders, name)
				}
			}
			return nil
		}
		if err != nil {
			return transient(err)
		}
		var page gDeltaPage
		if err := decode(resp.body, &page); err != nil {
			return err
		}
		for _, item := range page.Value {
			if err := i.reportItem(ctx, s, sink, st, known, f, item, initial, since, fresh, reported); err != nil {
				return err
			}
		}
		next := page.NextLink
		if next == "" {
			next = page.DeltaLink
		}
		if next == "" || !i.c.sameOrigin(next) {
			return &model.Error{Code: model.Permanent, SafeMessage: "graph delta page without a usable link", Retry: model.NoRetry}
		}
		f.DeltaLink = next
		if err := i.saveState(ctx, *st); err != nil {
			return err
		}
		if page.NextLink == "" {
			return nil
		}
		link = next
	}
}

// stateDeltaURL is the first delta request of a folder with no stored link:
// the folder's messages with the state fields selected. Changes to every
// message of the folder follow, whenever it was received.
func (i *instance) stateDeltaURL(folderID string) string {
	q := url.Values{"$select": {stateDeltaSelect}}
	return i.c.api(i.cfg.mailboxPath()+"/mailFolders/"+url.PathEscape(folderID)+"/messages/delta", q)
}

// reportItem reports the state of one delta entry.
func (i *instance) reportItem(ctx context.Context, s *session, sink adapter.Sink, st *receiverState, known map[string]*folderState, f *folderState, item gDeltaItem, initial bool, since time.Time, fresh, reported map[string]bool) error {
	if item.ID == "" || reported[item.ID] {
		return nil
	}
	m := gMessage{
		ID: item.ID, ParentFolderID: item.ParentFolderID, IsRead: item.IsRead, IsDraft: item.IsDraft,
		Flag: item.Flag, Importance: item.Importance, LastModifiedDateTime: item.LastModifiedDateTime,
	}
	if item.Removed != nil || m.ParentFolderID == "" {
		// Moved out of the folder or deleted: where is it now?
		cur, err := i.stateOf(ctx, s, item.ID)
		if model.IsCode(err, model.NotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		m = cur
		m.ID = item.ID
	} else if initial && m.LastModifiedDateTime.Before(since) {
		return nil
	}
	if m.IsDraft || m.ParentFolderID == "" {
		return nil
	}
	folder, err := i.folderByID(ctx, s, st, known, m.ParentFolderID)
	if err != nil {
		return err
	}
	if fresh[m.ID] && folder.Role == model.RoleInbox && !m.IsRead && (m.Flag == nil || m.Flag.FlagStatus != flagFlagged) && m.Importance != importanceHigh {
		return nil // new mail in its default state: the message envelope says it
	}
	reported[m.ID] = true
	if _, err := sink.Accept(ctx, i.stateIngress(m, folder)); err != nil {
		return err
	}
	return nil
}

// stateIngress maps a message's state to a conversation.state envelope. The
// conversation is named by the provider message: ingress finds the one that
// holds it.
func (i *instance) stateIngress(m gMessage, f *folderState) model.Ingress {
	at := m.LastModifiedDateTime
	if at.IsZero() {
		at = i.now()
	}
	read := m.IsRead
	st := model.ProviderConversationState{
		Archived: f.Role == model.RoleArchive, Trashed: f.Role == model.RoleTrash,
		Folder: &model.Folder{ID: f.ID, Name: f.Name, Role: f.Role},
		Read:   &read, ObservedAt: at,
	}
	flagged := m.Flag != nil && m.Flag.FlagStatus == flagFlagged
	if flagged {
		st.Flags = append(st.Flags, model.FlagStar)
	}
	important := m.Importance == importanceHigh
	if important {
		st.Flags = append(st.Flags, model.FlagImportant)
	}
	id := strings.Join([]string{"state", m.ID, f.ID, strconv.FormatBool(read), strconv.FormatBool(flagged), strconv.FormatBool(important), strconv.FormatInt(at.UnixNano(), 10)}, ":")
	return model.Ingress{Envelope: model.Envelope{
		EventID: id, Destination: i.destination(gMessage{}), Kind: model.EnvelopeConversationState,
		SystemActor: true, Assurance: assuranceVerified, OccurredAt: at,
		State: &model.ProviderStateReport{Conversation: model.ProviderMessageRef(i.ch.Ref, m.ID), State: st},
	}}
}

// assuranceVerified is the assurance of a state read from Graph itself,
// over the authenticated API call.
const assuranceVerified = "verified"
