package conformance

import (
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
)

// labelServiceCases are the conversation label cases: label definitions through
// the operation ledger, idempotent and evented add and remove, authorized
// reads and mutations, label events gated by conversation read, the
// conversation label filter and its cursors, refusing to delete an
// assigned label, and concurrent adds of one label.
var labelServiceCases = []struct {
	name string
	run  func(*testing.T, *labelsEnv)
}{
	{"DefinitionsThroughLedger", labelsDefinitions},
	{"AddRemoveIdempotentEvented", labelsAddRemove},
	{"Authorization", labelsAuthorization},
	{"EventsGatedByConversationRead", labelsEventsGated},
	{"ListByLabels", labelsListByLabels},
	{"DeleteAssignedConflict", labelsDeleteAssigned},
	{"ConcurrentAddsOneChange", labelsConcurrentAdds},
}

// labelsDefinitions: Configure creates under the creation target and
// replays; folded names clash; Get, ByName and List read; a rename at the
// label's revision replaces it.
func labelsDefinitions(t *testing.T, env *labelsEnv) {
	labels := env.client.Labels()
	cmd := env.cmd("")
	req := model.ConfigureLabel{Command: cmd, Label: model.Label{Ref: model.LabelRef{Tenant: serviceTenant}, Name: "Urgent", Color: "#ff0000"}}
	a, err := labels.Configure(env.ctx, req)
	if err != nil || a.Operation.State != model.Committed {
		t.Fatalf("Configure = %+v, %v", a, err)
	}
	ref, err := a.Operation.Result.AsLabel()
	if err != nil {
		t.Fatal(err)
	}
	found, err := env.client.Operations().Lookup(env.ctx, model.OperationLookup{
		Call: env.call(), Kind: model.ActionLabelConfigure, Target: model.CreationTarget(serviceTenant, model.KindLabel), ID: cmd.Operation,
	})
	if err != nil || found.Ref != a.Operation.Ref {
		t.Fatalf("Lookup = %+v, %v", found, err)
	}
	if again, err := labels.Configure(env.ctx, req); err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	_, err = labels.Configure(env.ctx, model.ConfigureLabel{Command: env.cmd(""), Label: model.Label{Ref: model.LabelRef{Tenant: serviceTenant}, Name: "uRGENT"}})
	serviceCode(t, err, model.Conflict)
	later := env.label("Later")
	byName, err := labels.ByName(env.ctx, model.LookupKey{Call: env.call(), Key: "URGENT"})
	if err != nil || byName.Ref != ref || byName.Color != "#ff0000" {
		t.Fatalf("ByName = %+v, %v", byName, err)
	}
	p, err := labels.List(env.ctx, serviceList(model.Filter{}, 10, ""))
	if err != nil || len(p.Items) != 2 || p.Items[0].Ref != ref || p.Items[1].Ref != later.Ref || !p.Complete {
		t.Fatalf("List = %+v, %v", p, err)
	}
	if _, err := labels.Configure(env.ctx, model.ConfigureLabel{Command: env.cmd(byName.Revision), Label: model.Label{Ref: ref, Name: "Now"}}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	_, err = labels.Configure(env.ctx, model.ConfigureLabel{Command: env.cmd(byName.Revision), Label: model.Label{Ref: ref, Name: "Again"}})
	serviceCode(t, err, model.Conflict)
	if got, err := labels.Get(env.ctx, model.Lookup[model.LabelRef]{Call: env.call(), Ref: ref}); err != nil || got.Name != "Now" || got.Color != "" {
		t.Fatalf("renamed = %+v, %v", got, err)
	}
}

// labelsAddRemove: one call moves the revision once and emits one event
// per changed label; already held or absent labels change nothing; a
// missing label fails the whole call.
func labelsAddRemove(t *testing.T, env *labelsEnv) {
	a, b := env.label("A").Ref, env.label("B").Ref
	acc, err := env.add(env.one.Ref, b, a)
	if err != nil {
		t.Fatal(err)
	}
	cur := env.get(env.one.Ref)
	want := []model.LabelRef{a, b}
	if b.ID < a.ID {
		want = []model.LabelRef{b, a}
	}
	if !slices.Equal(cur.Labels, want) || cur.Revision == env.one.Revision {
		t.Fatalf("after add = %+v", cur)
	}
	evs := env.events()
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	for i, ev := range evs {
		p := ev.payload
		if ev.Kind != model.EventConversationLabelAdded || ev.Revision != cur.Revision || ev.Audience.Visibility != model.Internal ||
			p.Label != want[i] || p.PreviousRevision != env.one.Revision || p.Trigger != model.TriggerCommand ||
			p.Operation == nil || *p.Operation != acc.Operation.Ref || p.Conversation != env.one.Ref {
			t.Fatalf("event %d = %+v %+v", i, ev.Event, p)
		}
	}
	if _, err := env.add(env.one.Ref, a); err != nil {
		t.Fatal(err)
	}
	if _, err := env.remove(env.two.Ref, a); err != nil {
		t.Fatal(err)
	}
	if got := env.get(env.one.Ref); got.Revision != cur.Revision || len(env.events()) != 2 {
		t.Fatalf("no-op calls changed state: %+v, %d events", got, len(env.events()))
	}
	ghost := model.LabelRef{Tenant: serviceTenant, ID: "00000000-0000-4000-8000-00000000dead"}
	_, err = env.remove(env.one.Ref, a, ghost)
	serviceCode(t, err, model.NotFound)
	if got := env.get(env.one.Ref); got.Revision != cur.Revision || len(got.Labels) != 2 {
		t.Fatalf("missing label applied: %+v", got)
	}
	if _, err := env.remove(env.one.Ref, a); err != nil {
		t.Fatal(err)
	}
	last := env.get(env.one.Ref)
	evs = env.events()
	if !slices.Equal(last.Labels, []model.LabelRef{b}) || len(evs) != 3 || evs[2].Kind != model.EventConversationLabelRemoved ||
		evs[2].payload.Label != a || evs[2].payload.PreviousRevision != cur.Revision {
		t.Fatalf("after remove = %+v, events %+v", last, evs)
	}
}

// labelsAuthorization: label reads need label.read per item; changing a
// conversation's labels needs conversation.label, not label.read.
func labelsAuthorization(t *testing.T, env *labelsEnv) {
	l := env.label("Urgent")
	env.policy.deny(model.ActionLabelRead)
	_, err := env.client.Labels().Get(env.ctx, model.Lookup[model.LabelRef]{Call: env.call(), Ref: l.Ref})
	serviceCode(t, err, model.Unauthorized)
	_, err = env.client.Labels().ByName(env.ctx, model.LookupKey{Call: env.call(), Key: "urgent"})
	serviceCode(t, err, model.Unauthorized)
	if p, err := env.client.Labels().List(env.ctx, serviceList(model.Filter{}, 10, "")); err != nil || len(p.Items) != 0 {
		t.Fatalf("List under denial = %+v, %v", p, err)
	}
	if _, err := env.add(env.one.Ref, l.Ref); err != nil {
		t.Fatalf("add without label.read: %v", err)
	}
	env.policy.deny(model.ActionConversationLabel)
	_, err = env.remove(env.one.Ref, l.Ref)
	serviceCode(t, err, model.Unauthorized)
	env.policy.deny(model.ActionLabelConfigure)
	_, err = env.client.Labels().Configure(env.ctx, model.ConfigureLabel{Command: env.cmd(""), Label: model.Label{Ref: model.LabelRef{Tenant: serviceTenant}, Name: "Other"}})
	serviceCode(t, err, model.Unauthorized)
}

// labelsEventsGated: label events are conversation events, delivered only
// to a subscriber who may read the conversation.
func labelsEventsGated(t *testing.T, env *labelsEnv) {
	l := env.label("Urgent")
	if _, err := env.add(env.one.Ref, l.Ref); err != nil {
		t.Fatal(err)
	}
	open := func(consumer string) model.SubscriptionRef {
		res, err := env.client.Events().Open(env.ctx, model.OpenSubscription{
			Command: env.cmd(""), Consumer: consumer, Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return res.Subscription.Ref
	}
	labelled := func(sub model.SubscriptionRef) int {
		b, err := env.client.Events().Poll(env.ctx, model.Poll{Call: env.call(), Subscription: sub, Limit: 100})
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		n := 0
		for _, ev := range b.Events {
			if ev.Kind == model.EventConversationLabelAdded {
				n++
			}
		}
		return n
	}
	env.policy.deny(model.ActionConversationRead)
	if n := labelled(open("hidden")); n != 0 {
		t.Fatalf("label events without conversation.read = %d", n)
	}
	env.policy.deny()
	if n := labelled(open("shown")); n != 1 {
		t.Fatalf("label events with conversation.read = %d, want 1", n)
	}
}

// labelsListByLabels: Conversations.List pages by Any, All and None, with
// cursors bound to the selector.
func labelsListByLabels(t *testing.T, env *labelsEnv) {
	a, b := env.label("A").Ref, env.label("B").Ref
	three := env.conversation(env.one.Ref.Inbox)
	for _, c := range []struct {
		conv   model.ConversationRef
		labels []model.LabelRef
	}{{env.one.Ref, []model.LabelRef{a}}, {env.two.Ref, []model.LabelRef{a, b}}, {three.Ref, []model.LabelRef{b}}} {
		if _, err := env.add(c.conv, c.labels...); err != nil {
			t.Fatal(err)
		}
	}
	list := func(sel model.LabelSelector) []model.ConversationRef {
		var out []model.ConversationRef
		var after model.Cursor
		for range 100 {
			p, err := env.client.Conversations().List(env.ctx, serviceList(model.Filter{Labels: &sel}, 1, after))
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			for _, c := range p.Items {
				out = append(out, c.Ref)
			}
			if p.Complete {
				return out
			}
			after = p.Next
		}
		t.Fatal("List never completed")
		return nil
	}
	for name, tc := range map[string]struct {
		sel  model.LabelSelector
		want []model.ConversationRef
	}{
		"any a":       {model.LabelSelector{Any: []model.LabelRef{a}}, []model.ConversationRef{env.one.Ref, env.two.Ref}},
		"all a b":     {model.LabelSelector{All: []model.LabelRef{a, b}}, []model.ConversationRef{env.two.Ref}},
		"none a":      {model.LabelSelector{None: []model.LabelRef{a}}, []model.ConversationRef{three.Ref}},
		"any b not a": {model.LabelSelector{Any: []model.LabelRef{b}, None: []model.LabelRef{a}}, []model.ConversationRef{three.Ref}},
	} {
		if got := list(tc.sel); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	p, err := env.client.Conversations().List(env.ctx, serviceList(model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{a}}}, 1, ""))
	if err != nil || p.Next == "" {
		t.Fatalf("first page = %+v, %v", p, err)
	}
	_, err = env.client.Conversations().List(env.ctx, serviceList(model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{b}}}, 1, p.Next))
	serviceCode(t, err, model.Validation)
	_, err = env.client.Conversations().List(env.ctx, serviceList(model.Filter{}, 1, p.Next))
	serviceCode(t, err, model.Validation)
}

// labelsDeleteAssigned: a held label cannot be deleted until removed.
func labelsDeleteAssigned(t *testing.T, env *labelsEnv) {
	l := env.label("Urgent")
	if _, err := env.add(env.one.Ref, l.Ref); err != nil {
		t.Fatal(err)
	}
	_, err := env.client.Labels().Delete(env.ctx, model.DeleteLabel{Command: env.cmd(l.Revision), Label: l.Ref})
	serviceCode(t, err, model.Conflict)
	if _, err := env.remove(env.one.Ref, l.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := env.client.Labels().Delete(env.ctx, model.DeleteLabel{Command: env.cmd(l.Revision), Label: l.Ref}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = env.client.Labels().Get(env.ctx, model.Lookup[model.LabelRef]{Call: env.call(), Ref: l.Ref})
	serviceCode(t, err, model.NotFound)
}

// labelsConcurrentAdds: concurrent adds of one label make one change and
// one event; every call commits. A call whose unit lost every engine rerun
// to the others is model.Transient and commits nothing; it is resent with
// the same operation, as a host would.
func labelsConcurrentAdds(t *testing.T, env *labelsEnv) {
	l := env.label("Urgent")
	const n = 4
	cmds := make([]model.Command, n)
	for i := range cmds {
		cmds[i] = env.cmd("")
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			req := model.ConversationLabels{Command: cmds[i], Conversation: env.one.Ref, Labels: []model.LabelRef{l.Ref}}
			deadline := time.Now().Add(30 * time.Second)
			for n := 0; ; n++ {
				_, errs[i] = env.client.Conversations().AddLabels(env.ctx, req)
				if !model.IsCode(errs[i], model.Transient) || time.Now().After(deadline) {
					return
				}
				time.Sleep(rand.N(min(time.Millisecond<<min(n, 7), 100*time.Millisecond))) // #nosec G404 -- jitter, not a secret
			}
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if got := env.get(env.one.Ref); !slices.Equal(got.Labels, []model.LabelRef{l.Ref}) {
		t.Fatalf("labels = %+v", got.Labels)
	}
	if evs := env.events(); len(evs) != 1 {
		t.Fatalf("events = %d, want one change", len(evs))
	}
}
