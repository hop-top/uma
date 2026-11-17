package conformance

import (
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// serviceHorizon is every retention horizon of the service suite's engine.
const serviceHorizon = time.Hour

// serviceRetentionPurge purges past the horizons through Runtime.Purge: a
// subscription whose checkpoint the purge passed fails Poll with
// cursor_expired, never by tailing, and one opened from the beginning
// afterwards starts at the retained history.
func serviceRetentionPurge(t *testing.T, env *serviceEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	env.policy.setStaff(true)
	early := env.open("early")
	oldNote := env.note("before the horizon")
	env.fx.Advance(2 * serviceHorizon)
	env.clock.Advance(2 * serviceHorizon)
	recent := env.note("after the horizon")

	rt := env.engine.Runtime()
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "staff-1"}, Call: model.Call{Correlation: "purge"}}
	rep, err := rt.Purge(env.ctx, call)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if rep.Events == 0 || rep.Operations == 0 || rep.EventBase == "" {
		t.Fatalf("purge report = %+v, want old events and operations purged and a base", rep)
	}
	_, err = env.client.Operations().Get(env.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c"}, Ref: oldNote.Operation.Ref})
	serviceCode(t, err, model.NotFound)
	if _, err := env.client.Operations().Get(env.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c"}, Ref: recent.Operation.Ref}); err != nil {
		t.Fatalf("recent operation: %v, want it retained", err)
	}

	_, err = env.client.Events().Poll(env.ctx, model.Poll{Call: model.Call{Correlation: "c"}, Subscription: early})
	serviceCode(t, err, model.CursorExpired)

	late := env.open("late")
	b, err := env.client.Events().Poll(env.ctx, model.Poll{Call: model.Call{Correlation: "c"}, Subscription: late})
	if err != nil {
		t.Fatalf("Poll after purge: %v", err)
	}
	if len(b.Events) == 0 {
		t.Fatal("subscription opened from the beginning after the purge sees no retained event")
	}
	for _, ev := range b.Events {
		if ev.Resource == oldNote.Operation.ResultMessage.Ref() {
			t.Errorf("purged event %s delivered", ev.ID)
		}
	}

	// Without horizons there is no purge.
	bare, err := uma.New(uma.Config{Store: env.st, Authorizer: env.policy, Clock: env.clock, IDs: env.ids})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bare.Close(env.ctx) }()
	_, err = bare.Runtime().Purge(env.ctx, call)
	serviceCode(t, err, model.Validation)
}

func (env *serviceEnv) open(consumer string) model.SubscriptionRef {
	env.t.Helper()
	res, err := env.client.Events().Open(env.ctx, model.OpenSubscription{
		Command: env.cmd(""), Consumer: consumer, Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		env.t.Fatalf("Open(%s): %v", consumer, err)
	}
	return res.Subscription.Ref
}

func (env *serviceEnv) note(text string) model.Acceptance {
	env.t.Helper()
	a, err := env.client.Messages().AddInternalNote(env.ctx, model.InternalNote{
		Command: env.cmd(""), Conversation: env.conv.Ref, PolicyRef: "staff",
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	})
	if err != nil {
		env.t.Fatalf("AddInternalNote: %v", err)
	}
	return a
}

// serviceOrphanContactsPurge purges through Runtime.Purge a contact an
// identity left by a link record written before the contacts horizon, by
// the store clock, and keeps the contact it moved to, a provisional
// contact no link left and a contact left inside the horizon. The link
// history is kept and keeps naming the purged contact.
func serviceOrphanContactsPurge(t *testing.T, env *serviceEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	moved, left := env.seedAuthor("moved", "c1")
	_, provisional := env.seedAuthor("provisional", "c1")
	target := model.Contact{Ref: model.ContactRef{Tenant: serviceTenant, ID: env.id()}, Kind: model.ContactKindPerson, Revision: "c1"}
	env.put(func(tx store.Tx) error { return tx.PutContact(env.ctx, target, "") })
	rec := env.linkTo(moved, target.Ref)
	env.fx.Advance(2 * serviceHorizon)
	env.clock.Advance(2 * serviceHorizon)
	fresh, freshLeft := env.seedAuthor("fresh", "c1")
	env.linkTo(fresh, target.Ref)

	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "staff-1"}, Call: model.Call{Correlation: "purge"}}
	rep, err := env.engine.Runtime().Purge(env.ctx, call)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if rep.Contacts != 1 {
		t.Fatalf("purge report = %+v, want the one orphan past the horizon", rep)
	}
	for _, c := range []struct {
		name   string
		ref    model.ContactRef
		exists bool
	}{
		{"orphan past the horizon", left.Ref, false},
		{"link target", target.Ref, true},
		{"provisional", provisional.Ref, true},
		{"orphan inside the horizon", freshLeft.Ref, true},
	} {
		err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
			_, err := r.GetContact(env.ctx, c.ref)
			return err
		})
		if c.exists && err != nil {
			t.Errorf("%s contact: %v, want it kept", c.name, err)
		}
		if !c.exists && !model.IsCode(err, model.NotFound) {
			t.Errorf("%s contact: %v, want it purged", c.name, err)
		}
	}
	p, err := env.client.Contacts().History(env.ctx, model.ListRequest{
		Call: model.Call{Correlation: "c"}, Filter: model.Filter{Identity: &moved.Ref}, Page: model.PageRequest{Limit: 10},
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(p.Items) != 1 || p.Items[0].ID != rec.ID || p.Items[0].From != left.Ref {
		t.Fatalf("history after the purge = %+v, want the link off the purged contact", p.Items)
	}
	again, err := env.engine.Runtime().Purge(env.ctx, call)
	if err != nil || again.Contacts != 0 {
		t.Fatalf("second purge = %+v, %v; want no contact", again, err)
	}
}

// linkTo appends a link record moving id to contact to.
func (env *serviceEnv) linkTo(id model.Identity, to model.ContactRef) store.LinkRecord {
	env.t.Helper()
	rec := store.LinkRecord{
		ID: env.id(), Tenant: serviceTenant, Identity: id.Ref,
		IdentityKey: model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: id.SourceID},
		Class:       model.LinkClassLink, From: id.Contact, To: to,
		Evidence: model.Evidence{
			Verifier: model.EvidenceKindAttestation, Assurance: model.LinkAssuranceAsserted,
			Facts: model.Extension{Name: model.SchemaEvidenceAttestation, Version: 1, Data: []byte(`{}`)},
		},
		Assurance: model.LinkAssuranceAsserted, Actor: "staff-1",
		Operation:      model.OperationRef{Tenant: serviceTenant, ID: env.id()},
		RevisionBefore: id.Revision, RevisionAfter: id.Revision + "+", RecordedAt: env.clock.Now(),
	}
	env.put(func(tx store.Tx) error { return tx.LinkIdentity(env.ctx, rec) })
	return rec
}
