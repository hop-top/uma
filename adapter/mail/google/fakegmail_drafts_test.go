package google

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"hop.top/uma/adapter/mail/mime"
)

// fakeDraft is a draft the fake holds: stand-in for a Gmail Draft
// (https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.drafts).
// Its message ID changes on every update while its own ID is stable, as
// the drafts guide describes
// (https://developers.google.com/workspace/gmail/api/guides/drafts).
type fakeDraft struct {
	id, messageID, threadID string
	raw                     []byte
	msgID                   string // the Message-ID header
	updates                 int
}

// draftsAPI answers users.drafts.create (POST), update (PUT), delete
// (DELETE) and list (GET with q=rfc822msgid:<id>), stand-in for Gmail.
func (g *fakeGmail) draftsAPI(w http.ResponseWriter, r *http.Request, b *fakeBox, rest string) {
	id, _ := url.PathUnescape(strings.TrimPrefix(strings.TrimPrefix(rest, "/drafts"), "/"))
	g.mu.Lock()
	if b.drafts == nil {
		b.drafts = map[string]*fakeDraft{}
	}
	g.mu.Unlock()
	switch {
	case r.Method == "GET" && id == "":
		g.draftList(w, r, b)
	case r.Method == "GET":
		g.draftGet(w, r, b, id)
	case r.Method == "POST" && id == "", r.Method == "PUT" && id != "":
		g.draftWrite(w, r, b, id)
	case r.Method == "DELETE" && id != "":
		g.mu.Lock()
		_, ok := b.drafts[id]
		delete(b.drafts, id)
		g.mu.Unlock()
		if !ok {
			googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{})
	default:
		googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
	}
}

func (g *fakeGmail) draftWrite(w http.ResponseWriter, r *http.Request, b *fakeBox, id string) {
	var req gDraftBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(req.Message.Raw)
	var parsed *mime.Message
	if err == nil {
		parsed, err = mime.Parse(bytes.NewReader(raw), mime.Limits{})
	}
	if err != nil || len(parsed.Header.To) == 0 {
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
		return
	}
	g.mu.Lock()
	d, ok := b.drafts[id]
	if id != "" && !ok {
		g.mu.Unlock()
		googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
		return
	}
	b.seq++
	if !ok {
		b.draftSeq++
		d = &fakeDraft{id: fmt.Sprintf("r-%d", b.draftSeq)}
		b.drafts[d.id] = d
	} else {
		d.updates++
	}
	d.messageID, d.threadID, d.raw, d.msgID = fmt.Sprintf("dm-%d", b.seq), req.Message.ThreadID, raw, parsed.Header.MessageID
	lost := !ok && b.draftLost > 0
	if lost {
		b.draftLost--
	}
	out := gDraft{ID: d.id, Message: gMessage{ID: d.messageID, ThreadID: d.threadID, LabelIDs: []string{labelDraft}}}
	g.mu.Unlock()
	if lost {
		panic(http.ErrAbortHandler)
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *fakeGmail) draftList(w http.ResponseWriter, r *http.Request, b *fakeBox) {
	msgid, ok := strings.CutPrefix(r.URL.Query().Get("q"), "rfc822msgid:")
	if !ok {
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
		return
	}
	g.mu.Lock()
	list := gDraftList{Drafts: []gDraft{}}
	for _, d := range b.drafts {
		if d.msgID == "<"+msgid+">" {
			list.Drafts = append(list.Drafts, gDraft{ID: d.id, Message: gMessage{ID: d.messageID, ThreadID: d.threadID}})
		}
	}
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, list)
}

// draftGet answers users.drafts.get with format=metadata: the draft's
// message with the requested headers.
func (g *fakeGmail) draftGet(w http.ResponseWriter, r *http.Request, b *fakeBox, id string) {
	g.mu.Lock()
	d, ok := b.drafts[id]
	g.mu.Unlock()
	if !ok || r.URL.Query().Get("format") != "metadata" {
		googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
		return
	}
	m := gMessage{ID: d.messageID, ThreadID: d.threadID, LabelIDs: []string{labelDraft}, Payload: &gPart{}}
	for _, name := range r.URL.Query()["metadataHeaders"] {
		if v := headerValue(d.raw, name); v != "" {
			m.Payload.Headers = append(m.Payload.Headers, gHeader{Name: name, Value: v})
		}
	}
	writeJSON(w, http.StatusOK, gDraft{ID: d.id, Message: m})
}
