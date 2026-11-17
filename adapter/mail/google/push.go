package google

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// maxPushBytes bounds a push request body; Gmail's data is two fields.
const maxPushBytes = 64 << 10

// Handler serves the channel's Pub/Sub push endpoint (the host terminates
// HTTP and mounts it, through Runtime.Webhook, at the URL of a push
// subscription on Config.Topic with authentication enabled):
//
//   - the request's OIDC token is verified first (see verifyPush): a
//     missing token is answered 401, a rejected one 403, unreachable
//     Google keys 503;
//   - the Gmail notification (emailAddress, historyId) is decoded from
//     message.data; one for another mailbox, or not newer than the stored
//     historyId, is acknowledged with nothing to do;
//   - otherwise the mailbox history is read from the stored historyId (one
//     page; the stored state is not advanced here) and every message added
//     to the inbox is fetched and handed to the sink, and the receiver is
//     woken to advance the state and read any further pages.
//
// The answer is 204, which Pub/Sub counts as an acknowledgement, once
// every message was handled. A retryable failure (Gmail unreachable, the
// sink unavailable) is answered 503 so Pub/Sub redelivers; ingress
// deduplication absorbs repeats and the receiver's history rounds cover
// anything Pub/Sub gives up on.
func (i *instance) Handler(sink adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.servePush(w, r, sink)
	})
}

func (i *instance) servePush(w http.ResponseWriter, r *http.Request, sink adapter.Sink) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !i.cfg.push() {
		http.Error(w, "push not configured", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	switch err := i.verifyPush(ctx, r); {
	case errors.Is(err, errNoToken):
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	case errors.Is(err, errBadToken):
		http.Error(w, "push token rejected", http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, "push token cannot be checked", http.StatusServiceUnavailable)
		return
	}
	n, ok := decodePush(io.LimitReader(r.Body, maxPushBytes))
	if !ok {
		// Authenticated but not a Gmail notification: redelivery cannot
		// fix it; the receiver's history round covers the mailbox.
		i.state.notify()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s := i.c.session()
	addr, err := i.mailboxAddress(ctx, s)
	if err != nil {
		pushFailed(w, err)
		return
	}
	if !strings.EqualFold(n.EmailAddress, addr) {
		w.WriteHeader(http.StatusNoContent) // another mailbox's notification
		return
	}
	st, err := i.loadState(ctx)
	if err != nil {
		pushFailed(w, err)
		return
	}
	if n.HistoryID != 0 && st.HistoryID != 0 && n.HistoryID <= st.HistoryID {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	err = i.syncHistory(ctx, s, sink, &st, false, 1)
	i.state.notify()
	if err != nil && !errors.Is(err, errHistoryGone) && retryable(err) {
		pushFailed(w, err)
		return
	}
	// A permanent failure or an out-of-date historyId is not redelivered:
	// the receiver resynchronizes.
	w.WriteHeader(http.StatusNoContent)
}

func pushFailed(w http.ResponseWriter, err error) {
	if retryable(err) {
		http.Error(w, "notification not processed", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodePush reads a Pub/Sub push body and its Gmail notification.
// message.data is base64; standard and URL alphabets, padded or not, are
// accepted.
func decodePush(r io.Reader) (gNotification, bool) {
	var p gPush
	var n gNotification
	if json.NewDecoder(r).Decode(&p) != nil || p.Message.Data == "" {
		return n, false
	}
	data := strings.TrimRight(p.Message.Data, "=")
	b, err := base64.RawStdEncoding.DecodeString(data)
	if err != nil {
		if b, err = base64.RawURLEncoding.DecodeString(data); err != nil {
			return n, false
		}
	}
	if json.Unmarshal(b, &n) != nil || n.EmailAddress == "" {
		return n, false
	}
	return n, true
}

// retryable reports an untyped failure or a typed one whose advice is to
// retry the same work.
func retryable(err error) bool {
	if isContext(err) {
		return true
	}
	code := model.CodeOf(err)
	return code == "" || code.DefaultRetry() == model.RetrySameOperation
}
