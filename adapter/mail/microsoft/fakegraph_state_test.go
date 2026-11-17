package microsoft

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// The fake's folders and provider state: well-known folders with the IDs
// Graph would give them, custom folders named by their ID, a change log per
// folder behind the folder delta, and the state verbs' calls (PATCH isRead
// and flag, POST move) with the immutable-ID behavior Microsoft Learn
// documents: a move answers the message under a new ID unless the request
// asked for immutable IDs.

// fchange is one entry of a folder's change log.
type fchange struct {
	id      string
	removed bool
}

// stateNote is what the harness saw the adapter request for a message: Graph
// sees the message and the change, not the operation or the route.
type stateNote struct {
	cap   model.Capability
	op    model.OperationRef
	route model.ExternalRef
}

var knownFolders = map[string]struct{ display, role string }{
	"inbox": {"Inbox", model.RoleInbox}, "archive": {"Archive", model.RoleArchive},
	"deleteditems": {"Deleted Items", model.RoleTrash}, "junkemail": {"Junk Email", model.RoleJunk},
	"drafts": {"Drafts", ""}, "sentitems": {"Sent Items", ""},
}

var stampSeq atomic.Int64

// fakeStamp is a last-modified time inside the initial sync window of the
// suite's clock, later each call.
func fakeStamp() time.Time {
	return time.Unix(1_700_000_000, 0).UTC().Add(time.Duration(stampSeq.Add(1)) * time.Millisecond)
}

// folderID is the ID of folder name: well-known folders get an opaque ID,
// custom folders are their own.
func folderID(name string) string {
	if _, ok := knownFolders[name]; ok {
		return "AAMk-" + name
	}
	return name
}

// canonFolder is the internal name of a folder named by alias or ID.
func canonFolder(x string) string {
	if n, ok := strings.CutPrefix(x, "AAMk-"); ok {
		if _, known := knownFolders[n]; known {
			return n
		}
	}
	return x
}

func folderDisplay(name string) string {
	if k, ok := knownFolders[name]; ok {
		return k.display
	}
	return name
}

// view is the message as Graph answers it, with its folder and times.
func (fm *fakeMessage) view() gMessage {
	m := fm.m
	m.ParentFolderID = folderID(fm.folder)
	m.LastModifiedDateTime = fm.modified
	return m
}

// touch records a change of fm in its folder's log. The caller holds g.mu.
func (mb *fakeMailbox) touch(fm *fakeMessage) {
	fm.modified = fakeStamp()
	mb.flog[fm.folder] = append(mb.flog[fm.folder], fchange{id: fm.m.ID})
}

// relocate moves fm to folder, logging the removal and the arrival. The
// caller holds g.mu.
func (mb *fakeMailbox) relocate(fm *fakeMessage, folder string) {
	if _, ok := knownFolders[folder]; !ok {
		mb.folders[folder] = true
	}
	mb.flog[fm.folder] = append(mb.flog[fm.folder], fchange{id: fm.m.ID, removed: true})
	fm.folder = folder
	mb.touch(fm)
}

func (g *fakeGraph) mailFolder(w http.ResponseWriter, mb *fakeMailbox, name string) {
	c := canonFolder(name)
	g.mu.Lock()
	_, known := knownFolders[c]
	custom := mb.folders[c]
	g.mu.Unlock()
	if !known && !custom {
		graphError(w, http.StatusNotFound, "ErrorItemNotFound")
		return
	}
	writeJSON(w, http.StatusOK, gMailFolder{ID: folderID(c), DisplayName: folderDisplay(c)})
}

// folderDelta answers a folder's delta: the inbox's created-messages query
// (changeType=created, or a token it issued) is the mail stream; any other
// is the state stream, whose tokens start with "s".
func (g *fakeGraph) folderDelta(w http.ResponseWriter, r *http.Request, mb *fakeMailbox, folder string) {
	q := r.URL.Query()
	tok := q.Get("$skiptoken") + q.Get("$deltatoken")
	if canonFolder(folder) == "inbox" && (q.Get("changeType") == "created" || (tok != "" && !strings.HasPrefix(tok, "s"))) {
		g.delta(w, r, mb)
		return
	}
	c := canonFolder(folder)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, known := knownFolders[c]; !known && !mb.folders[c] {
		graphError(w, http.StatusNotFound, "ErrorItemNotFound")
		return
	}
	items := []map[string]any{}
	item := func(fm *fakeMessage) map[string]any {
		v := fm.view()
		it := map[string]any{"id": v.ID, "parentFolderId": v.ParentFolderID, "isRead": v.IsRead, "isDraft": v.IsDraft, "lastModifiedDateTime": v.LastModifiedDateTime}
		if v.Flag != nil {
			it["flag"] = v.Flag
		}
		if v.Importance != "" {
			it["importance"] = v.Importance
		}
		return it
	}
	if tok == "" {
		for _, id := range mb.order {
			if fm, ok := mb.messages[id]; ok && fm.folder == c {
				items = append(items, item(fm))
			}
		}
	} else {
		pos, _ := strconv.Atoi(strings.TrimPrefix(tok, "s"))
		seen := map[string]bool{}
		for _, ch := range mb.flog[c][min(pos, len(mb.flog[c])):] {
			fm, ok := mb.messages[ch.id]
			if seen[ch.id] {
				continue
			}
			seen[ch.id] = true
			if !ok || fm.folder != c {
				items = append(items, map[string]any{"id": ch.id, "@removed": map[string]string{"reason": "deleted"}})
				continue
			}
			items = append(items, item(fm))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"value":            items,
		"@odata.deltaLink": g.srv.URL + r.URL.Path + "?$deltatoken=s" + strconv.Itoa(len(mb.flog[c])),
	})
}

// callOf builds the call a state request is, for message fm as the wire
// shows it. The caller holds g.mu.
func (g *fakeGraph) callOf(mb *fakeMailbox, fm *fakeMessage, c model.Capability) conformance.ProviderCall {
	n := mb.notes[fm.m.ID]
	if n.cap != "" {
		c = n.cap
	}
	return conformance.ProviderCall{
		Capability: c, Operation: n.op, Route: n.route,
		Items: []model.ExternalRef{model.ProviderMessageRef(mb.channel, fm.m.ID)},
	}
}

// nextResponse pops the scripted response of the mailbox. The caller holds
// g.mu.
func (mb *fakeMailbox) nextResponse() conformance.Response {
	resp := conformance.Response{Outcome: conformance.OutcomeAccepted}
	if len(mb.responses) > 0 {
		resp, mb.responses = mb.responses[0], mb.responses[1:]
	}
	return resp
}

// answer writes the scripted failure of resp, or reports false for an
// accepted one.
func answer(w http.ResponseWriter, resp conformance.Response) bool {
	switch resp.Outcome {
	case conformance.OutcomeRejected:
		graphError(w, http.StatusBadRequest, "ErrorInvalidRequest")
	case conformance.OutcomeTransient:
		graphError(w, http.StatusServiceUnavailable, "ServiceUnavailable")
	case conformance.OutcomeRateLimited:
		w.Header().Set("Retry-After", strconv.Itoa(int(resp.RetryAfter/time.Second)))
		graphError(w, http.StatusTooManyRequests, "TooManyRequests")
	case conformance.OutcomeRequestLost, conformance.OutcomeAnswerLost:
		panic(http.ErrAbortHandler) // the connection drops: no answer
	default:
		return false
	}
	return true
}

func applies(resp conformance.Response) bool {
	return resp.Outcome == conformance.OutcomeAccepted || resp.Outcome == conformance.OutcomeAnswerLost
}

// patchState answers a PATCH of isRead or flag.
func (g *fakeGraph) patchState(w http.ResponseWriter, r *http.Request, mb *fakeMailbox, fm *fakeMessage, fields map[string]json.RawMessage) {
	c := model.CapabilityReadState
	_, flag := fields["flag"]
	_, imp := fields["importance"]
	if flag || imp {
		c = model.CapabilityFlag
	}
	g.mu.Lock()
	resp := mb.nextResponse()
	call := g.callOf(mb, fm, c)
	if applies(resp) {
		if c == model.CapabilityReadState {
			var read bool
			_ = json.Unmarshal(fields["isRead"], &read)
			fm.m.IsRead, call.Read = read, read
		} else if imp {
			var v string
			_ = json.Unmarshal(fields["importance"], &v)
			fm.m.Importance, call.FlagKind, call.Undo = v, model.FlagImportant, v != importanceHigh
		} else {
			var f gFlag
			_ = json.Unmarshal(fields["flag"], &f)
			fm.m.Flag, call.FlagKind, call.Undo = &f, model.FlagStar, f.FlagStatus != flagFlagged
		}
		mb.touch(fm)
		call.Applied = true
	}
	mb.calls = append(mb.calls, call)
	m := fm.view()
	g.mu.Unlock()
	if resp.Outcome == conformance.OutcomeAnswerLost && call.Applied {
		panic(http.ErrAbortHandler)
	}
	if !answer(w, resp) {
		writeJSON(w, http.StatusOK, m)
	}
}

// move answers POST .../move. Without Prefer: IdType="ImmutableId" the moved
// message has a new ID and the old one names nothing: Graph's mutable IDs.
func (g *fakeGraph) move(w http.ResponseWriter, r *http.Request, mb *fakeMailbox, fm *fakeMessage) {
	var body gMove
	b, _ := io.ReadAll(r.Body)
	if json.Unmarshal(b, &body) != nil || body.DestinationID == "" {
		graphError(w, http.StatusBadRequest, "ErrorInvalidRequest")
		return
	}
	dest := canonFolder(body.DestinationID)
	immutable := slices.Contains(r.Header.Values("Prefer"), preferImmutableID)
	g.mu.Lock()
	resp := mb.nextResponse()
	call := g.callOf(mb, fm, model.CapabilityMove)
	if k, ok := knownFolders[dest]; ok && k.role != "" {
		call.Destination = model.MoveDestination{Role: k.role}
	} else {
		call.Destination = model.MoveDestination{ID: dest}
	}
	call.Undo = (call.Capability == model.CapabilityArchive || call.Capability == model.CapabilityTrash) && dest == "inbox"
	if applies(resp) {
		mb.relocate(fm, dest)
		call.Applied = true
		if !immutable {
			old := fm.m.ID
			fm.m.ID = old + "~moved"
			mb.messages[fm.m.ID] = fm
			delete(mb.messages, old)
			for n, id := range mb.order {
				if id == old {
					mb.order[n] = fm.m.ID
				}
			}
			mb.notes[fm.m.ID] = mb.notes[old]
			mb.events[fm.m.ID] = mb.events[old]
		}
		call.References = []model.ExternalRef{model.ProviderMessageRef(mb.channel, fm.m.ID)}
	}
	mb.calls = append(mb.calls, call)
	m := fm.view()
	g.mu.Unlock()
	if !answer(w, resp) {
		writeJSON(w, http.StatusCreated, m)
	}
}
