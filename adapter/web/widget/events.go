package widget

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"hop.top/uma/model"
)

// readBatch bounds the entries an event stream reads per round.
const readBatch = 100

// wireEvent is an event as the stream's data line carries it.
type wireEvent struct {
	Event
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Events returns the event stream of channel: a GET answered with
// Server-Sent Events for the visitor of a session token, presented as a
// bearer token or as the token query parameter.
//
// The stream starts after the cursor in the Last-Event-ID header or the
// after query parameter, or at the start of the retained log without one.
// Each event's SSE id is its log sequence, its SSE event name its Type and
// its data the Event as JSON, with expires_at for ephemeral replies and
// signals. Entries for another session of the visitor, and expired
// entries, are skipped. When entries after the cursor are no longer
// retained the stream sends a reset event first. Idle streams send a
// comment every heartbeat. A stream ends when the client goes away, the
// token expires, the log fails or the Factory closes; the client reconnects
// with its cursor.
//
// It answers 405 for another method, 400 for a malformed cursor, 401 for a
// missing, forged, expired or foreign token, and 503 before the channel's
// first instance has been opened (mounting Runtime.Webhook opens one), when
// the session key cannot be resolved, or after Close.
func (f *Factory) Events(channel model.ChannelRef) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			status(w, http.StatusMethodNotAllowed)
			return
		}
		b, ok := f.binding(channel)
		if !ok {
			status(w, http.StatusServiceUnavailable)
			return
		}
		after, ok := cursor(r)
		if !ok {
			status(w, http.StatusBadRequest)
			return
		}
		key, err := resolveKey(r.Context(), b.creds, b.channel)
		if err != nil {
			status(w, http.StatusServiceUnavailable)
			return
		}
		token := bearer(r)
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		c, err := verify(key, token, channel, b.clock.Now())
		if err != nil {
			status(w, http.StatusUnauthorized)
			return
		}
		if _, ok := b.cfg.site(c.Site); !ok {
			status(w, http.StatusUnauthorized)
			return
		}
		f.stream(w, r, b, c, after)
	})
}

// cursor reads the resume cursor of r.
func cursor(r *http.Request) (uint64, bool) {
	s := r.Header.Get("Last-Event-ID")
	if s == "" {
		s = r.URL.Query().Get("after")
	}
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

func (f *Factory) stream(w http.ResponseWriter, r *http.Request, b *binding, c claims, after uint64) {
	ctx := r.Context()
	key := c.key(b.channel.Ref)
	wake, stop := f.log.Watch(key)
	defer stop()
	expiry := time.NewTimer(time.Unix(c.Expires, 0).Sub(b.clock.Now()))
	defer expiry.Stop()
	heartbeat := time.NewTicker(f.heartbeat)
	defer heartbeat.Stop()

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	out := bufio.NewWriter(w)
	_, _ = out.WriteString("retry: 2000\n\n")
	if out.Flush() != nil || rc.Flush() != nil {
		return
	}
	for {
		entries, err := f.log.Read(ctx, key, after, readBatch)
		if err != nil {
			return
		}
		if len(entries) > 0 && entries[0].Seq > after+1 {
			_, _ = out.WriteString("event: " + EventReset + "\ndata: {}\n\n")
		}
		now := b.clock.Now()
		skipped := false
		for _, e := range entries {
			after = e.Seq
			skipped = e.Session != "" && e.Session != c.Session || !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt)
			if !skipped {
				writeEvent(out, e)
			}
		}
		if skipped {
			// An id-only message moves the client's cursor past entries
			// it does not see, without dispatching an event.
			_, _ = out.WriteString("id: " + strconv.FormatUint(after, 10) + "\n\n")
		}
		if out.Flush() != nil || rc.Flush() != nil {
			return
		}
		if len(entries) == readBatch {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-f.done:
			return
		case <-expiry.C:
			return
		case <-wake:
		case <-heartbeat.C:
			_, _ = out.WriteString(": ping\n\n")
		}
	}
}

func writeEvent(out *bufio.Writer, e Entry) {
	we := wireEvent{Event: e.Event}
	if !e.ExpiresAt.IsZero() {
		t := e.ExpiresAt.UTC()
		we.ExpiresAt = &t
	}
	data, err := json.Marshal(we)
	if err != nil {
		return
	}
	_, _ = out.WriteString("id: " + strconv.FormatUint(e.Seq, 10) + "\nevent: " + e.Event.Type + "\ndata: ")
	_, _ = out.Write(data)
	_, _ = out.WriteString("\n\n")
}
