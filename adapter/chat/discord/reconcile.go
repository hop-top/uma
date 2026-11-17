package discord

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

const (
	// pageSize is the largest page Get Channel Messages answers ("limit
	// 1-100").
	pageSize = 100
	// reconcilePages bounds the pages one reconciliation reads; past it the
	// result is inconclusive, never "not found".
	reconcilePages = 10
	// reconcileSkew is how much earlier than the attempt a message may
	// carry its snowflake and still be the attempt's: the host's clock and
	// Discord's differ.
	reconcileSkew = 5 * time.Second
	// codeUnknownMember is Discord's answer for a user who is not a
	// thread member.
	codeUnknownMember = 10007
)

// Reconcile resolves an attempt whose answer was lost, by reading what
// Discord holds. It never writes, apart from Create DM, which answers the
// existing direct message channel.
//
//   - send: the bot's messages of the channel posted from the attempt on
//     (Get Channel Messages with after, a page of 100 at a time). A message
//     carrying the operation's nonce is it; a message carrying another
//     nonce is not. A message with no nonce (a read returns none: recorded
//     against Discord) is it when the bot posted it with the text
//     and components the attempt sent. Found is provider_accepted with its
//     reference; a listing read to its end without it is failed with
//     not_found, so a retry is safe; a listing cut short is inconclusive.
//   - edit: the message's content is the replacement's → applied; else not
//     applied; a message that is gone is rejected not_found.
//   - delete: the message is gone → applied; present → not applied.
//   - react: the message's reaction of the emoji is the bot's ("me") →
//     applied, or for a removal is not; else not applied.
//   - membership: Get Thread Member answers the user (or, for a removal, not
//     a member) → applied; else not applied.
//   - archive: Get Channel shows the thread archived (for an unarchive, not)
//     → applied; else not applied; a thread that is gone is rejected
//     not_found.
//
// Not applied is failed with not_found. A response or an ephemeral message
// is not reconciled: an interaction webhook names no message to look up
// until it answers.
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
	case model.CapabilityMembership:
		return x.reconcileMembership(ctx, req)
	case model.CapabilityArchive:
		return x.reconcileArchive(ctx, req)
	}
	return adapter.Outcome{}, final(model.Unsupported, "reconciliation of this capability is not supported")
}

func notAppliedOutcome() adapter.Outcome {
	return adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "Discord does not show the attempt's effect")}
}

// messagesAfter reads one page of a channel's messages after the snowflake
// after, newest first as Discord answers.
func (x *Instance) messagesAfter(ctx context.Context, channel, after string, limit int) ([]message, error) {
	var page []message
	q := url.Values{"after": {after}, "limit": {strconv.Itoa(limit)}}
	err := x.do(ctx, request{method: http.MethodGet, path: "channels/" + channel + "/messages", query: q}, &page)
	return page, err
}

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
	channel, out, err := x.channelOf(ctx, t)
	if err != nil {
		return out, err
	}
	if out.State != "" {
		return notAppliedOutcome(), nil
	}
	nonce := operationNonce(req.Effect)
	after := snowflakeAt(req.AttemptedAt.Add(-reconcileSkew))
	var exact, candidate *message
	complete := false
	for range reconcilePages {
		page, err := x.messagesAfter(ctx, channel, after, pageSize)
		if err != nil {
			return failure(ctx, err, readCall)
		}
		for i := range page {
			m := page[i]
			if compareSnowflake(m.ID, after) > 0 {
				after = m.ID
			}
			if m.Author == nil || m.Author.ID != x.cfg.BotUserID {
				continue
			}
			switch n := m.nonce(); {
			case n == nonce:
				exact = &m
			case n != "":
			case m.Content != msg.text || (msg.components == nil) != emptyJSON(m.componentsRaw()):
			case candidate == nil || lessSnowflake(m.ID, candidate.ID):
				candidate = &m
			}
		}
		if exact != nil || len(page) < pageSize {
			complete = true
			break
		}
	}
	found := exact
	if found == nil {
		found = candidate
	}
	switch {
	case found != nil:
		return adapter.Outcome{
			State:      model.ProviderAccepted,
			References: []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, messageID(channel, found.ID))},
		}, nil
	case !complete:
		return adapter.Outcome{State: model.Unknown}, nil
	}
	return notAppliedOutcome(), nil
}

// readMessage reads one message; gone reports that Discord no longer has
// it (404).
func (x *Instance) readMessage(ctx context.Context, channel, id string) (m message, gone bool, err error) {
	err = x.call(ctx, http.MethodGet, "channels/"+channel+"/messages/"+id, nil, &m)
	if isStatus(err, http.StatusNotFound) {
		return message{}, true, nil
	}
	return m, false, err
}

func (x *Instance) reconcileMessage(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	channel, id, err := x.messageTarget(req.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	m, gone, err := x.readMessage(ctx, channel, id)
	if err != nil {
		return failure(ctx, err, readCall)
	}
	if req.Capability == model.CapabilityDelete {
		if gone {
			return adapter.Outcome{State: model.ProviderAccepted}, nil
		}
		return notAppliedOutcome(), nil
	}
	if gone {
		return rejected(model.NotFound, "message not found"), nil
	}
	want, rerr := render(req.Content)
	if rerr == nil && m.Content == want.text && (want.components == nil) == emptyJSON(m.componentsRaw()) {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	return notAppliedOutcome(), nil
}

func (x *Instance) reconcileReaction(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	channel, id, err := x.messageTarget(req.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	m, gone, err := x.readMessage(ctx, channel, id)
	if err != nil {
		return failure(ctx, err, readCall)
	}
	if gone {
		return rejected(model.NotFound, "message not found"), nil
	}
	held := false
	for _, r := range m.Reactions {
		if r.Emoji.ID == "" && r.Emoji.Name == req.Emoji && r.Me {
			held = true
		}
	}
	if held != req.Remove {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	return notAppliedOutcome(), nil
}

func (x *Instance) reconcileMembership(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	t, err := x.parseRoute(req.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	p := req.Participant
	if t.parent == "" || p.Namespace != UserNamespace || !isSnowflake(p.ID) {
		return notAppliedOutcome(), nil
	}
	err = x.call(ctx, http.MethodGet, "channels/"+t.channel+"/thread-members/"+p.ID, nil, nil)
	member := err == nil
	switch {
	case isCode(err, codeUnknownChannel):
		return rejected(model.NotFound, "thread not found"), nil
	case err != nil && (isCode(err, codeUnknownMember) || isStatus(err, http.StatusNotFound)):
	case err != nil:
		return failure(ctx, err, readCall)
	}
	if member != req.Remove {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	return notAppliedOutcome(), nil
}

var _ adapter.Reconciler = (*Instance)(nil)
