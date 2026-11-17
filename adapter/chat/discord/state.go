package discord

import (
	"context"
	"net/http"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Provider conversation state. Discord holds one conversation state a bot
// changes: a thread's archived flag, for everyone in the guild (scope
// shared). A room has no archive, a direct message has none, and no other
// verb (move, read, flag, trash) exists for a conversation; they are not
// declared.
//
// Discord changes the flag on its own, and a person changes it too:
//
//   - a thread idle for its auto_archive_duration (1 hour, 1 day, 3 days or
//     7 days) is archived;
//   - a new message in an archived thread unarchives it;
//   - a person with MANAGE_THREADS, or the thread's creator, archives or
//     unarchives it in the client.
//
// Every change reaches the gateway as THREAD_UPDATE, with the time of the
// change in thread_metadata.archive_timestamp, and is reported as a
// conversation.state envelope: the bot's own change is the echo that
// settles its operation.

// archiveLimits are the limits of the archive status: the thread's state
// changes for everyone.
func archiveLimits() model.Extension {
	return model.StateLimits{Scope: model.ScopeShared}.Extension()
}

// Archive archives the thread the route names, or unarchives it with Undo
// (Modify Channel, archived). Archiving needs MANAGE_THREADS; unarchiving a
// thread that is not locked needs only SEND_MESSAGES, and a locked one
// MANAGE_THREADS ("When setting archived to false, when locked is also
// false, only the SEND_MESSAGES permission is required. Otherwise, requires
// the MANAGE_THREADS permission",
// https://docs.discord.com/developers/resources/channel). Discord's refusal
// is final. The conversation is the thread: Request.Items, the messages it
// holds, are not named, and the one call changes them all. A room or a
// direct message has no archive: rejected unsupported, with no call.
// Archiving an archived thread, and unarchiving one that is not, is
// applied: the state is what was asked.
func (x *Instance) Archive(ctx context.Context, r adapter.ArchiveRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	t, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if t.parent == "" {
		return rejected(model.Unsupported, "only a Discord thread archives"), nil
	}
	if err := x.call(ctx, http.MethodPatch, "channels/"+t.channel, map[string]any{"archived": !r.Undo}, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// threadMetadata is a thread's thread_metadata.
type threadMetadata struct {
	Archived         bool   `json:"archived"`
	ArchiveTimestamp string `json:"archive_timestamp"`
}

// threadChannel is a thread channel object as THREAD_UPDATE and Get Channel
// carry it.
type threadChannel struct {
	ID       string          `json:"id"`
	Type     int             `json:"type"`
	ParentID string          `json:"parent_id"`
	Metadata *threadMetadata `json:"thread_metadata"`
}

// reconcileArchive reads the thread: archived is applied for an archive
// and not for its undo, and the reverse; a thread that is gone is rejected
// not found. Applying the state already held is applied, so a read that
// shows the state asked is the attempt's, or an earlier one's, never
// another's.
func (x *Instance) reconcileArchive(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	t, err := x.parseRoute(req.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if t.parent == "" {
		return notAppliedOutcome(), nil
	}
	var ch threadChannel
	if err := x.call(ctx, http.MethodGet, "channels/"+t.channel, nil, &ch); err != nil {
		if isCode(err, codeUnknownChannel) || isStatus(err, http.StatusNotFound) {
			return rejected(model.NotFound, "thread not found"), nil
		}
		return failure(ctx, err, readCall)
	}
	if ch.Metadata != nil && ch.Metadata.Archived != req.Undo {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	return notAppliedOutcome(), nil
}

// threadUpdated reports THREAD_UPDATE: the thread's archive state, complete
// (the thread is the whole conversation), observed when Discord says it
// changed (archive_timestamp, else now). Other changes of the thread (its
// name, its lock) carry the same state and the same timestamp: they report
// the same event again, which changes nothing. A dispatch that is not a
// thread of a parent channel, or carries no metadata, has nothing to
// report.
func (x *Instance) threadUpdated(ch threadChannel) []delivery {
	if !isThreadType(ch.Type) || !isSnowflake(ch.ID) || !isSnowflake(ch.ParentID) || ch.Metadata == nil {
		return nil
	}
	x.learnChannel(ch.ID, ch.Type, ch.ParentID)
	at, err := time.Parse(time.RFC3339Nano, ch.Metadata.ArchiveTimestamp)
	if err != nil {
		at = x.now()
	}
	at = at.UTC()
	state := "unarchived"
	if ch.Metadata.Archived {
		state = "archived"
	}
	env := model.Envelope{
		EventID: "thread:" + ch.ID + ":" + state + ":" + at.Format(time.RFC3339Nano), Destination: x.cfg.ApplicationID,
		Kind: model.EnvelopeConversationState, SystemActor: true, Assurance: "verified", OccurredAt: at,
		State: &model.ProviderStateReport{
			Conversation: x.threadRef(ch.ParentID, ch.ID),
			State:        model.ProviderConversationState{Archived: ch.Metadata.Archived, ObservedAt: at, Complete: true},
		},
	}
	return []delivery{{ingress: ingress(env)}}
}

var _ adapter.Archiver = (*Instance)(nil)
