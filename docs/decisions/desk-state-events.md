# Desk state events: read markers and snooze end

Status: accepted.

A client that keeps an unread or waiting count from the event log alone had to
re-list, because a read marker move and a snooze ending emitted nothing. This
record fixes the events that close that gap. Every change is additive under
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

## 2. Snooze end

Decision: a snooze that ends by itself becomes a real transition. A snooze
ends when `Until` passes or a live inbound message that was not imported is
recorded after `At`; neither is a command, and until now neither wrote
anything. Readers keep the query-time rule (`Snooze.InEffect`), so a
conversation reads as awake the moment the snooze ends, with or without a
worker. A sweep then records the end.

| Field | Value |
| --- | --- |
| Kind, schema | `conversation.snooze_ended`, `uma.conversation.snooze_ended`, payload major 1 |
| Payload | `conversation`, `cause` (`until`, or `message`; `until` wins when both hold), `until`, `trigger` (`system`), `previous_revision` |
| Resource, Inbox | the conversation; its inbox |
| Event.Actor, Audience | none (system); internal |
| Gate | the workflow-event gate: inbox and conversation read plus `conversation.read_workflow` |

The sweep (`operations.WakeSnoozes`, called by `Runtime.Run` beside the
held-operation sweep on `SweepEvery`) lists the conversations selected by the
new filter `Filter.SnoozeEnded`. Per conversation, in one unit: reload, judge
the snooze again, clear `Snooze`, write the conversation at a new revision with
a compare-and-swap on the revision read, and append the event. A sweeper that
loses the swap or finds the snooze gone emits nothing, so the event is
once per snooze however many workers race, and a restart between the end and
the sweep delays it without losing it.

Why clear `Snooze` instead of marking it woken: every reader (`Get`, `List`,
`Filter.Snoozed`, the unsnooze no-op rule) already treats a missing snooze as
awake, so a marker would add a field and a filter state to every backend. The
cost is that the snooze's history lives in the event log, and the conversation
revision moves once at the wake.

Without a running worker nothing is swept: the snooze stays stored, readers
still see the conversation awake, and no event is emitted. A conversation
listed under `Filter.SnoozeEnded=true` is therefore transient once a worker
runs. The filter is a workflow filter like `Snoozed`; subscriptions and exports
refuse it.
