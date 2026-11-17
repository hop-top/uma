package conformance

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"hop.top/uma"
	"hop.top/uma/model"
)

// labelsOtherTenant is a tenant that owns nothing the suite created.
const labelsOtherTenant model.TenantID = "conformance-other"

// labelMoreCases extend labelServiceCases: replay and stale revisions of the
// label calls, the whole selector truth table, label events through
// Events.List and a subscription, and a principal of another tenant.
var labelMoreCases = []struct {
	name string
	run  func(*testing.T, *labelsEnv)
}{
	{"ReplayAndStaleRevision", labelsReplay},
	{"SelectorTruthTable", labelsTruthTable},
	{"SelectorRefusals", labelsSelectorRefusals},
	{"EventsThroughListAndSubscription", labelsEventsListed},
	{"ForeignTenant", labelsForeignTenant},
}

// labelsReplay: a retry of an add with the same operation returns the
// stored one and changes nothing; the same operation with another payload
// or on RemoveLabels conflicts; a stale ExpectedRevision conflicts and
// applies nothing.
func labelsReplay(t *testing.T, env *labelsEnv) {
	a, b := env.label("A").Ref, env.label("B").Ref
	req := model.ConversationLabels{Command: env.cmd(""), Conversation: env.one.Ref, Labels: []model.LabelRef{a}}
	first, err := env.client.Conversations().AddLabels(env.ctx, req)
	if err != nil || first.Replayed {
		t.Fatalf("AddLabels = %+v, %v", first, err)
	}
	after := env.get(env.one.Ref)
	again, err := env.client.Conversations().AddLabels(env.ctx, req)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if got := env.get(env.one.Ref); got.Revision != after.Revision || len(env.events()) != 1 {
		t.Fatalf("replay changed state: %+v, %d events", got, len(env.events()))
	}
	other := req
	other.Labels = []model.LabelRef{b}
	_, err = env.client.Conversations().AddLabels(env.ctx, other)
	serviceCode(t, err, model.Conflict)
	_, err = env.client.Conversations().RemoveLabels(env.ctx, req)
	serviceCode(t, err, model.Conflict)

	stale := model.ConversationLabels{Command: env.cmd(env.one.Revision), Conversation: env.one.Ref, Labels: []model.LabelRef{b}}
	_, err = env.client.Conversations().AddLabels(env.ctx, stale)
	serviceCode(t, err, model.Conflict)
	if got := env.get(env.one.Ref); !slices.Equal(got.Labels, []model.LabelRef{a}) || len(env.events()) != 1 {
		t.Fatalf("stale add applied: %+v", got)
	}
	fresh := model.ConversationLabels{Command: env.cmd(after.Revision), Conversation: env.one.Ref, Labels: []model.LabelRef{b}}
	if _, err := env.client.Conversations().AddLabels(env.ctx, fresh); err != nil {
		t.Fatalf("add at the current revision: %v", err)
	}
}

// labelsTruthTable: Conversations.List over every combination of the
// selector's parts, on conversations holding {a}, {a b}, {b} and nothing.
func labelsTruthTable(t *testing.T, env *labelsEnv) {
	a, b, c := env.label("A").Ref, env.label("B").Ref, env.label("C").Ref
	unknown := model.LabelRef{Tenant: serviceTenant, ID: "00000000-0000-4000-8000-00000000dead"}
	one, two := env.one.Ref, env.two.Ref
	three, four := env.conversation(env.one.Ref.Inbox).Ref, env.conversation(env.one.Ref.Inbox).Ref
	for _, h := range []struct {
		conv   model.ConversationRef
		labels []model.LabelRef
	}{{one, []model.LabelRef{a}}, {two, []model.LabelRef{a, b}}, {three, []model.LabelRef{b}}} {
		if _, err := env.add(h.conv, h.labels...); err != nil {
			t.Fatal(err)
		}
	}
	every := []model.ConversationRef{one, two, three, four}
	refs := func(l ...model.LabelRef) []model.LabelRef { return l }
	for _, tc := range []struct {
		name string
		sel  model.LabelSelector
		want []model.ConversationRef
	}{
		{"any a", model.LabelSelector{Any: refs(a)}, []model.ConversationRef{one, two}},
		{"any a b", model.LabelSelector{Any: refs(a, b)}, []model.ConversationRef{one, two, three}},
		{"any unassigned", model.LabelSelector{Any: refs(c)}, nil},
		{"all a", model.LabelSelector{All: refs(a)}, []model.ConversationRef{one, two}},
		{"all a b", model.LabelSelector{All: refs(a, b)}, []model.ConversationRef{two}},
		{"all a c", model.LabelSelector{All: refs(a, c)}, nil},
		{"none a", model.LabelSelector{None: refs(a)}, []model.ConversationRef{three, four}},
		{"none a b", model.LabelSelector{None: refs(a, b)}, []model.ConversationRef{four}},
		{"none unassigned", model.LabelSelector{None: refs(c)}, every},
		{"any a, all b", model.LabelSelector{Any: refs(a), All: refs(b)}, []model.ConversationRef{two}},
		{"any a, none b", model.LabelSelector{Any: refs(a), None: refs(b)}, []model.ConversationRef{one}},
		{"any a b, none c", model.LabelSelector{Any: refs(a, b), None: refs(c)}, []model.ConversationRef{one, two, three}},
		{"all a, none b", model.LabelSelector{All: refs(a), None: refs(b)}, []model.ConversationRef{one}},
		{"any b, all a, none c", model.LabelSelector{Any: refs(b), All: refs(a), None: refs(c)}, []model.ConversationRef{two}},
		{"any unknown", model.LabelSelector{Any: refs(unknown)}, nil},
		{"any a unknown", model.LabelSelector{Any: refs(a, unknown)}, []model.ConversationRef{one, two}},
		{"all unknown", model.LabelSelector{All: refs(unknown)}, nil},
		{"none unknown", model.LabelSelector{None: refs(unknown)}, every},
	} {
		sel := tc.sel
		var got []model.ConversationRef
		var after model.Cursor
		for range 100 {
			p, err := env.client.Conversations().List(env.ctx, serviceList(model.Filter{Labels: &sel}, 2, after))
			if err != nil {
				t.Fatalf("%s: List: %v", tc.name, err)
			}
			for _, cv := range p.Items {
				got = append(got, cv.Ref)
			}
			if p.Complete {
				break
			}
			after = p.Next
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// labelsSelectorRefusals: a selector that names nothing, contradicts itself,
// repeats a label or names too many is model.Validation; a label of another
// tenant is model.NotFound; Messages.List does not honour the selector.
func labelsSelectorRefusals(t *testing.T, env *labelsEnv) {
	a, b := env.label("A").Ref, env.label("B").Ref
	foreign := model.LabelRef{Tenant: labelsOtherTenant, ID: a.ID}
	many := make([]model.LabelRef, model.MaxLabelSelectorPart+1)
	for i := range many {
		many[i] = model.LabelRef{Tenant: serviceTenant, ID: model.ID(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))}
	}
	for name, tc := range map[string]struct {
		sel  model.LabelSelector
		code model.ErrorCode
	}{
		"empty":              {model.LabelSelector{}, model.Validation},
		"any and none":       {model.LabelSelector{Any: []model.LabelRef{a, b}, None: []model.LabelRef{a}}, model.Validation},
		"all and none":       {model.LabelSelector{All: []model.LabelRef{a}, None: []model.LabelRef{a}}, model.Validation},
		"repeated in a part": {model.LabelSelector{Any: []model.LabelRef{a, a}}, model.Validation},
		"part too long":      {model.LabelSelector{Any: many}, model.Validation},
		"foreign tenant":     {model.LabelSelector{Any: []model.LabelRef{foreign}}, model.NotFound},
	} {
		sel := tc.sel
		f := model.Filter{Labels: &sel}
		_, err := env.client.Conversations().List(env.ctx, serviceList(f, 10, ""))
		if model.CodeOf(err) != tc.code {
			t.Fatalf("%s: conversations error = %v, want %s", name, err, tc.code)
		}
		// Messages.List refuses the same selectors the same way.
		_, err = env.client.Messages().List(env.ctx, serviceList(f, 10, ""))
		if model.CodeOf(err) != tc.code {
			t.Fatalf("%s: messages error = %v, want %s", name, err, tc.code)
		}
	}
}

// labelsEventsListed: label events read through Events.List match the
// stored ones in order and payload and are the ones a subscription
// delivers; a principal who may not read the conversation lists none of
// them and still reaches the head.
func labelsEventsListed(t *testing.T, env *labelsEnv) {
	a, b := env.label("A").Ref, env.label("B").Ref
	if _, err := env.add(env.one.Ref, a, b); err != nil {
		t.Fatal(err)
	}
	if _, err := env.remove(env.one.Ref, a); err != nil {
		t.Fatal(err)
	}
	stored := env.events()
	if len(stored) != 3 {
		t.Fatalf("stored label events = %d, want 3", len(stored))
	}
	isLabel := func(ev model.Event) bool {
		return ev.Kind == model.EventConversationLabelAdded || ev.Kind == model.EventConversationLabelRemoved
	}
	labelIDs := func(evs []model.Event) []model.ID {
		var out []model.ID
		for _, ev := range evs {
			if isLabel(ev) {
				out = append(out, ev.ID)
			}
		}
		return out
	}
	want := make([]model.ID, len(stored))
	for i, ev := range stored {
		want[i] = ev.ID
	}
	list := func(c uma.Client) model.Page[model.Event] {
		p, err := c.Events().List(env.ctx, model.ListEvents{Call: env.call(), Start: model.StartBeginning, Limit: uma.MaxEventListLimit})
		if err != nil {
			t.Fatalf("Events.List: %v", err)
		}
		return p
	}
	p := list(env.client)
	if got := labelIDs(p.Items); !slices.Equal(got, want) {
		t.Fatalf("listed label events %v, want %v", got, want)
	}
	for _, ev := range p.Items {
		if !isLabel(ev) {
			continue
		}
		var pl model.ConversationLabelPayload
		if err := json.Unmarshal(ev.Payload.Data, &pl); err != nil || pl.Conversation != env.one.Ref || ev.Audience.Visibility != model.Internal {
			t.Fatalf("listed event %+v, payload %+v, %v", ev, pl, err)
		}
	}
	res, err := env.client.Events().Open(env.ctx, model.OpenSubscription{
		Command: env.cmd(""), Consumer: "labels", Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	batch, err := env.client.Events().Poll(env.ctx, model.Poll{Call: env.call(), Subscription: res.Subscription.Ref, Limit: 100})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got := labelIDs(batch.Events); !slices.Equal(got, want) {
		t.Fatalf("subscription label events %v, want %v", got, want)
	}

	env.policy.deny(model.ActionConversationRead)
	hidden := list(env.client)
	env.policy.deny()
	if got := labelIDs(hidden.Items); len(got) != 0 || hidden.Next != p.Next || !hidden.Complete {
		t.Fatalf("without conversation.read: %v, next %q (want %q), complete %v", got, hidden.Next, p.Next, hidden.Complete)
	}
}

// labelsForeignTenant: another tenant's principal sees none of this
// tenant's labels or label events and changes none of its conversations,
// and this tenant cannot name its labels; both keep their own labels of the
// same name.
func labelsForeignTenant(t *testing.T, env *labelsEnv) {
	l := env.label("Urgent")
	if _, err := env.add(env.one.Ref, l.Ref); err != nil {
		t.Fatal(err)
	}
	other, err := env.eng.For(model.Principal{Tenant: labelsOtherTenant, Actor: labelActor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.Labels().Get(env.ctx, model.Lookup[model.LabelRef]{Call: env.call(), Ref: l.Ref})
	serviceCode(t, err, model.NotFound)
	_, err = other.Labels().ByName(env.ctx, model.LookupKey{Call: env.call(), Key: "urgent"})
	serviceCode(t, err, model.NotFound)
	if p, err := other.Labels().List(env.ctx, serviceList(model.Filter{}, 10, "")); err != nil || len(p.Items) != 0 {
		t.Fatalf("foreign List = %+v, %v", p, err)
	}
	_, err = other.Labels().Delete(env.ctx, model.DeleteLabel{Command: env.cmd(l.Revision), Label: l.Ref})
	serviceCode(t, err, model.NotFound)
	_, err = other.Conversations().AddLabels(env.ctx, model.ConversationLabels{Command: env.cmd(""), Conversation: env.one.Ref, Labels: []model.LabelRef{l.Ref}})
	serviceCode(t, err, model.NotFound)
	_, err = other.Conversations().RemoveLabels(env.ctx, model.ConversationLabels{Command: env.cmd(""), Conversation: env.one.Ref, Labels: []model.LabelRef{l.Ref}})
	serviceCode(t, err, model.NotFound)
	_, err = env.client.Conversations().AddLabels(env.ctx, model.ConversationLabels{
		Command: env.cmd(""), Conversation: env.one.Ref, Labels: []model.LabelRef{{Tenant: labelsOtherTenant, ID: l.Ref.ID}},
	})
	if c := model.CodeOf(err); c != model.NotFound && c != model.Validation {
		t.Fatalf("foreign label on own conversation = %v", err)
	}
	if p, err := other.Events().List(env.ctx, model.ListEvents{Call: env.call(), Start: model.StartBeginning, Limit: uma.MaxEventListLimit}); err != nil || len(p.Items) != 0 {
		t.Fatalf("foreign Events.List = %+v, %v", p, err)
	}

	mine, err := other.Labels().Configure(env.ctx, model.ConfigureLabel{
		Command: env.cmd(""), Label: model.Label{Ref: model.LabelRef{Tenant: labelsOtherTenant}, Name: "Urgent"},
	})
	if err != nil {
		t.Fatalf("same name in another tenant: %v", err)
	}
	if ref, err := mine.Operation.Result.AsLabel(); err != nil || ref.Tenant != labelsOtherTenant || ref.ID == l.Ref.ID {
		t.Fatalf("foreign label = %+v, %v", ref, err)
	}
	if got, err := env.client.Labels().ByName(env.ctx, model.LookupKey{Call: env.call(), Key: "urgent"}); err != nil || got.Ref != l.Ref {
		t.Fatalf("own ByName = %+v, %v", got, err)
	}
	if got := env.get(env.one.Ref); !slices.Equal(got.Labels, []model.LabelRef{l.Ref}) || len(env.events()) != 1 {
		t.Fatalf("foreign calls changed state: %+v", got)
	}
}
