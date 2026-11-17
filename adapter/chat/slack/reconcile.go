package slack

import (
	"context"
	"errors"
	"slices"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

const (
	// historyPage is the page size of reads: Slack recommends no more than
	// 200 results at a time (https://docs.slack.dev/reference/methods/conversations.history/).
	historyPage = 200
	// reconcilePages bounds the pages one reconciliation reads; past it the
	// result is inconclusive, never "not found".
	reconcilePages = 10
	// reconcileSkew is how much earlier than the attempt a message may
	// carry its ts and still be the attempt's: the host's clock and Slack's
	// differ.
	reconcileSkew = 5 * time.Second
)

// readArgs are the arguments of conversations.history and
// conversations.replies.
type readArgs struct {
	Channel            string `json:"channel"`
	TS                 string `json:"ts,omitempty"`
	Oldest             string `json:"oldest,omitempty"`
	Latest             string `json:"latest,omitempty"`
	Inclusive          bool   `json:"inclusive,omitempty"`
	Limit              int    `json:"limit,omitempty"`
	Cursor             string `json:"cursor,omitempty"`
	IncludeAllMetadata bool   `json:"include_all_metadata,omitempty"`
}

type readReply struct {
	Messages         []slackEvent `json:"messages"`
	HasMore          bool         `json:"has_more"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// readPages reads method (conversations.history or conversations.replies)
// page after page, handing each message to visit until it returns true or
// pages run out. It reports whether the listing was read to its end.
func (x *Instance) readPages(ctx context.Context, method string, args readArgs, maxPages int, visit func(slackEvent) bool) (complete bool, err error) {
	args.Limit = historyPage
	for range maxPages {
		var page readReply
		if err := x.call(ctx, method, &args, &page); err != nil {
			return false, err
		}
		for _, m := range page.Messages {
			if visit(m) {
				return true, nil
			}
		}
		if !page.HasMore || page.ResponseMetadata.NextCursor == "" {
			return true, nil
		}
		args.Cursor = page.ResponseMetadata.NextCursor
	}
	return false, nil
}

// Reconcile resolves an attempt whose answer was lost, by reading what
// Slack holds. It never writes.
//
//   - send: the bot's message of the attempt in the conversation (or the
//     thread), read newest first from the attempt on. A message whose
//     metadata names the attempt's operation and attempt is it. Without
//     metadata (Slack drops what the app did not register), a message by the
//     bot, posted after the attempt, with the text the attempt sent, and no
//     marker of another attempt, is it. Found is provider_accepted with its
//     reference; a listing read to its end without it is failed with
//     not_found, so a retry is safe; a listing cut short is inconclusive.
//   - edit: the message carries the attempt's marker or the replacement text
//     → applied; any other state means the attempt did not apply; a message
//     that is gone is rejected not_found.
//   - delete: the message is gone → applied; present → not applied.
//   - react: the bot holds the reaction (or, for a removal, does not) →
//     applied; otherwise not applied. A reaction is read with the message's
//     full list, which "will always contain the authenticated user"
//     (https://docs.slack.dev/reference/methods/reactions.get/).
//   - archive: the channel's is_archived, read with conversations.info, is
//     what the attempt asked for → applied; otherwise not applied.
//
// Not applied is failed with not_found. A view effect (extension_execute) is
// not reconciled: a spent trigger_id cannot be asked what it opened, so the
// request is unsupported and a lost answer settles outcome_unknown, never
// retried.
func (x *Instance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	switch req.Capability {
	case model.CapabilitySend, model.CapabilityStartConversation:
		return x.reconcileSend(ctx, req)
	case model.CapabilityEdit, model.CapabilityDelete:
		return x.reconcileMessage(ctx, req)
	case model.CapabilityReact:
		return x.reconcileReaction(ctx, req)
	case model.CapabilityArchive:
		return x.reconcileArchive(ctx, req)
	}
	return adapter.Outcome{}, final(model.Unsupported, "reconciliation of this capability is not supported")
}

func notAppliedOutcome() adapter.Outcome {
	return adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "Slack does not show the attempt's effect")}
}

// markedBy reports whether m carries the attempt's marker, and whether it
// carries another attempt's.
func markedBy(m slackEvent, e adapter.Effect) (this, other bool) {
	if m.Metadata == nil || m.Metadata.EventType != MetadataEventType {
		return false, false
	}
	op, _ := m.Metadata.EventPayload["operation"].(string)
	att, _ := m.Metadata.EventPayload["attempt"].(string)
	if op == string(e.Operation.ID) && att == string(e.Attempt) {
		return true, false
	}
	return false, true
}

func (x *Instance) isOwn(m slackEvent) bool { return x.own(m.User, m.BotID) }

func (x *Instance) reconcileSend(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	t, err := x.parseRoute(req.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	msg, rerr := render(req.Content)
	if rerr != nil {
		// Never sent: nothing to find.
		return notAppliedOutcome(), nil
	}
	conv, out, err := x.conversation(ctx, t)
	if err != nil || out.State != "" {
		return out, err
	}
	since := formatTS(req.AttemptedAt.Add(-reconcileSkew))
	method, args := "conversations.history", readArgs{Channel: conv, Oldest: since, IncludeAllMetadata: true}
	if anchor := threadAnchor(t, conv, req); anchor != "" {
		method, args.TS = "conversations.replies", anchor
	}
	var exact, candidate *slackEvent
	complete, err := x.readPages(ctx, method, args, reconcilePages, func(m slackEvent) bool {
		if !x.isOwn(m) || !tsPattern.MatchString(m.TS) || m.TS <= since {
			return false
		}
		this, other := markedBy(m, req.Effect)
		switch {
		case this:
			exact = &m
			return true
		case other || m.Text != msg.text || (msg.blocks == nil) != (len(m.Blocks) == 0):
		case candidate == nil || m.TS < candidate.TS:
			c := m
			candidate = &c
		}
		return false
	})
	if err != nil {
		return failure(ctx, err, readCall)
	}
	found := exact
	if found == nil {
		found = candidate
	}
	switch {
	case found != nil:
		return adapter.Outcome{
			State:      model.ProviderAccepted,
			References: []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, messageID(conv, found.TS))},
		}, nil
	case !complete:
		return adapter.Outcome{State: model.Unknown}, nil
	}
	return notAppliedOutcome(), nil
}

// threadAnchor is the ts conversations.replies reads for a send into a
// thread: the thread of the route, or the message the send answered ("the
// parent message's ts or any message in the thread").
func threadAnchor(t target, conversation string, req adapter.ReconcileRequest) string {
	if t.thread != "" {
		return t.thread
	}
	if req.Target != nil && req.Target.Namespace == model.ProviderMessageNamespace && req.Target.Kind == model.RefKindMessage {
		if c, ts, ok := parseMessageID(req.Target.ID); ok && c == conversation {
			return ts
		}
	}
	return ""
}

// message reads one message by its conversation and ts, with its metadata:
// conversations.replies bounded to that ts reads top-level messages and
// replies alike. gone reports that Slack no longer has it.
func (x *Instance) message(ctx context.Context, conv, ts string) (m slackEvent, gone bool, err error) {
	args := readArgs{Channel: conv, TS: ts, Oldest: ts, Latest: ts, Inclusive: true, IncludeAllMetadata: true}
	var found bool
	_, err = x.readPages(ctx, "conversations.replies", args, 1, func(e slackEvent) bool {
		if e.TS == ts {
			m, found = e, true
			return true
		}
		return false
	})
	if ae, ok := errors.AsType[*apiError](err); ok && slices.Contains(missingCodes, ae.code) {
		return slackEvent{}, true, nil
	}
	return m, err == nil && !found, err
}

func (x *Instance) reconcileMessage(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	conv, ts, err := x.messageTarget(req.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	m, gone, err := x.message(ctx, conv, ts)
	if err != nil {
		return failure(ctx, err, readCall)
	}
	applied := adapter.Outcome{State: model.ProviderAccepted}
	if req.Capability == model.CapabilityDelete {
		if gone {
			return applied, nil
		}
		return notAppliedOutcome(), nil
	}
	if gone {
		return rejected(model.NotFound, "message not found"), nil
	}
	if this, _ := markedBy(m, req.Effect); this {
		return applied, nil
	}
	msg, rerr := render(req.Content)
	if rerr == nil && m.Text == msg.text && (msg.blocks == nil) == (len(m.Blocks) == 0) {
		return applied, nil
	}
	return notAppliedOutcome(), nil
}

func (x *Instance) reconcileReaction(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	conv, ts, err := x.messageTarget(req.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	name, ok := reactionName(req.Emoji)
	if !ok {
		return rejected(model.Unsupported, "Slack has no reaction name for this emoji"), nil
	}
	var res struct {
		Message slackEvent `json:"message"`
	}
	err = x.call(ctx, "reactions.get", map[string]any{"channel": conv, "timestamp": ts, "full": true}, &res)
	if ae, ok := errors.AsType[*apiError](err); ok && slices.Contains(missingCodes, ae.code) {
		return rejected(model.NotFound, "message not found"), nil
	}
	if err != nil {
		return failure(ctx, err, readCall)
	}
	held := false
	for _, r := range res.Message.Reactions {
		if r.Name == name && slices.Contains(r.Users, x.cfg.BotUserID) {
			held = true
		}
	}
	if held != req.Remove {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	return notAppliedOutcome(), nil
}

var _ adapter.Reconciler = (*Instance)(nil)
