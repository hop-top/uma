package discord

import (
	"context"
	"encoding/json"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// laneQueue is how many gateway messages are read ahead of the receiver,
// and how many dispatches it holds, waiting or being handled; past either
// it stops reading until it catches up.
const laneQueue = 64

// gatewayLanes is how many dispatches are handled at once: one channel's
// one at a time, in order, and up to gatewayLanes channels side by side.
const gatewayLanes = 8

// lane is the dispatches of one channel waiting their turn; busy while one
// of them is handled.
type lane struct {
	key   string
	queue []laneJob
	busy  bool
}

// laneJob is one dispatch: its session, sequence number, event name and
// data, and the receiver cursor its ingress carries.
type laneJob struct {
	gen     int
	seq     int64
	session string
	url     string
	typ     string
	data    json.RawMessage
	cursor  string
}

// laneResult ends job on lane, with the sink's refusal, if any.
type laneResult struct {
	lane    *lane
	job     laneJob
	refusal error
}

// dispatch puts job in its lane and starts what may start.
func (r *gatewayReceiver) dispatch(ctx context.Context, job laneJob) {
	key := laneKey(job.typ, job.data)
	l := r.lanes[key]
	if l == nil {
		l = &lane{key: key}
		r.lanes[key] = l
	}
	l.queue = append(l.queue, job)
	r.held++
	if job.gen == r.gen {
		r.outstanding[job.seq]++
	}
	if !l.busy && len(l.queue) == 1 {
		r.ready = append(r.ready, l)
	}
	r.startReady(ctx)
}

// startReady handles the next dispatch of each ready lane while fewer than
// gatewayLanes are being handled.
func (r *gatewayReceiver) startReady(ctx context.Context) {
	for r.running < gatewayLanes && len(r.ready) > 0 {
		l := r.ready[0]
		r.ready = r.ready[1:]
		job := l.queue[0]
		l.queue = l.queue[1:]
		l.busy = true
		r.running++
		job.cursor = r.cursorFor(job)
		r.wg.Go(func() {
			res := laneResult{lane: l, job: job, refusal: r.x.handle(ctx, r.sink, job)}
			select {
			case r.results <- res:
			case <-ctx.Done():
			}
		})
	}
}

// cursorFor is the receiver cursor of job's ingress: its session and the
// highest sequence number below which every dispatch read is handled, the
// job's own included once the rest is. Resuming from it replays nothing
// that was not recorded; core absorbs what it replays twice.
func (r *gatewayReceiver) cursorFor(job laneJob) string {
	if job.session == "" {
		return ""
	}
	seq := job.seq
	if job.gen == r.gen {
		for s, n := range r.outstanding {
			if n > 0 && s != job.seq && s-1 < seq {
				seq = s - 1
			}
		}
	}
	return session{ID: job.session, Seq: seq, URL: job.url}.cursor()
}

// finish ends one dispatch's handling: the next of its lane becomes ready,
// and a refusal that is retryable, or a denial, ends Run with it.
func (r *gatewayReceiver) finish(ctx context.Context, res laneResult) error {
	l, job := res.lane, res.job
	r.running--
	r.held--
	l.busy = false
	if job.gen == r.gen {
		if r.outstanding[job.seq]--; r.outstanding[job.seq] <= 0 {
			delete(r.outstanding, job.seq)
		}
	}
	if h := r.x.handled; h != nil {
		h(job.seq, res.refusal)
	}
	if res.refusal != nil {
		code := model.CodeOf(res.refusal)
		if code.DefaultRetry() == model.RetrySameOperation || code == model.Unauthorized {
			return res.refusal
		}
	}
	if len(l.queue) > 0 {
		r.ready = append(r.ready, l)
	} else {
		delete(r.lanes, l.key)
	}
	r.startReady(ctx)
	return nil
}

// laneKey is the lane of a dispatch: the channel it names, which a message
// and every edit, deletion and reaction of it share; an interaction's own.
func laneKey(typ string, data json.RawMessage) string {
	var p struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
	}
	_ = json.Unmarshal(data, &p)
	switch typ {
	case "INTERACTION_CREATE":
		return "interaction:" + p.ID
	case "THREAD_UPDATE":
		// The payload is the thread itself: its state changes are ordered
		// with the thread's messages.
		return "channel:" + p.ID
	}
	return "channel:" + p.ChannelID
}

// handle records one dispatch and returns the sink's refusal, if any. A
// dispatch the adapter cannot read is dropped.
func (x *Instance) handle(ctx context.Context, sink adapter.Sink, job laneJob) error {
	var batch []delivery
	var err error
	switch job.typ {
	case "MESSAGE_CREATE", "MESSAGE_UPDATE":
		var m message
		if json.Unmarshal(job.data, &m) != nil {
			return nil
		}
		if job.typ == "MESSAGE_CREATE" {
			batch, err = x.created(ctx, m)
		} else {
			batch, err = x.updated(ctx, m)
		}
	case "MESSAGE_DELETE":
		var d struct {
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
		}
		if json.Unmarshal(job.data, &d) != nil {
			return nil
		}
		batch = x.deleted(d)
	case "MESSAGE_REACTION_ADD", "MESSAGE_REACTION_REMOVE":
		var re reactionEvent
		if json.Unmarshal(job.data, &re) != nil {
			return nil
		}
		batch = x.reaction("reaction:"+job.session+":"+strconv.FormatInt(job.seq, 10), re, job.typ == "MESSAGE_REACTION_REMOVE")
	case "THREAD_UPDATE":
		var ch threadChannel
		if json.Unmarshal(job.data, &ch) != nil {
			return nil
		}
		batch = x.threadUpdated(ch)
	case "INTERACTION_CREATE":
		return x.gatewayInteraction(ctx, sink, job.data, job.cursor)
	}
	if err != nil {
		_, ferr := failure(ctx, err, readCall)
		return ferr
	}
	if ctx.Err() != nil {
		return nil
	}
	return deliver(ctx, sink, batch, job.cursor)
}
