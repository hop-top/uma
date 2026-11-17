package conformance

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// contactWriteCases are the contact write cases of RunServiceSuite:
// Contacts.Create and Contacts.Update through the operation ledger, their
// authorization apart from channel configuration, stale revisions, events
// for the internal audience, and contacts staying unified by link alone.
var contactWriteCases = []struct {
	name string
	run  func(*testing.T, *contactsEnv)
}{
	{"CreateThroughLedger", contactsCreateThroughLedger},
	{"CreateInvalidReachesNothing", contactsCreateInvalid},
	{"CreateLinksNoIdentity", contactsCreateLinksNothing},
	{"UpdateRenamesAtRevision", contactsUpdateRenames},
	{"UpdateStaleRevisionConflicts", contactsUpdateStale},
	{"UpdateNoChangeIsQuiet", contactsUpdateNoChange},
	{"WriteNeedsContactWrite", contactsWriteAuthorization},
	{"ExternalParticipantCannotWrite", contactsExternalCannotWrite},
	{"WriteIsTenantScoped", contactsWriteTenantScope},
	{"ConcurrentUpdatesOneWins", contactsConcurrentUpdates},
	{"CascadeScrubsNameAndAttributes", contactsCascadeScrubs},
}

// sameContact reports whether two contacts are equal field by field,
// attributes included.
func sameContact(a, b model.Contact) bool { return reflect.DeepEqual(a, b) }

// createContact creates a contact as the staff principal.
func (env *contactsEnv) createContact(name string, attrs ...model.Attribute) model.Contact {
	env.t.Helper()
	acc, err := env.client.Contacts().Create(env.ctx, model.CreateContact{
		Command: env.cmd(""), Tenant: serviceTenant, DisplayName: name, Attributes: attrs,
	})
	if err != nil {
		env.t.Fatalf("Create %q: %v", name, err)
	}
	ref, err := acc.Operation.Result.AsContact()
	if err != nil {
		env.t.Fatal(err)
	}
	return env.getContact(ref)
}

func (env *contactsEnv) getContact(ref model.ContactRef) model.Contact {
	env.t.Helper()
	ct, err := env.client.Contacts().Get(env.ctx, model.Lookup[model.ContactRef]{Call: model.Call{Correlation: "corr"}, Ref: ref})
	if err != nil {
		env.t.Fatalf("Get %s: %v", ref.ID, err)
	}
	return ct
}

func (env *contactsEnv) updateReq(ct model.Contact, name string, attrs ...model.Attribute) model.UpdateContact {
	return model.UpdateContact{Command: env.cmd(ct.Revision), Contact: ct.Ref, DisplayName: name, Attributes: attrs}
}

// contactEvents returns the contact.created and contact.updated events in
// log order, as the staff principal reads them.
func (env *contactsEnv) contactEvents() []model.Event {
	env.t.Helper()
	return env.contactEventsAs(env.client)
}

func (env *contactsEnv) contactEventsAs(c uma.Client) []model.Event {
	env.t.Helper()
	var out []model.Event
	var after model.Cursor
	for range 100 {
		p, err := c.Events().List(env.ctx, model.ListEvents{
			Call: model.Call{Correlation: "corr"}, Filter: model.Filter{Kinds: []string{model.EventContactCreated, model.EventContactUpdated}},
			Start: model.StartBeginning, After: after, Limit: 100,
		})
		if err != nil {
			env.t.Fatalf("Events.List: %v", err)
		}
		out = append(out, p.Items...)
		if p.Complete {
			return out
		}
		after = p.Next
	}
	env.t.Fatal("event list never completed")
	return nil
}

var (
	ana  = model.Attribute{Kind: model.AttributeEmail, Value: "ana@example.com"}
	anaP = model.Attribute{Kind: model.AttributePhone, Value: "+14155550100"}
	anaB = model.Attribute{Kind: model.AttributeEmail, Value: "ana@example.org"}
)

// contactsCreateThroughLedger: Create commits under the creation target,
// stores the contact with its kind, name and sorted attributes, announces
// it to the internal audience, replays under the same operation ID and
// conflicts under another payload.
func contactsCreateThroughLedger(t *testing.T, env *contactsEnv) {
	req := model.CreateContact{
		Command: env.cmd(""), Tenant: serviceTenant, DisplayName: "Ana", Attributes: []model.Attribute{anaP, anaB, ana},
	}
	acc, err := env.client.Contacts().Create(env.ctx, req)
	if err != nil || acc.Operation.State != model.Committed || acc.Replayed {
		t.Fatalf("Create = %+v, %v", acc, err)
	}
	ref, err := acc.Operation.Result.AsContact()
	if err != nil {
		t.Fatal(err)
	}
	got := env.getContact(ref)
	if got.Kind != model.ContactKindPerson || got.DisplayName != "Ana" || got.Revision == "" ||
		!slices.Equal(got.Attributes, []model.Attribute{ana, anaB, anaP}) {
		t.Fatalf("contact = %+v", got)
	}
	target := model.CreationTarget(serviceTenant, model.KindContact)
	found, err := env.client.Operations().Lookup(env.ctx, model.OperationLookup{
		Call: model.Call{Correlation: "corr"}, Kind: model.ActionContactWrite, Target: target, ID: req.Operation,
	})
	if err != nil || found.Ref != acc.Operation.Ref {
		t.Fatalf("Lookup = %+v, %v", found, err)
	}
	again, err := env.client.Contacts().Create(env.ctx, req)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if n := len(env.contacts(model.Filter{}, 10)); n != 1 {
		t.Fatalf("replay created another contact: %d contacts", n)
	}
	other := req
	other.DisplayName = "Anna"
	_, err = env.client.Contacts().Create(env.ctx, other)
	serviceCode(t, err, model.Conflict)

	evs := env.contactEvents()
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want one", evs)
	}
	var p model.ContactCreatedPayload
	if err := json.Unmarshal(evs[0].Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if evs[0].Kind != model.EventContactCreated || evs[0].Payload.Name != model.SchemaContactCreated ||
		evs[0].Resource != ref.Ref() || evs[0].Inbox != "" || evs[0].Revision != got.Revision ||
		evs[0].Audience.Visibility != model.Internal || evs[0].Actor != contactsActor ||
		p.Contact != ref || p.Kind != model.ContactKindPerson || p.Trigger != model.TriggerCommand || p.Operation != acc.Operation.Ref {
		t.Fatalf("event = %+v payload %+v", evs[0], p)
	}
	for _, secret := range []string{"Ana", "ana@example"} {
		if raw := string(evs[0].Payload.Data); raw == "" || strings.Contains(raw, secret) {
			t.Fatalf("payload carries %q: %s", secret, raw)
		}
	}

	bot, err := env.client.Contacts().Create(env.ctx, model.CreateContact{
		Command: env.cmd(""), Tenant: serviceTenant, Kind: model.ContactKindBot, DisplayName: "Helper",
	})
	if err != nil {
		t.Fatal(err)
	}
	botRef, _ := bot.Operation.Result.AsContact()
	if c := env.getContact(botRef); c.Kind != model.ContactKindBot || c.Attributes != nil {
		t.Fatalf("bot contact = %+v", c)
	}
}

// contactsCreateInvalid: a request failing validation reaches neither the
// authorizer nor the ledger, and creates nothing.
func contactsCreateInvalid(t *testing.T, env *contactsEnv) {
	env.pol.take()
	bad := []model.CreateContact{
		{Command: env.cmd(""), Tenant: serviceTenant},
		{Command: env.cmd(""), Tenant: serviceTenant, DisplayName: "A", Kind: "company"},
		{Command: env.cmd(""), Tenant: serviceTenant, DisplayName: "A", Attributes: []model.Attribute{{Kind: model.AttributeEmail, Value: "a@example.com", Verified: true}}},
		{Command: env.cmd(""), Tenant: serviceTenant, DisplayName: "A", Attributes: []model.Attribute{ana, ana}},
		{Command: env.cmd("r1"), Tenant: serviceTenant, DisplayName: "A"},
	}
	for i, req := range bad {
		_, err := env.client.Contacts().Create(env.ctx, req)
		serviceCode(t, err, model.Validation)
		if got := env.pol.take(); len(got) != 0 {
			t.Fatalf("case %d reached the authorizer: %+v", i, got)
		}
		if env.claimed(model.ActionContactWrite, model.CreationTarget(serviceTenant, model.KindContact), req.Operation) {
			t.Fatalf("case %d claimed an operation", i)
		}
	}
	if n := len(env.contacts(model.Filter{}, 10)); n != 0 {
		t.Fatalf("%d contacts created", n)
	}
	if evs := env.contactEvents(); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
}

// contactsCreateLinksNothing: a created contact has no identity and no
// link history; it becomes the target of a Link like any other contact, a
// Link that changes neither its revision nor its fields, and a contact of
// another kind refuses the identity.
func contactsCreateLinksNothing(t *testing.T, env *contactsEnv) {
	ct := env.createContact("Ana", ana)
	if got := env.identities(model.Filter{Contact: &ct.Ref}, 10); len(got) != 0 {
		t.Fatalf("identities = %+v", got)
	}
	if got := env.history(model.Filter{Contact: &ct.Ref}, 10); len(got) != 0 {
		t.Fatalf("history = %+v", got)
	}
	id, _ := env.arrive(env.one, "alice")
	home, err := env.contact(id.Contact)
	if err != nil {
		t.Fatal(err)
	}
	env.mustLink(&id, ct.Ref, "ref-1")
	if id.Contact != ct.Ref {
		t.Fatalf("identity on %+v, want the created contact", id.Contact)
	}
	if got, err := env.contact(ct.Ref); err != nil || !sameContact(got, ct) {
		t.Fatalf("contact after link = %+v, %v; want %+v", got, err, ct)
	}
	if got, err := env.contact(home.Ref); err != nil || !sameContact(got, home) {
		t.Fatalf("provisional contact changed: %+v, %v", got, err)
	}
	botContact := env.createBot("Helper")
	person, _ := env.arrive(env.two, "bob")
	_, err = env.link(env.linkReq(person, botContact.Ref, "ref-2"))
	serviceCode(t, err, model.Validation)
}

func (env *contactsEnv) createBot(name string) model.Contact {
	env.t.Helper()
	acc, err := env.client.Contacts().Create(env.ctx, model.CreateContact{
		Command: env.cmd(""), Tenant: serviceTenant, Kind: model.ContactKindBot, DisplayName: name,
	})
	if err != nil {
		env.t.Fatal(err)
	}
	ref, _ := acc.Operation.Result.AsContact()
	return env.getContact(ref)
}

// contactsUpdateRenames: Update at the contact's revision replaces name and
// attributes, moves the revision, keeps kind and identities, and announces
// the fields that changed.
func contactsUpdateRenames(t *testing.T, env *contactsEnv) {
	id, _ := env.arrive(env.one, "alice")
	claimed := env.getContact(id.Contact)
	if claimed.DisplayName != "" {
		t.Fatalf("claimed contact = %+v", claimed)
	}
	req := env.updateReq(claimed, "Alice", anaP, ana)
	acc, err := env.client.Contacts().Update(env.ctx, req)
	if err != nil || acc.Operation.State != model.Committed {
		t.Fatalf("Update = %+v, %v", acc, err)
	}
	if res, err := acc.Operation.Result.AsContact(); err != nil || res != claimed.Ref {
		t.Fatalf("result = %+v, %v", res, err)
	}
	got := env.getContact(claimed.Ref)
	if got.DisplayName != "Alice" || got.Kind != claimed.Kind || got.Revision == claimed.Revision ||
		!slices.Equal(got.Attributes, []model.Attribute{ana, anaP}) {
		t.Fatalf("after update = %+v, was %+v", got, claimed)
	}
	if after := env.identity(id.Ref); after.Revision != id.Revision || after.Contact != id.Contact {
		t.Fatalf("identity changed: %+v, was %+v", after, id)
	}
	found, err := env.client.Operations().Lookup(env.ctx, model.OperationLookup{
		Call: model.Call{Correlation: "corr"}, Kind: model.ActionContactWrite, Target: claimed.Ref.Ref(), ID: req.Operation,
	})
	if err != nil || found.Ref != acc.Operation.Ref {
		t.Fatalf("Lookup = %+v, %v", found, err)
	}

	rename := env.updateReq(got, "Alice M.", anaP, ana)
	if _, err := env.client.Contacts().Update(env.ctx, rename); err != nil {
		t.Fatal(err)
	}
	last := env.getContact(claimed.Ref)
	if last.DisplayName != "Alice M." || !slices.Equal(last.Attributes, got.Attributes) || last.Revision == got.Revision {
		t.Fatalf("after rename = %+v", last)
	}
	evs := env.contactEvents()
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	want := [][]string{
		{model.ContactChangeDisplayName, model.ContactChangeAttributes},
		{model.ContactChangeDisplayName},
	}
	prev := []model.Revision{claimed.Revision, got.Revision}
	revs := []model.Revision{got.Revision, last.Revision}
	for i, ev := range evs {
		var p model.ContactUpdatedPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			t.Fatal(err)
		}
		if ev.Kind != model.EventContactUpdated || ev.Payload.Name != model.SchemaContactUpdated || ev.Resource != claimed.Ref.Ref() ||
			ev.Revision != revs[i] || ev.Audience.Visibility != model.Internal ||
			p.Contact != claimed.Ref || !slices.Equal(p.Changes, want[i]) || p.PreviousRevision != prev[i] || p.Trigger != model.TriggerCommand {
			t.Fatalf("event %d = %+v payload %+v", i, ev, p)
		}
	}
	if len(env.linkedEvents()) != 0 {
		t.Fatal("update emitted identity.linked")
	}
}

// contactsUpdateStale: a stale revision conflicts with reconcile_first,
// claims nothing and changes nothing; a retry of an applied update replays
// even though the revision has moved.
func contactsUpdateStale(t *testing.T, env *contactsEnv) {
	ct := env.createContact("Ana")
	first := env.updateReq(ct, "Ana M.")
	if _, err := env.client.Contacts().Update(env.ctx, first); err != nil {
		t.Fatal(err)
	}
	moved := env.getContact(ct.Ref)
	stale := env.updateReq(ct, "Someone Else")
	_, err := env.client.Contacts().Update(env.ctx, stale)
	requireReconcile(t, err)
	if env.claimed(model.ActionContactWrite, ct.Ref.Ref(), stale.Operation) {
		t.Fatal("stale update claimed its operation")
	}
	if got := env.getContact(ct.Ref); !sameContact(got, moved) {
		t.Fatalf("stale update changed the contact: %+v, was %+v", got, moved)
	}
	if n := len(env.contactEvents()); n != 2 {
		t.Fatalf("events = %d, want created and one update", n)
	}
	again, err := env.client.Contacts().Update(env.ctx, first)
	if err != nil || !again.Replayed {
		t.Fatalf("replay after the revision moved = %+v, %v", again, err)
	}
	// The same operation ID with another payload still conflicts.
	changed := first
	changed.DisplayName = "Ana N."
	_, err = env.client.Contacts().Update(env.ctx, changed)
	serviceCode(t, err, model.Conflict)
}

// contactsUpdateNoChange: an update that changes nothing commits with the
// revision and the log unchanged, but a stale one still conflicts.
func contactsUpdateNoChange(t *testing.T, env *contactsEnv) {
	ct := env.createContact("Ana", ana, anaP)
	acc, err := env.client.Contacts().Update(env.ctx, env.updateReq(ct, "Ana", anaP, ana))
	if err != nil || acc.Operation.State != model.Committed {
		t.Fatalf("Update = %+v, %v", acc, err)
	}
	if got := env.getContact(ct.Ref); !sameContact(got, ct) {
		t.Fatalf("no-op update changed the contact: %+v, was %+v", got, ct)
	}
	if n := len(env.contactEvents()); n != 1 {
		t.Fatalf("events = %d, want only the creation", n)
	}
	stale := env.updateReq(ct, "Ana")
	stale.ExpectedRevision = "stale"
	_, err = env.client.Contacts().Update(env.ctx, stale)
	requireReconcile(t, err)
}

// contactsWriteAuthorization: both calls ask contact.write, with the
// creation target or the contact at its revision, never channel.configure;
// a denial changes nothing; denying channel configuration blocks nothing.
func contactsWriteAuthorization(t *testing.T, env *contactsEnv) {
	ct := env.createContact("Ana")
	env.pol.set(nil)
	if _, err := env.client.Contacts().Update(env.ctx, env.updateReq(ct, "Ana M.")); err != nil {
		t.Fatal(err)
	}
	asked := env.pol.take()
	if len(asked) == 0 {
		t.Fatal("update asked nothing")
	}
	for _, a := range asked {
		if a.Action != model.ActionContactWrite || a.Resource != ct.Ref.Ref() || a.Revision != ct.Revision {
			t.Fatalf("update asked %+v", a)
		}
	}
	moved := env.getContact(ct.Ref)
	env.pol.take()
	if _, err := env.client.Contacts().Create(env.ctx, model.CreateContact{Command: env.cmd(""), Tenant: serviceTenant, DisplayName: "Bo"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range env.pol.take() {
		if a.Action != model.ActionContactWrite || a.Resource != model.CreationTarget(serviceTenant, model.KindContact) {
			t.Fatalf("create asked %+v", a)
		}
	}

	env.pol.set(func(a policy.Authorization) bool { return a.Action == model.ActionContactWrite })
	create := model.CreateContact{Command: env.cmd(""), Tenant: serviceTenant, DisplayName: "Cy"}
	_, err := env.client.Contacts().Create(env.ctx, create)
	serviceCode(t, err, model.Unauthorized)
	update := env.updateReq(moved, "Renamed")
	_, err = env.client.Contacts().Update(env.ctx, update)
	serviceCode(t, err, model.Unauthorized)
	env.pol.set(nil)
	if got := env.getContact(ct.Ref); !sameContact(got, moved) {
		t.Fatalf("denied update changed the contact: %+v", got)
	}
	if n := len(env.contacts(model.Filter{}, 10)); n != 2 {
		t.Fatalf("%d contacts, want 2", n)
	}
	if env.claimed(model.ActionContactWrite, ct.Ref.Ref(), update.Operation) ||
		env.claimed(model.ActionContactWrite, model.CreationTarget(serviceTenant, model.KindContact), create.Operation) {
		t.Fatal("a denied write claimed its operation")
	}
	if n := len(env.contactEvents()); n != 3 {
		t.Fatalf("events = %d, want 3", n)
	}

	env.pol.set(func(a policy.Authorization) bool {
		return a.Action == model.ActionChannelConfigure || a.Action == model.ActionInboxConfigure
	})
	if _, err := env.client.Contacts().Update(env.ctx, env.updateReq(moved, "Ana N.")); err != nil {
		t.Fatalf("channel configuration denial blocked a contact write: %v", err)
	}
	env.pol.set(nil)
}

// externalActor is a customer-side principal the policy lets read but
// never write contacts, and never see internal-audience events.
const externalActor model.ActorID = "customer-1"

// contactsExternalCannotWrite: a principal outside the desk is refused by
// the authorizer, creates and changes nothing, and does not see the
// internal-audience contact events the desk sees.
func contactsExternalCannotWrite(t *testing.T, env *contactsEnv) {
	ct := env.createContact("Ana", ana)
	env.pol.set(func(a policy.Authorization) bool {
		return a.Principal.Actor == externalActor &&
			(a.Action == model.ActionContactWrite || a.Audience.Visibility == model.Internal)
	})
	ext, err := env.engine.For(model.Principal{Tenant: serviceTenant, Actor: externalActor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ext.Contacts().Create(env.ctx, model.CreateContact{Command: env.cmd(""), Tenant: serviceTenant, DisplayName: "Mallory"})
	serviceCode(t, err, model.Unauthorized)
	_, err = ext.Contacts().Update(env.ctx, env.updateReq(ct, "Mallory"))
	serviceCode(t, err, model.Unauthorized)
	if got := env.getContact(ct.Ref); !sameContact(got, ct) {
		t.Fatalf("external update changed the contact: %+v", got)
	}
	if n := len(env.contacts(model.Filter{}, 10)); n != 1 {
		t.Fatalf("%d contacts, want 1", n)
	}
	if got := env.contactEventsAs(ext); len(got) != 0 {
		t.Fatalf("external principal reads internal events: %+v", got)
	}
	if got := env.contactEvents(); len(got) != 1 {
		t.Fatalf("desk events = %d, want 1", len(got))
	}
	env.pol.set(nil)
}

// contactsWriteTenantScope: another tenant's contact, or a creation in
// another tenant, is not found before the authorizer is asked.
func contactsWriteTenantScope(t *testing.T, env *contactsEnv) {
	env.pol.take()
	_, err := env.client.Contacts().Create(env.ctx, model.CreateContact{Command: env.cmd(""), Tenant: "conformance-other", DisplayName: "Ana"})
	serviceCode(t, err, model.NotFound)
	ct := env.createContact("Ana")
	env.pol.take()
	foreign := model.ContactRef{Tenant: "conformance-other", ID: ct.Ref.ID}
	_, err = env.client.Contacts().Update(env.ctx, model.UpdateContact{Command: env.cmd("r1"), Contact: foreign, DisplayName: "X"})
	serviceCode(t, err, model.NotFound)
	ghost := model.ContactRef{Tenant: serviceTenant, ID: "00000000-0000-4000-8000-00000000dead"}
	_, err = env.client.Contacts().Update(env.ctx, model.UpdateContact{Command: env.cmd("r1"), Contact: ghost, DisplayName: "X"})
	serviceCode(t, err, model.NotFound)
	if got := env.pol.take(); len(got) != 0 {
		t.Fatalf("scope failures reached the authorizer: %+v", got)
	}
}

// contactsConcurrentUpdates: updates racing at one revision commit once
// and the others conflict. Every update changes the display name: one that
// changes nothing is a no-op that commits at the current revision without
// moving it, so it may legitimately precede the winner and also succeed.
func contactsConcurrentUpdates(t *testing.T, env *contactsEnv) {
	ct := env.createContact("Ana")
	const n = 6
	errs := make([]error, n)
	reqs := make([]model.UpdateContact, n)
	names := make([]string, n)
	for i := range reqs {
		names[i] = "Update " + string(rune('A'+i))
		reqs[i] = env.updateReq(ct, names[i])
	}
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = env.client.Contacts().Update(env.ctx, reqs[i])
		}()
	}
	wg.Wait()
	won := -1
	for i, err := range errs {
		switch {
		case err == nil && won < 0:
			won = i
		case err == nil:
			t.Fatalf("updates %d and %d both committed", won, i)
		case !model.IsCode(err, model.Conflict):
			t.Fatalf("update %d: %v", i, err)
		}
	}
	if won < 0 {
		t.Fatal("no update committed")
	}
	if got := env.getContact(ct.Ref); got.DisplayName != names[won] {
		t.Fatalf("contact = %+v, want %q", got, names[won])
	}
	if evs := env.contactEvents(); len(evs) != 2 {
		t.Fatalf("events = %d, want created and one update", len(evs))
	}
}

// contactsCascadeScrubs: a privacy cascade on a contact also erases the
// display name and attributes a desk wrote on it, keeping the contact, its
// kind and its identities; an identity cascade leaves them.
func contactsCascadeScrubs(t *testing.T, env *contactsEnv) {
	ct := env.createContact("Ana", ana, anaP)
	id, msg := env.arrive(env.one, "alice")
	env.mustLink(&id, ct.Ref, "ref-ana")
	if _, err := env.redact(env.cascade(id.Ref.Ref(), id.Revision)); err != nil {
		t.Fatalf("identity cascade: %v", err)
	}
	if got := env.getContact(ct.Ref); !sameContact(got, ct) {
		t.Fatalf("identity cascade changed the contact: %+v", got)
	}

	acc, err := env.redact(env.cascade(ct.Ref.Ref(), ct.Revision))
	if err != nil {
		t.Fatalf("contact cascade: %v", err)
	}
	if op := env.run(acc.Operation.Ref, settled); op.State != model.Committed {
		t.Fatalf("contact cascade settled %+v", op)
	}
	got := env.getContact(ct.Ref)
	if got.DisplayName != "" || got.Attributes != nil || got.Kind != ct.Kind || got.Revision == ct.Revision {
		t.Fatalf("contact after cascade = %+v, was %+v", got, ct)
	}
	if after := env.identity(id.Ref); after.Contact != ct.Ref {
		t.Fatalf("cascade moved the identity: %+v", after)
	}
	if !env.message(msg.Ref).Redacted {
		t.Fatal("cascade left the message")
	}
}
