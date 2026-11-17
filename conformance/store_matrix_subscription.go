package conformance

import (
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// subscriptionHonours lists the filter fields a stored subscription honours,
// as the filter matrix in the store package documentation states for
// Events: a subscription follows the event log.
var subscriptionHonours = []string{"inboxes", "kinds"}

// subscriptionFilterCases pin that PutSubscription, which persists a filter
// as a read would apply it, rejects every filter field the event log does not
// honour with model.Validation instead of storing a filter nothing applies.
// They are driven by matrixFields, so a field added to model.Filter is
// covered once it joins the matrix.
var subscriptionFilterCases = []storeCase{
	{"SubscriptionFilterRefusesUnsupportedFields", func(t *testing.T, e *env) {
		w := e.seedMatrixWorld(t)
		for _, name := range subscriptionHonours {
			if !slices.ContainsFunc(matrixFields, func(f matrixField) bool { return f.name == name }) {
				t.Fatalf("subscription honours unknown filter field %q", name)
			}
		}
		put := func(f model.Filter) error {
			rec := store.SubscriptionRecord{
				Subscription: model.Subscription{Ref: model.SubscriptionRef{Tenant: tenantA, ID: e.id(t)}, Revision: "s1"},
				Principal:    model.Principal{Tenant: tenantA, Actor: "conformance-actor"},
				Consumer:     "bot",
				Filter:       f,
				PayloadMajor: 1,
			}
			return e.txErr(func(tx store.Tx) error { return tx.PutSubscription(e.ctx, rec, "") })
		}
		for _, f := range matrixFields {
			t.Run(f.name, func(t *testing.T) {
				var filter model.Filter
				f.set(&filter, w)
				err := put(filter)
				if slices.Contains(subscriptionHonours, f.name) {
					if err != nil {
						t.Fatalf("PutSubscription with filter.%s = %v, want nil", f.name, err)
					}
					return
				}
				if err == nil {
					t.Fatalf("PutSubscription accepted filter.%s: a stored filter the event log does not honour is model.Validation, never stored", f.name)
				}
				wantCode(t, err, model.Validation)
			})
		}
	}},
}
