package telegram

import (
	"context"
	"errors"
	"strconv"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// AllowedUpdates are the update types the adapter reads. The Bot API sends
// every type but chat_member, message_reaction and message_reaction_count by
// default, and "If not specified, the previous setting will be used", so a
// receiver names the types it wants on every read, and setWebhook takes the
// same list (https://core.telegram.org/bots/api#getupdates).
var AllowedUpdates = []string{"message", "edited_message", "channel_post", "edited_channel_post", "message_reaction", "callback_query"}

// pollLimit is the most updates one read takes: "Values between 1-100 are
// accepted. Defaults to 100".
const pollLimit = 100

// jitterFraction is the spread of the pause between reads: +-10%.
const jitterFraction = 0.1

type pollArgs struct {
	Offset         int64    `json:"offset,omitempty"`
	Limit          int      `json:"limit"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates"`
}

// Run reads the bot's updates by long polling (getUpdates) and hands them to
// sink until ctx ends, which it returns. It is the other way to receive: "There
// are two mutually exclusive ways of receiving updates for your bot - the
// getUpdates method on one hand and webhooks on the other", so while a webhook
// is set Telegram refuses every read ("This method will not work if an
// outgoing webhook is set up") with error_code 409, observed with the
// undocumented description "Conflict: can't use getUpdates method while
// webhook is active; use deleteWebhook to delete the webhook first", and
// Run returns model.Conflict. The adapter never sets or deletes a webhook:
// that is a decision about the bot, for its owner.
//
// An update is "confirmed as soon as getUpdates is called with an offset
// higher than its update_id", so Run asks with the update after the last it
// handled, and Telegram forgets the rest; an update the sink refused is never
// confirmed and is read again. The cursor the engine keeps is the update_id of
// the last envelope an update made, committed with it, so a restart resumes
// after the last update handled, and an update that made no envelope is
// confirmed by the read after it. A receiver's connection is reopened by the
// engine after a retryable failure; Telegram, which keeps updates for 24 hours
// at most, redelivers what was not confirmed, and the engine's duplicate check
// absorbs what was handed twice.
//
// The reads use the host's HTTP client, which must allow a request to last
// longer than Config.PollTimeoutSeconds. A flood limit is waited out; any
// other refusal ends Run: a bad token (unauthorized), a webhook (conflict), a
// server error (retryable). An update the adapter cannot read is skipped,
// never retried for good.
//
// The instance reports its link to the host (adapter.ReceiverReporter):
// connected once a getUpdates is answered, even with no update, and not
// connected after a failed read, a flood limit included, and when Run
// returns.
//
// Under Config.Receive ReceiveWebhook the channel does not poll: Run returns
// model.Disabled at once and calls nothing. The engine never gets there, as
// the instance reports receive disabled.
func (x *Instance) Run(ctx context.Context, sink adapter.Sink) error {
	if !x.cfg.offers(model.CapabilityReceive) {
		return final(model.Disabled, "the channel receives by webhook (configuration receive is \"webhook\")")
	}
	if err := x.ready(); err != nil {
		return err
	}
	defer x.link(false)
	var offset int64
	if cs, ok := sink.(adapter.CursorSink); ok {
		c, err := cs.ReceiverCursor(ctx)
		if err != nil {
			return err
		}
		if n, perr := strconv.ParseInt(c, 10, 64); perr == nil && n > 0 {
			offset = n + 1
		}
	}
	interval := x.opts.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		started := time.Now()
		// Each pause is the interval +-10%, so channels do not read in phase.
		gap := adapter.Spread(interval, jitterFraction, x.opts.Jitter)
		var updates []update
		args := pollArgs{
			Offset: offset, Limit: pollLimit, Timeout: int(x.cfg.pollTimeout() / time.Second), AllowedUpdates: AllowedUpdates,
		}
		if err := x.call(ctx, "getUpdates", &args, &updates); err != nil {
			x.link(false)
			_, ferr := failure(ctx, err, readCall)
			if me, ok := errors.AsType[*model.Error](ferr); ok && me.Code == model.RateLimited {
				if werr := x.wait(ctx, me.RetryAfter); werr != nil {
					return werr
				}
				continue
			}
			return ferr
		}
		x.link(true)
		for _, u := range updates {
			if err := x.accept(ctx, sink, u); err != nil {
				return err
			}
			offset = max(offset, u.UpdateID+1)
		}
		if len(updates) == 0 {
			if rest := gap - time.Since(started); rest > 0 {
				if err := x.wait(ctx, rest); err != nil {
					return err
				}
			}
		}
	}
}

// accept hands one update's envelopes to the sink, in order, the cursor on
// the last: the engine saves it with that ingress, so a restart never skips
// an envelope of an update it did not finish. An update that cannot be read
// is skipped; a refusal of the sink ends the connection, whatever its kind:
// the engine reconnects after a retryable one, and stops on any other.
func (x *Instance) accept(ctx context.Context, sink adapter.Sink, u update) error {
	batch, err := x.normalize(u)
	if err != nil || len(batch) == 0 {
		return nil
	}
	cursor := idString(u.UpdateID)
	for i, d := range batch {
		in := d.ingress
		var fallback *model.Ingress
		if d.fallback != nil {
			f := *d.fallback
			fallback = &f
		}
		if i == len(batch)-1 {
			in.ReceiverCursor = cursor
			if fallback != nil {
				fallback.ReceiverCursor = cursor
			}
		}
		_, err := sink.Accept(ctx, in)
		if model.IsCode(err, model.NotFound) && fallback != nil {
			_, err = sink.Accept(ctx, *fallback)
		}
		if model.IsCode(err, model.NotFound) && dropsWhenUnknown(in.Envelope.Kind) {
			err = nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
