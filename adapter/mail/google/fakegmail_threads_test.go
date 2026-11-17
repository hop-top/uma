package google

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// threads answers the users.threads calls the adapter makes: get with
// format=minimal or metadata, and the three mutations modify, trash and
// untrash, each applied to every message of the thread
// (https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.threads).
// A mutation takes the next scripted response of the mailbox, as a send
// does, and records the call the conformance suite reads, with the state
// request the harness noted for a message of the thread. Unlike Gmail the
// fake takes any label ID on a thread modify, so the suite can name labels
// no mailbox holds.
func (g *fakeGmail) threads(w http.ResponseWriter, r *http.Request, b *fakeBox, rest string) {
	id, verb, _ := strings.Cut(rest, "/")
	id, _ = url.PathUnescape(id)
	switch {
	case r.Method == "GET" && verb == "":
		g.threadGet(w, r, b, id)
	case r.Method == "POST" && (verb == "modify" || verb == "trash" || verb == "untrash"):
		g.threadMutate(w, r, b, id, verb)
	default:
		googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
	}
}

// threadMessages are the messages of thread id, oldest first; g.mu is held.
func (g *fakeGmail) threadMessages(b *fakeBox, id string) []*fakeMsg {
	var out []*fakeMsg
	for _, mid := range b.order {
		if m, ok := b.messages[mid]; ok && m.threadID == id {
			out = append(out, m)
		}
	}
	return out
}

func (g *fakeGmail) threadGet(w http.ResponseWriter, r *http.Request, b *fakeBox, id string) {
	q := r.URL.Query()
	g.mu.Lock()
	msgs := g.threadMessages(b, id)
	g.mu.Unlock()
	if len(msgs) == 0 {
		googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
		return
	}
	format := q.Get("format")
	if format != "minimal" && format != "metadata" {
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
		return
	}
	out := gThread{ID: id, HistoryID: int64s(b.historyID)}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, m := range msgs {
		gm := gMessage{ID: m.id, ThreadID: m.threadID, LabelIDs: slices.Clone(m.labels)}
		if format == "metadata" {
			gm.Payload = &gPart{}
			for _, name := range q["metadataHeaders"] {
				if v := headerValue(m.raw, name); v != "" {
					gm.Payload.Headers = append(gm.Payload.Headers, gHeader{Name: name, Value: v})
				}
			}
		}
		out.Messages = append(out.Messages, gm)
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *fakeGmail) threadMutate(w http.ResponseWriter, r *http.Request, b *fakeBox, id, verb string) {
	var req gModify
	if verb == "modify" && (json.NewDecoder(r.Body).Decode(&req) != nil || len(req.AddLabelIDs) > 100 || len(req.RemoveLabelIDs) > 100) {
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
		return
	}
	g.mu.Lock()
	b.threadOps = append(b.threadOps, verb+" "+id)
	if verb == "modify" {
		b.modifies = append(b.modifies, req)
	}
	msgs := g.threadMessages(b, id)
	if len(msgs) == 0 {
		g.mu.Unlock()
		googleError(w, http.StatusNotFound, "notFound", "NOT_FOUND")
		return
	}
	resp := conformance.Response{Outcome: conformance.OutcomeAccepted}
	if len(b.responses) > 0 {
		resp, b.responses = b.responses[0], b.responses[1:]
	}
	var call conformance.ProviderCall
	for _, m := range msgs {
		if c, ok := b.stateCalls[m.id]; ok {
			call = c
			break
		}
	}
	apply := resp.Outcome == conformance.OutcomeAccepted || resp.Outcome == conformance.OutcomeAnswerLost
	if apply {
		add, remove := req.AddLabelIDs, req.RemoveLabelIDs
		switch verb {
		case "trash":
			add, remove = []string{"TRASH"}, []string{labelInbox}
		case "untrash":
			add, remove = nil, []string{"TRASH"}
		}
		for _, m := range msgs {
			g.relabel(b, m, add, remove)
		}
		call.Applied = true
	}
	b.calls = append(b.calls, call)
	g.mu.Unlock()
	switch resp.Outcome {
	case conformance.OutcomeAccepted:
		writeJSON(w, http.StatusOK, gThread{ID: id})
	case conformance.OutcomeRejected:
		googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
	case conformance.OutcomeRateLimited:
		w.Header().Set("Retry-After", strconv.Itoa(int(resp.RetryAfter/time.Second)))
		googleError(w, http.StatusTooManyRequests, "rateLimitExceeded", "RESOURCE_EXHAUSTED")
	case conformance.OutcomeTransient:
		googleError(w, http.StatusServiceUnavailable, "backendError", "UNAVAILABLE")
	case conformance.OutcomeAnswerLost, conformance.OutcomeRequestLost:
		panic(http.ErrAbortHandler)
	}
}

// remove deletes message id from user's mailbox with a messageDeleted
// history record.
func (g *fakeGmail) remove(user, id string) {
	b := g.box(user, model.ChannelRef{})
	g.mu.Lock()
	defer g.mu.Unlock()
	m, ok := b.messages[id]
	if !ok {
		return
	}
	b.historyID++
	b.history = append(b.history, fakeHistory{id: b.historyID, msg: id, thread: m.threadID, kind: "messageDeleted", labels: slices.Clone(m.labels)})
	delete(b.messages, id)
	b.order = slices.DeleteFunc(b.order, func(x string) bool { return x == id })
}

// relabelThread changes the labels of every message of thread id as another
// client would: with history records and without any state call.
func (g *fakeGmail) relabelThread(user, id string, add, remove []string) {
	b := g.box(user, model.ChannelRef{})
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, m := range g.threadMessages(b, id) {
		g.relabel(b, m, add, remove)
	}
}
