package conformance

import (
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// keyCases cover host-set channel and inbox keys: a key resolves the one
// record holding it in the unit's tenant, is unique among the tenant's
// channels or inboxes, and is released when its record changes key.
var keyCases = []storeCase{
	{"ChannelKeyResolves", func(t *testing.T, e *env) {
		ch := e.channelRecord(model.ChannelRef{Tenant: tenantA, ID: e.id(t)}, "r1")
		ch.Key = "support-mail"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, ch, "") })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.ResolveChannelKey(e.ctx, "support-mail")
			if err == nil && (got.Ref != ch.Ref || got.Key != ch.Key) {
				t.Errorf("ResolveChannelKey = %+v, want %s keyed support-mail", got, ch.Ref.ID)
			}
			return err
		})
		wantCode(t, e.viewErr(func(r store.Reader) error {
			_, err := r.ResolveChannelKey(e.ctx, "other")
			return err
		}), model.NotFound)
		wantCode(t, e.viewErr(func(r store.Reader) error {
			_, err := r.ResolveChannelKey(e.ctx, "")
			return err
		}), model.Validation)
	}},
	{"InboxKeyResolves", func(t *testing.T, e *env) {
		in := model.Inbox{Ref: model.InboxRef{Tenant: tenantA, ID: e.id(t)}, Channel: e.channel, Name: "sales", Key: "sales", Revision: "r1"}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutInbox(e.ctx, in, "") })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.ResolveInboxKey(e.ctx, "sales")
			if err == nil && (got.Ref != in.Ref || got.Key != in.Key) {
				t.Errorf("ResolveInboxKey = %+v, want %s keyed sales", got, in.Ref.ID)
			}
			return err
		})
		wantCode(t, e.viewErr(func(r store.Reader) error {
			_, err := r.ResolveInboxKey(e.ctx, "support")
			return err
		}), model.NotFound)
	}},
	{"ChannelKeyIsUniquePerTenant", func(t *testing.T, e *env) {
		first := e.channelRecord(model.ChannelRef{Tenant: tenantA, ID: e.id(t)}, "r1")
		first.Key = "shared"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, first, "") })
		second := e.channelRecord(model.ChannelRef{Tenant: tenantA, ID: e.id(t)}, "r1")
		second.Key = "shared"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutChannel(e.ctx, second, "") }), model.Conflict)
		// Keyless channels never collide.
		for range 2 {
			ch := e.channelRecord(model.ChannelRef{Tenant: tenantA, ID: e.id(t)}, "r1")
			e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, ch, "") })
		}
		// Another tenant may use the same key.
		other := e.channelRecord(model.ChannelRef{Tenant: tenantB, ID: e.id(t)}, "r1")
		other.Key = "shared"
		if err := e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error { return tx.PutChannel(e.ctx, other, "") }); err != nil {
			t.Fatalf("PutChannel in another tenant: %v", err)
		}
		e.mustView(t, func(r store.Reader) error {
			got, err := r.ResolveChannelKey(e.ctx, "shared")
			if err == nil && got.Ref != first.Ref {
				t.Errorf("ResolveChannelKey = %s, want %s", got.Ref.ID, first.Ref.ID)
			}
			return err
		})
		// Rekeying releases the old key.
		first.Key, first.Revision = "renamed", "r2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, first, "r1") })
		e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, second, "") })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.ResolveChannelKey(e.ctx, "shared")
			if err == nil && got.Ref != second.Ref {
				t.Errorf("ResolveChannelKey after rekey = %s, want %s", got.Ref.ID, second.Ref.ID)
			}
			return err
		})
	}},
	{"InboxKeyIsUniquePerTenant", func(t *testing.T, e *env) {
		other := e.channelRecord(model.ChannelRef{Tenant: tenantA, ID: e.id(t)}, "r1")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, other, "") })
		first := model.Inbox{Ref: model.InboxRef{Tenant: tenantA, ID: e.id(t)}, Channel: e.channel, Name: "a", Key: "front", Revision: "r1"}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutInbox(e.ctx, first, "") })
		// Unique across the tenant's channels, not only within one.
		second := model.Inbox{Ref: model.InboxRef{Tenant: tenantA, ID: e.id(t)}, Channel: other.Ref, Name: "b", Key: "front", Revision: "r1"}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutInbox(e.ctx, second, "") }), model.Conflict)
		// Rewriting the holder under its own key is no conflict.
		first.Name, first.Revision = "a2", "r2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutInbox(e.ctx, first, "r1") })
		second.Key = ""
		e.mustTx(t, func(tx store.Tx) error { return tx.PutInbox(e.ctx, second, "") })
	}},
}

// viewErr runs fn in tenantA and returns its error.
func (e *env) viewErr(fn func(store.Reader) error) error {
	return e.s.View(e.ctx, tenantA, fn)
}
