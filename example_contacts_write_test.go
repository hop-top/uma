package uma_test

import (
	"fmt"

	"hop.top/uma/model"
)

// writeCommand is a fresh operation expected at revision rev.
func (d *desk) writeCommand(rev model.Revision) model.Command {
	d.ops++
	return model.Command{
		Call: model.Call{Correlation: "desk"}, Operation: model.OperationID(fmt.Sprintf("op-%d", d.ops)),
		ExpectedRevision: rev,
	}
}

// Create records a contact before any channel has seen the person. It links
// no identity, so the contact stays unified by link only: until Link points
// an identity at it, it has none. Authorization is contact.write, a
// different action from channel configuration; here the desk policy lets
// the supervisor write and refuses triage.
func ExampleContacts_Create() {
	d := newDesk()
	req := model.CreateContact{
		Command: d.writeCommand(""), Tenant: deskTenant, DisplayName: "Katherine Johnson",
		Attributes: []model.Attribute{{Kind: model.AttributeEmail, Value: "katherine@example.com"}},
	}
	acc, err := d.client(supervisor).Contacts().Create(d.ctx, req)
	if err != nil {
		panic(err)
	}
	ref, err := acc.Operation.Result.AsContact()
	if err != nil {
		panic(err)
	}
	fmt.Println("create:", acc.Operation.State)

	again, err := d.client(supervisor).Contacts().Create(d.ctx, req)
	if err != nil {
		panic(err)
	}
	fmt.Println("same operation replayed:", again.Replayed)

	ct, err := d.client(supervisor).Contacts().Get(d.ctx, model.Lookup[model.ContactRef]{Call: model.Call{Correlation: "desk"}, Ref: ref})
	if err != nil {
		panic(err)
	}
	fmt.Println(ct.DisplayName, ct.Kind, ct.Attributes[0].Value)

	identities, err := d.client(supervisor).Contacts().Identities(d.ctx, model.ListRequest{
		Call: model.Call{Correlation: "desk"}, Filter: model.Filter{Contact: &ref},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("identities:", len(identities.Items))

	req.Command = d.writeCommand("")
	_, err = d.client(triage).Contacts().Create(d.ctx, req)
	fmt.Println("triage:", model.CodeOf(err))
	// Output:
	// create: committed
	// same operation replayed: true
	// Katherine Johnson person katherine@example.com
	// identities: 0
	// triage: unauthorized
}

// Update replaces a contact's display name and attributes at the revision
// Get returned. A second update at the same revision is model.Conflict: the
// caller reads the contact again and decides again. The contact's identities
// do not change.
func ExampleContacts_Update() {
	d := newDesk()
	contacts := d.client(supervisor).Contacts()
	read := func() model.Contact {
		ct, err := contacts.Get(d.ctx, model.Lookup[model.ContactRef]{Call: model.Call{Correlation: "desk"}, Ref: d.grace.Contact})
		if err != nil {
			panic(err)
		}
		return ct
	}

	before := read()
	rename := model.UpdateContact{
		Command: d.writeCommand(before.Revision), Contact: before.Ref, DisplayName: "Grace Hopper",
	}
	acc, err := contacts.Update(d.ctx, rename)
	if err != nil {
		panic(err)
	}
	fmt.Println("rename:", acc.Operation.State)
	fmt.Println("named:", read().DisplayName)

	stale := rename
	stale.Command = d.writeCommand(before.Revision) // new operation, old revision
	stale.DisplayName = "Someone Else"
	_, err = contacts.Update(d.ctx, stale)
	fmt.Println("stale revision:", model.CodeOf(err))
	fmt.Println("still named:", read().DisplayName)
	fmt.Println("identity kept:", d.identity(d.grace.Ref).Contact == before.Ref)

	_, err = d.client(triage).Contacts().Update(d.ctx, model.UpdateContact{
		Command: d.writeCommand(read().Revision), Contact: before.Ref, DisplayName: "Mallory",
	})
	fmt.Println("triage:", model.CodeOf(err))
	// Output:
	// rename: committed
	// named: Grace Hopper
	// stale revision: conflict
	// still named: Grace Hopper
	// identity kept: true
	// triage: unauthorized
}
