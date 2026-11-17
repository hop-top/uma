package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWorkflowRequests(t *testing.T) {
	until := time.Unix(1_700_000_000, 0).UTC()
	runRequestCases(t, map[string]requestCase{
		"assign":                {ConversationAssignment{Command: cmd, Conversation: convA, Assignee: "desk-1"}, ""},
		"assign at revision":    {ConversationAssignment{Command: cmdRev, Conversation: convA, Assignee: "desk-1"}, ""},
		"unassign":              {ConversationAssignment{Command: cmd, Conversation: convA}, ""},
		"assign bad actor":      {ConversationAssignment{Command: cmd, Conversation: convA, Assignee: " desk"}, "assignee"},
		"assign no operation":   {ConversationAssignment{Command: Command{Call: call}, Conversation: convA}, "operation"},
		"assign bad conv":       {ConversationAssignment{Command: cmd, Assignee: "desk-1"}, "conversation.inbox.tenant"},
		"snooze":                {ConversationSnooze{Command: cmd, Conversation: convA, Until: &until}, ""},
		"snooze at revision":    {ConversationSnooze{Command: cmdRev, Conversation: convA, Until: &until}, ""},
		"unsnooze":              {ConversationSnooze{Command: cmd, Conversation: convA}, ""},
		"snooze no operation":   {ConversationSnooze{Command: Command{Call: call}, Conversation: convA}, "operation"},
		"snooze bad conv":       {ConversationSnooze{Command: cmd, Until: &until}, "conversation.inbox.tenant"},
		"snooze no correlation": {ConversationSnooze{Command: Command{Operation: "op-1"}, Conversation: convA}, "correlation"},
	})
	if (ConversationAssignment{}).RevisionRule() != RevisionOptional || (ConversationSnooze{}).RevisionRule() != RevisionOptional {
		t.Fatal("workflow requests take an optional expected revision")
	}
}

func TestAssigneeFilter(t *testing.T) {
	for name, tc := range map[string]struct {
		f    AssigneeFilter
		want string
	}{
		"actor":       {AssigneeFilter{Actor: "desk-1"}, ""},
		"self":        {AssigneeFilter{Self: true}, ""},
		"none":        {AssigneeFilter{None: true}, ""},
		"empty":       {AssigneeFilter{}, "assignee"},
		"actor+none":  {AssigneeFilter{Actor: "desk-1", None: true}, "assignee"},
		"self+none":   {AssigneeFilter{Self: true, None: true}, "assignee"},
		"bad actor":   {AssigneeFilter{Actor: " x"}, "assignee.actor"},
		"actor+self":  {AssigneeFilter{Actor: "desk-1", Self: true}, "assignee"},
		"in a filter": {AssigneeFilter{}, "filter.assignee"},
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if name == "in a filter" {
				err = ListRequest{Call: call, Filter: Filter{Assignee: &tc.f}}.Validate()
			} else {
				err = tc.f.Validate()
			}
			if tc.want == "" {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, tc.want)
		})
	}
	desk := ActorID("desk-1")
	other := ActorID("desk-2")
	for name, tc := range map[string]struct {
		f        AssigneeFilter
		assignee *ActorID
		want     bool
	}{
		"actor match":    {AssigneeFilter{Actor: desk}, &desk, true},
		"actor other":    {AssigneeFilter{Actor: desk}, &other, false},
		"actor nil":      {AssigneeFilter{Actor: desk}, nil, false},
		"none nil":       {AssigneeFilter{None: true}, nil, true},
		"none set":       {AssigneeFilter{None: true}, &desk, false},
		"self never":     {AssigneeFilter{Self: true}, &desk, false},
		"self never nil": {AssigneeFilter{Self: true}, nil, false},
	} {
		if got := tc.f.Matches(tc.assignee); got != tc.want {
			t.Errorf("%s: Matches = %v, want %v", name, got, tc.want)
		}
	}
}

// A snooze is in effect until Until, and until an inbound message is
// recorded after At; whichever comes first wakes the conversation.
func TestSnoozeInEffect(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	s := &Snooze{Until: at.Add(time.Hour), At: at, By: "desk-1"}
	before, after := at.Add(-time.Second), at.Add(time.Second)
	for name, tc := range map[string]struct {
		s       *Snooze
		now     time.Time
		inbound *time.Time
		want    bool
	}{
		"nil":                 {nil, at, nil, false},
		"no inbound":          {s, at, nil, true},
		"inbound before":      {s, at, &before, true},
		"inbound at":          {s, at, &at, true},
		"inbound after":       {s, at, &after, false},
		"at until":            {s, s.Until, nil, false},
		"past until":          {s, s.Until.Add(time.Nanosecond), nil, false},
		"just before until":   {s, s.Until.Add(-time.Nanosecond), nil, true},
		"expired and inbound": {s, s.Until.Add(time.Hour), &after, false},
	} {
		if got := tc.s.InEffect(tc.now, tc.inbound); got != tc.want {
			t.Errorf("%s: InEffect = %v, want %v", name, got, tc.want)
		}
	}
}

// Workflow state is cloned deep and dropped whole for a reader without the
// workflow read action.
func TestConversationWorkflowCloneAndStrip(t *testing.T) {
	desk := ActorID("desk-1")
	c := Conversation{Ref: convA, Assignee: &desk, Snooze: &Snooze{By: desk}}
	d := c.Clone()
	*d.Assignee = "desk-2"
	d.Snooze.By = "desk-2"
	if *c.Assignee != desk || c.Snooze.By != desk {
		t.Fatalf("Clone shares workflow state: %+v", c)
	}
	s := c.WithoutWorkflow()
	if s.Assignee != nil || s.Snooze != nil || s.Ref != c.Ref || c.Assignee == nil {
		t.Fatalf("WithoutWorkflow = %+v from %+v", s, c)
	}
	yes := true
	f := Filter{Assignee: &AssigneeFilter{Actor: desk}, Snoozed: &yes}
	g := f.Clone()
	g.Assignee.Actor, *g.Snoozed = "desk-2", false
	if f.Assignee.Actor != desk || !*f.Snoozed {
		t.Fatalf("Filter.Clone shares workflow fields: %+v", f)
	}
}

// Workflow events carry references only, and absent assignee, previous
// and until keys drop out.
func TestWorkflowPayloadsRefsOnly(t *testing.T) {
	b, err := json.Marshal(ConversationAssignmentPayload{Conversation: convA, Trigger: TriggerCommand, PreviousRevision: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"assignee", "previous", "operation"} {
		if _, ok := m[k]; ok {
			t.Errorf("unassigned payload carries %q: %s", k, b)
		}
	}
	for _, k := range []string{EventConversationAssigned, EventConversationUnassigned, EventConversationSnoozed, EventConversationUnsnoozed} {
		if !IsWorkflowEvent(k) {
			t.Errorf("%s is not a workflow event", k)
		}
	}
	if IsWorkflowEvent(EventConversationChanged) || IsWorkflowEvent(EventConversationLabelAdded) {
		t.Error("non-workflow events reported as workflow events")
	}
}

func TestConversationLinkRequests(t *testing.T) {
	other := ConversationRef{Inbox: convA.Inbox, ID: "00000000-0000-7000-8000-0000000000ee"}
	item := WorkItemRef{Conversation: convA, ID: idB}
	foreign := ConversationRef{Inbox: InboxRef{Tenant: tenantB, ID: convA.Inbox.ID}, ID: other.ID}
	runRequestCases(t, map[string]requestCase{
		"conversation":   {ConversationLinkChange{Command: cmd, Conversation: convA, Link: ConversationLink{Conversation: &other}}, ""},
		"work item":      {ConversationLinkChange{Command: cmdRev, Conversation: convA, Link: ConversationLink{WorkItem: &item}}, ""},
		"no end":         {ConversationLinkChange{Command: cmd, Conversation: convA}, "link"},
		"both ends":      {ConversationLinkChange{Command: cmd, Conversation: convA, Link: ConversationLink{Conversation: &other, WorkItem: &item}}, "link"},
		"itself":         {ConversationLinkChange{Command: cmd, Conversation: convA, Link: ConversationLink{Conversation: &convA}}, "link.conversation"},
		"other tenant":   {ConversationLinkChange{Command: cmd, Conversation: convA, Link: ConversationLink{Conversation: &foreign}}, "link"},
		"no operation":   {ConversationLinkChange{Command: Command{Call: call}, Conversation: convA, Link: ConversationLink{Conversation: &other}}, "operation"},
		"mark":           {ReadMark{Call: call, Conversation: convA}, ""},
		"mark message":   {ReadMark{Call: call, Conversation: convA, Message: &MessageRef{Conversation: convA, ID: idB}}, ""},
		"mark elsewhere": {ReadMark{Call: call, Conversation: convA, Message: &MessageRef{Conversation: other, ID: idB}}, "message"},
		"mark no call":   {ReadMark{Conversation: convA}, "correlation"},
	})
}

// A conversation's links are sorted conversations first, unique, capped,
// in its tenant and never itself.
func TestConversationValidateLinks(t *testing.T) {
	a := ConversationRef{Inbox: convA.Inbox, ID: idA}
	b := ConversationRef{Inbox: convA.Inbox, ID: idB}
	item := WorkItemRef{Conversation: a, ID: idB}
	c := Conversation{Ref: ConversationRef{Inbox: convA.Inbox, ID: "00000000-0000-7000-8000-0000000000ff"}}
	for name, tc := range map[string]struct {
		links []ConversationLink
		want  string
	}{
		"sorted":    {[]ConversationLink{{Conversation: &a}, {Conversation: &b}, {WorkItem: &item}}, ""},
		"unsorted":  {[]ConversationLink{{WorkItem: &item}, {Conversation: &a}}, "links[1]"},
		"duplicate": {[]ConversationLink{{Conversation: &a}, {Conversation: &a}}, "links[1]"},
		"itself":    {[]ConversationLink{{Conversation: &c.Ref}}, "links[0]"},
		"no end":    {[]ConversationLink{{}}, "links[0]"},
	} {
		c.Links = tc.links
		err := c.ValidateLinks()
		if tc.want == "" {
			requireOK(t, err)
			continue
		}
		requireError(t, err, Validation, tc.want)
		_ = name
	}
}

// A message is unread for an actor when recorded after the marker, not
// its own uma-authored message and not an echo; no marker reads nothing.
func TestReadMarkerUnread(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	m := &ReadMarker{ReadThrough: at}
	msg := func(rec time.Time, edit func(*Message)) Message {
		v := Message{RecordedAt: rec}
		if edit != nil {
			edit(&v)
		}
		return v
	}
	echo := MessageRef{}
	for name, tc := range map[string]struct {
		m    *ReadMarker
		msg  Message
		want bool
	}{
		"after":          {m, msg(at.Add(time.Second), nil), true},
		"at":             {m, msg(at, nil), false},
		"before":         {m, msg(at.Add(-time.Second), nil), false},
		"no marker":      {nil, msg(at.Add(-time.Hour), nil), true},
		"own":            {m, msg(at.Add(time.Second), func(v *Message) { v.Origin = &Origin{Actor: "desk-1"} }), false},
		"someone else's": {m, msg(at.Add(time.Second), func(v *Message) { v.Origin = &Origin{Actor: "desk-2"} }), true},
		"echo":           {m, msg(at.Add(time.Second), func(v *Message) { v.EchoOf = &echo }), false},
	} {
		if got := tc.m.Unread(tc.msg, "desk-1"); got != tc.want {
			t.Errorf("%s: Unread = %v, want %v", name, got, tc.want)
		}
	}
	requireError(t, ReadMarker{Conversation: convA, Actor: "desk-1"}.Validate(), Validation, "revision")
	requireOK(t, ReadMarker{Conversation: convA, Actor: "desk-1", Revision: "r1"}.Validate())
	requireError(t, ListRequest{Call: call, Filter: Filter{Unread: &UnreadFilter{Actor: " x"}}}.Validate(), Validation, "filter.unread.actor")
}
