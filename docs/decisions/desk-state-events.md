# Desk state events: read markers

Status: accepted.

A client that keeps an unread count from the event log alone had to re-list
after every mark, because a read marker emitted nothing. This record fixes the
events that close that gap. Every change is additive under
[compatibility.md](../compatibility.md).

## 1. Read marker moves

Decision: `Conversations.MarkRead` and `MarkUnread` append one event in the
unit that writes the marker, when the marker's `ReadThrough` actually moves.

| Field | Value |
| --- | --- |
| Kinds | `conversation.read_marked`, `conversation.unread_marked` |
| Schemas | `uma.conversation.read_marked`, `uma.conversation.unread_marked`, payload major 1 |
| Payload | `conversation`, `actor`, `revision` (the marker's new revision). References only: no message, no `read_through` |
| Resource, Inbox | the conversation; its inbox |
| Event.Actor, Audience | the marking actor; internal |
| Event.Revision | the conversation's revision, which marking never moves |

The kinds are `*_marked` because event and action names share one registry
and `conversation.read` is already the action that reads a conversation.

A call that leaves the marker as it was (the same `ReadThrough`) returns the
stored marker and emits nothing, so a retry after a lost answer adds no event
and each move is exactly one event.

Gate: the marker is the actor's own record and `Conversations.ReadMarker`
requires `conversation.read_workflow`, so the event reaches only a subscriber
that holds the inbox and conversation read rights, holds
`conversation.read_workflow` on the conversation, and is the payload's actor.
Another desk actor, however privileged on the conversation, never receives it.

Ordering: events of one actor on one conversation arrive in log order; the
payload revision equals the revision `ReadMarker` then returns, so a client can
drop a stale poll by comparing it.
