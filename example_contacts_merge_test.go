package uma_test

import (
	"fmt"

	"hop.top/uma/model"
)

// Merge merges Ada's email contact into her web chat contact. It is
// explicit and never automatic: PreviewMerge lists what would move, the
// caller passes that preview back with both contacts' revisions, and the
// authorizer must allow contact.merge on the contact merged and relink on
// every identity. The merged contact stays, with no identities and a
// redirect; each identity gets a link record of class merge, so History
// shows who moved it and why. Conversations are not touched: they keep
// their owner, which now belongs to the contact merged into.
func ExampleContacts_Merge() {
	d := newDesk()
	contacts := d.client(supervisor).Contacts()

	preview, err := contacts.PreviewMerge(d.ctx, model.PreviewMerge{
		Call: model.Call{Correlation: "desk"}, From: d.adaMail.Contact, Into: d.adaWeb.Contact,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("would move identities:", len(preview.Identities))

	req := model.MergeContacts{
		Command: d.command(model.Identity{Revision: preview.FromRevision}),
		From:    preview.From, Into: preview.Into, ExpectedIntoRevision: preview.IntoRevision,
		Identities: preview.Identities, Reason: "same_person",
	}
	_, err = d.client(triage).Contacts().Merge(d.ctx, req)
	fmt.Println("triage:", model.CodeOf(err))

	acc, err := contacts.Merge(d.ctx, req)
	if err != nil {
		panic(err)
	}
	fmt.Println("merge:", acc.Operation.State)
	again, err := contacts.Merge(d.ctx, req)
	if err != nil {
		panic(err)
	}
	fmt.Println("same operation replayed:", again.Replayed)

	fmt.Println("email identity on:", d.name(d.identity(d.adaMail.Ref).Contact))
	page, err := contacts.List(d.ctx, model.ListRequest{Call: model.Call{Correlation: "desk"}})
	if err != nil {
		panic(err)
	}
	for _, c := range page.Items {
		if c.MergedInto != nil {
			fmt.Println(d.name(c.Ref), "redirects to", d.name(*c.MergedInto))
		}
	}
	history, err := contacts.History(d.ctx, model.ListRequest{
		Call: model.Call{Correlation: "desk"}, Filter: model.Filter{Identity: &d.adaMail.Ref},
	})
	if err != nil {
		panic(err)
	}
	for _, r := range history.Items {
		fmt.Printf("%s: %s -> %s (%s)\n", r.Class, d.name(r.From), d.name(r.To), r.Reason)
	}

	stale := req
	stale.Command = d.command(model.Identity{Revision: preview.FromRevision})
	_, err = contacts.Merge(d.ctx, stale)
	fmt.Println("merging it again:", model.CodeOf(err))
	// Output:
	// would move identities: 1
	// triage: unauthorized
	// merge: committed
	// same operation replayed: true
	// email identity on: ada-web
	// ada-mail redirects to ada-web
	// merge: ada-mail -> ada-web (same_person)
	// merging it again: conflict
}
