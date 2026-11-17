package a2a

import (
	"context"
	"errors"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// await answers a blocking SendMessage: it reads what answers the accepted
// message every poll interval (+-10%), store-polled so a worker in another
// process counts, until the first of
//
//   - a uma-authored reply to the message, while the message is bound to
//     no task: a Message response;
//   - the message's task in a terminal or interrupted state, reported
//     after the message (or opened by it): a Task response, whichever
//     terminal state it is, a failed or canceled task included;
//   - the wait running out, the shorter of the channel's blocking_wait
//     and the caller's own deadline: the deadline error. Expiry returns
//     no non-terminal task as success, opens and cancels nothing; the
//     accepted message and any task stay readable.
func (c *call) await(ctx context.Context, r model.IngressReceipt, clientID string, history int) (any, *rpcError) {
	wait := c.x.cfg.wait
	if dl, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(dl))
	}
	timer := time.NewTimer(max(wait, 0))
	defer timer.Stop()
	// A ticker cannot spread each interval, so each wait is a timer of its
	// own, restarted after the read.
	tick := time.NewTimer(c.pollWait())
	defer tick.Stop()
	var seen *model.Task
	last := false
	for {
		resp, err := c.s.tasks.Responses(ctx, c.principal, *r.Message)
		if err != nil {
			switch {
			case errors.Is(ctx.Err(), context.Canceled):
				return nil, nil // the client went away
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				return nil, c.deadline(r, clientID, seen)
			}
			return nil, internalError(c.x.f.domain, err)
		}
		switch t := resp.Task; {
		case t != nil && (model.IsTerminalTaskState(t.State) || model.IsInterruptedTaskState(t.State)):
			v, rerr := c.view(ctx, *t, history, true)
			if rerr != nil {
				return nil, rerr
			}
			return sendResult{Task: &v}, nil
		case t != nil:
			seen = t
		case len(resp.Replies) > 0:
			m := replyView(resp.Replies[0], r.Context.ID)
			return sendResult{Message: &m}, nil
		}
		if last {
			return nil, c.deadline(r, clientID, seen)
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, nil
			}
			return nil, c.deadline(r, clientID, seen)
		case <-timer.C:
			// One last read, so an answer in the final interval counts.
			last = true
		case <-tick.C:
			tick.Reset(c.pollWait())
		}
	}
}

// jitterFraction is the spread of the wait between store reads: +-10%.
const jitterFraction = 0.1

// pollWait is the wait before the next store read: the poll interval +-10%
// (adapter.Spread), so calls do not read in phase.
func (c *call) pollWait() time.Duration {
	return adapter.Spread(c.x.cfg.poll, jitterFraction, c.x.f.jitter)
}

// deadline is the error of a wait that ran out: uma's documented mapping,
// JSON-RPC -32603 with an ErrorInfo of reason DEADLINE_EXCEEDED in the
// configured domain. Its metadata names the context and the client's
// message ID, and the task only when one exists.
func (c *call) deadline(r model.IngressReceipt, clientID string, seen *model.Task) *rpcError {
	meta := map[string]string{"contextId": r.Context.ID, "messageId": clientID}
	switch {
	case r.Task != nil:
		meta["taskId"] = string(model.TaskExternalRef(c.x.ch, r.Task.ID).ID)
	case seen != nil:
		meta["taskId"] = seen.External.ID
	}
	return umaError(c.x.f.domain, codeInternal, "The host did not answer before the wait ran out", "DEADLINE_EXCEEDED", meta)
}
