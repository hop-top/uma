package microsoft

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Lifecycle events Graph sends to the lifecycle notification URL.
const (
	eventReauthorizationRequired = "reauthorizationRequired"
	eventSubscriptionRemoved     = "subscriptionRemoved"
	eventMissed                  = "missed"
)

// Handler serves the channel's change and lifecycle notifications:
//
//   - a request carrying validationToken is the subscription endpoint
//     check: the decoded token is echoed as text/plain with 200;
//   - otherwise the body is a notification collection. Notifications whose
//     clientState differs from the channel's (compared in constant time)
//     are discarded; a collection with none valid is answered 403. Each
//     valid "created" notification is fetched from the channel's own
//     mailbox by its resource ID (never by the notified resource path) and
//     handed to the sink; lifecycle events wake the receiver to renew,
//     recreate or resynchronize.
//
// The answer is 202 once every valid notification was handled. A
// retryable failure (Graph unreachable, the sink unavailable) is answered
// 503 so Graph redelivers; ingress deduplication absorbs repeats and the
// receiver's delta round covers anything Graph gives up on.
func (i *instance) Handler(sink adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.serveNotification(w, r, sink)
	})
}

func (i *instance) serveNotification(w http.ResponseWriter, r *http.Request, sink adapter.Sink) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if q := r.URL.Query(); q.Has("validationToken") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		// Graph requires the decoded token back verbatim as text/plain;
		// nosniff keeps browsers from rendering it.
		_, _ = io.WriteString(w, q.Get("validationToken")) // #nosec G705 -- text/plain echo the Graph handshake requires
		return
	}
	var batch gNotificationCollection
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		http.Error(w, "invalid notification body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	want, err := i.clientState(ctx)
	if err != nil {
		http.Error(w, "notification secret unavailable", http.StatusServiceUnavailable)
		return
	}
	var valid []gNotification
	for _, n := range batch.Value {
		if subtle.ConstantTimeCompare([]byte(n.ClientState), []byte(want)) == 1 {
			valid = append(valid, n)
		}
	}
	if len(valid) == 0 {
		http.Error(w, "unrecognized client state", http.StatusForbidden)
		return
	}
	s := i.c.session()
	retry := false
	for _, n := range valid {
		switch n.LifecycleEvent {
		case eventReauthorizationRequired:
			i.state.notify(lifecycleRenew)
			continue
		case eventSubscriptionRemoved:
			i.state.notify(lifecycleRecreate)
			continue
		case eventMissed:
			i.state.notify(0)
			continue
		case "":
		default:
			continue
		}
		if n.ChangeType != "created" || n.ResourceData == nil || n.ResourceData.ID == "" {
			continue
		}
		if err := i.ingestNotified(r, s, sink, n.ResourceData.ID); err != nil {
			if retryable(err) {
				retry = true
			}
			// A permanent failure is not redelivered: the message stays in
			// the mailbox and the receiver's delta round meets it again.
			i.state.notify(0)
		}
	}
	if retry {
		http.Error(w, "notification not processed", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (i *instance) ingestNotified(r *http.Request, s *session, sink adapter.Sink, id string) error {
	in, skip, err := i.fetchIngress(r.Context(), s, sink, id)
	if err != nil || skip {
		return err
	}
	_, err = sink.Accept(r.Context(), in)
	return err
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
