package model

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestListEventsValidate(t *testing.T) {
	inboxes := Filter{Inboxes: []InboxRef{inboxA}, Kinds: []string{EventMessageReceived}}
	since := time.Unix(1700000000, 0).UTC()
	runRequestCases(t, map[string]requestCase{
		"beginning":        {ListEvents{Call: call, Start: StartBeginning}, ""},
		"tail":             {ListEvents{Call: call, Start: StartTail}, ""},
		"checkpoint":       {ListEvents{Call: call, Start: StartCheckpoint, After: "c1"}, ""},
		"inboxes kinds":    {ListEvents{Call: call, Filter: inboxes, Start: StartBeginning, Limit: 1000}, ""},
		"conversation":     {ListEvents{Call: call, Filter: Filter{Conversation: &convA}, Start: StartBeginning}, ""},
		"since":            {ListEvents{Call: call, Filter: Filter{Since: &since}, Start: StartBeginning}, ""},
		"bad conversation": {ListEvents{Call: call, Start: StartTail, Filter: Filter{Conversation: &ConversationRef{Inbox: inboxA}}}, "filter.conversation.id"},
		"limit over cap":   {ListEvents{Call: call, Start: StartTail, Limit: 5000}, ""},
		"no correlation":   {ListEvents{Start: StartBeginning}, "correlation"},
		"no start":         {ListEvents{Call: call}, "start"},
		"bad start":        {ListEvents{Call: call, Start: "latest"}, "start"},
		"no after":         {ListEvents{Call: call, Start: StartCheckpoint}, "after"},
		"stray after":      {ListEvents{Call: call, Start: StartTail, After: "c1"}, "after"},
		"stray after head": {ListEvents{Call: call, Start: StartBeginning, After: "c1"}, "after"},
		"negative limit":   {ListEvents{Call: call, Start: StartTail, Limit: -1}, "limit"},
		"bad inbox":        {ListEvents{Call: call, Start: StartTail, Filter: Filter{Inboxes: []InboxRef{{Tenant: tenantA}}}}, "filter.inboxes[0].id"},
		"bad kind":         {ListEvents{Call: call, Start: StartTail, Filter: Filter{Kinds: []string{" x"}}}, "filter.kinds[0]"},
	})
}

// Events.List honours Filter.Inboxes, Conversation, Kinds and Since only:
// every other set field is Validation, never ignored.
func TestListEventsRejectsUnsupportedFilter(t *testing.T) {
	at, has := time.Unix(1700000000, 0).UTC(), true
	runRequestCases(t, map[string]requestCase{
		"contact":     {ListEvents{Call: call, Start: StartTail, Filter: Filter{Contact: &contact}}, "filter.contact"},
		"statuses":    {ListEvents{Call: call, Start: StartTail, Filter: Filter{Statuses: []string{"open"}}}, "filter.statuses"},
		"identity":    {ListEvents{Call: call, Start: StartTail, Filter: Filter{Identity: &ident}}, "filter.identity"},
		"labels":      {ListEvents{Call: call, Start: StartTail, Filter: Filter{Labels: &LabelSelector{Any: []LabelRef{labelA}}}}, "filter.labels"},
		"task":        {ListEvents{Call: call, Start: StartTail, Filter: Filter{Task: &task}}, "filter.task"},
		"text":        {ListEvents{Call: call, Start: StartTail, Filter: Filter{Text: "hello"}}, "filter.text"},
		"before":      {ListEvents{Call: call, Start: StartTail, Filter: Filter{Before: &at}}, "filter.before"},
		"attachments": {ListEvents{Call: call, Start: StartTail, Filter: Filter{Attachments: &AttachmentFilter{Has: &has}}}, "filter.attachments"},
		"actor":       {ListEvents{Call: call, Start: StartTail, Filter: Filter{Actor: new(ActorID("staff"))}}, "filter.actor"},
	})
}

// A Filter field added later is refused by Events.List until it learns to
// honour it: every field besides Inboxes, Conversation, Kinds and Since,
// whatever its type, makes eventListFields fail when set.
func TestEventListRejectsEveryOtherFilterField(t *testing.T) {
	honoured := map[string]bool{"Inboxes": true, "Conversation": true, "Kinds": true, "Since": true}
	typ := reflect.TypeOf(Filter{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		var f Filter
		set := reflect.ValueOf(&f).Elem().Field(i)
		switch field.Type.Kind() {
		case reflect.Pointer:
			set.Set(reflect.New(field.Type.Elem()))
		case reflect.Slice:
			set.Set(reflect.MakeSlice(field.Type, 1, 1))
		case reflect.String:
			set.SetString("x")
		default:
			t.Fatalf("Filter.%s has kind %s: teach this test to set it", field.Name, field.Type.Kind())
		}
		err := f.eventListFields("filter")
		switch {
		case honoured[field.Name] && err != nil:
			t.Errorf("Filter.%s is honoured but rejected: %v", field.Name, err)
		case !honoured[field.Name] && (err == nil || !strings.Contains(err.Error(), "not supported by Events.List")):
			t.Errorf("Filter.%s set: error = %v, want it refused as unsupported by Events.List", field.Name, err)
		}
	}
}

// The event log resource is tenant-scoped, one per tenant, with a fixed ID.
func TestEventLogRef(t *testing.T) {
	a, b := EventLogRef(tenantA), EventLogRef(tenantB)
	requireOK(t, a.Validate())
	if a.Kind != KindEvent || a.Tenant != tenantA || a.Inbox != "" || a.ID == "" {
		t.Fatalf("EventLogRef(%q) = %+v", tenantA, a)
	}
	if a.ID != b.ID || b.Tenant != tenantB {
		t.Fatalf("event log IDs differ across tenants: %+v, %+v", a, b)
	}
	requireOK(t, CheckScope(tenantA, a))
	requireError(t, CheckScope(tenantB, a), NotFound, "")
	requireError(t, Ref{Tenant: tenantA, Kind: KindEvent, Inbox: idA, ID: a.ID}.Validate(), Validation, "inbox")
}
