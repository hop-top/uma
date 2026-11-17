package slack

import (
	"context"
	"errors"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Archive archives the channel the effect's route names, through
// conversations.archive, or unarchives it with conversations.unarchive when
// Undo is set. Archiving is channel-level and changes the room for everyone
// ("Archives a conversation", the channel stays readable and nobody can
// post), so the capability reports scope shared; the request's Items are
// not used, as a room is one object. A thread or a direct message is no
// archivable object: a route to one is a final unsupported rejection before
// any call. Applying the state already held is applied: Slack answers
// already_archived and not_archived. The channel the app may not archive,
// the workspace's #general, is refused as unsupported.
func (x *Instance) Archive(ctx context.Context, r adapter.ArchiveRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	t, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if t.thread != "" || t.user != "" {
		return rejected(model.Unsupported, "only a Slack channel can be archived, not a thread or a direct message"), nil
	}
	method, already := "conversations.archive", "already_archived"
	if r.Undo {
		method, already = "conversations.unarchive", "not_archived"
	}
	err = x.call(ctx, method, map[string]string{"channel": t.conversation}, nil)
	if ae, ok := errors.AsType[*apiError](err); ok {
		switch ae.code {
		case already:
			err = nil
		case "cant_archive_general", "method_not_supported_for_channel_type":
			return rejected(model.Unsupported, "Slack cannot archive this conversation: "+ae.code), nil
		}
	}
	if err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// reconcileArchive reads the channel's is_archived with conversations.info:
// the state the attempt asked for is applied, the other is not.
func (x *Instance) reconcileArchive(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	t, err := x.parseRoute(req.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if t.thread != "" || t.user != "" {
		return rejected(model.Unsupported, "only a Slack channel can be archived, not a thread or a direct message"), nil
	}
	var res struct {
		Channel struct {
			IsArchived bool `json:"is_archived"`
		} `json:"channel"`
	}
	err = x.call(ctx, "conversations.info", map[string]string{"channel": t.conversation}, &res)
	if ae, ok := errors.AsType[*apiError](err); ok && ae.code == "channel_not_found" {
		return rejected(model.NotFound, "channel not found"), nil
	}
	if err != nil {
		return failure(ctx, err, readCall)
	}
	if res.Channel.IsArchived != req.Undo {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	return notAppliedOutcome(), nil
}

// archiveState reports a channel_archive, channel_unarchive, group_archive or
// group_unarchive event (a public or a private channel) as the
// provider's state of the channel's conversation: the room is the whole
// state, so the report is complete. Slack names who changed it, and a
// change the app's own bot made is reported too: it is the echo of the
// operation that asked for it.
func (x *Instance) archiveState(eventID string, ev slackEvent) ([]delivery, error) {
	if !slackID.MatchString(ev.Channel) || !tsPattern.MatchString(ev.EventTS) {
		return nil, malformed(ev.Type + " channel or event_ts")
	}
	at := slackTime(ev.EventTS)
	env := model.Envelope{
		EventID: eventID, Destination: x.cfg.TeamID, Kind: model.EnvelopeConversationState,
		SystemActor: true, Assurance: "verified", OccurredAt: at,
		State: &model.ProviderStateReport{
			Conversation: x.roomRef(ev.Channel),
			State:        model.ProviderConversationState{Channel: x.channel.Ref, Archived: ev.Type == "channel_archive" || ev.Type == "group_archive", ObservedAt: at, Complete: true},
		},
	}
	return []delivery{{ingress: x.ingress(env)}}, nil
}

var _ adapter.Archiver = (*Instance)(nil)
