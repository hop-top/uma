package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
)

// snoozeAcrossRestart: a snooze is stored state judged when read, so it
// holds across a restart, as do assignees and read markers, and still wakes after one on an inbound message
// recorded after it was set and at its expiry, with no write of the
// conversation.
func snoozeAcrossRestart(t *testing.T, e *env, cross func(*testing.T)) {
	set := e.at(100)
	inbound := e.putWorkflowConversation(t, nil, &model.Snooze{Until: e.fx.Now().Add(24 * time.Hour).UTC(), At: set, By: "desk-1"})
	expiring := e.putWorkflowConversation(t, nil, &model.Snooze{Until: e.fx.Now().Add(time.Hour).UTC(), At: set, By: "desk-1"})
	desk := model.ActorID("desk-1")
	assigned := e.putWorkflowConversation(t, &desk, nil)
	e.putMarker(t, assigned, desk, e.at(5), "")
	yes := true
	cross(t)
	if got := e.readMarker(t, assigned, desk); !got.ReadThrough.Equal(e.at(5)) || got.Revision != "m1" {
		t.Fatalf("read marker after restart %+v", got)
	}
	e.wantConversations(t, model.Filter{Snoozed: &yes}, inbound, expiring)
	e.wantConversations(t, model.Filter{Assignee: &model.AssigneeFilter{Actor: desk}}, assigned)
	e.putWorkflowMessage(t, inbound, set.Add(time.Second), nil)
	cross(t)
	e.wantConversations(t, model.Filter{Snoozed: &yes}, expiring)
	if e.fx.Advance == nil {
		return
	}
	e.fx.Advance(time.Hour)
	cross(t)
	e.wantConversations(t, model.Filter{Snoozed: &yes})
}
