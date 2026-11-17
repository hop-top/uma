package conformance

import (
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

var inboxCases = []storeCase{
	{"DisabledRoundTrips", func(t *testing.T, e *env) {
		in := model.Inbox{
			Ref: model.InboxRef{Tenant: tenantA, ID: e.id(t)}, Channel: e.channel, Name: "off",
			Destinations: []string{"off@example.test"}, Disabled: true, Revision: "r1",
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutInbox(e.ctx, in, "") })
		check := func(want bool) {
			t.Helper()
			e.mustView(t, func(r store.Reader) error {
				got, err := r.GetInbox(e.ctx, in.Ref)
				if err == nil && got.Disabled != want {
					t.Errorf("GetInbox Disabled = %v, want %v", got.Disabled, want)
				}
				res, err := r.ResolveDestination(e.ctx, e.channel, "off@example.test")
				if err == nil && res.Disabled != want {
					t.Errorf("ResolveDestination Disabled = %v, want %v", res.Disabled, want)
				}
				return err
			})
		}
		check(true)
		in.Disabled, in.Revision = false, "r2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutInbox(e.ctx, in, "r1") })
		check(false)
	}},
}
