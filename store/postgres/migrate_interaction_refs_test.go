package postgres

import (
	"reflect"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// TestInteractionMessageRefsMigration stores interactions of every shape,
// migrates down past migration 25 and up again, and requires the provider
// message columns and the link flag backfilled from each body: the
// unlinked interactions on a provider message are found again, exactly,
// and an interaction naming no provider message has none.
func TestInteractionMessageRefsMigration(t *testing.T) {
	pool := newDatabase(t, true)
	ctx := t.Context()
	const tenant model.TenantID = "interaction-refs"
	ch := model.ChannelRef{Tenant: tenant, ID: benchID(0xb3, 1)}
	in := model.InboxRef{Tenant: tenant, ID: benchID(0xb3, 2)}
	conv := model.ConversationRef{Inbox: in, ID: benchID(0xb3, 3)}
	src := model.ProviderMessageRef(ch, "pm-1")
	threaded := src
	threaded.ParentID = "room-1"
	n := 10
	record := func(source *model.ExternalRef, linked bool) store.InteractionRecord {
		n++
		rec := store.InteractionRecord{
			Interaction: model.Interaction{
				Ref: model.InteractionRef{Conversation: conv, ID: benchID(0xb3, n)}, Kind: model.InteractionAction, Action: "approve",
				MessageExternal: source, RecordedAt: time.Unix(1_700_000_000, 0).UTC(), Revision: "i1",
			},
			Ingress: benchID(0xb4, n),
		}
		if linked {
			rec.Interaction.Message = &model.MessageRef{Conversation: conv, ID: benchID(0xb5, n)}
		}
		return rec
	}
	unlinked, linked, bare, other := record(&src, false), record(&src, true), record(nil, false), record(&threaded, false)
	st, err := Open(ctx, pool, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		return firstErr(
			tx.PutChannel(ctx, model.Channel{Ref: ch, Type: linkType, Configuration: model.Extension{Name: "c", Version: 1}, Revision: "ch1"}, ""),
			tx.PutInbox(ctx, model.Inbox{Ref: in, Channel: ch, Name: "refs", Revision: "in1"}, ""),
			tx.PutConversation(ctx, model.Conversation{Ref: conv, Mode: model.ModePerRoom, Status: model.ConversationOpen, CreatedAt: time.Now(), Revision: "cv1"}, ""),
			tx.PutInteraction(ctx, unlinked), tx.PutInteraction(ctx, linked), tx.PutInteraction(ctx, bare), tx.PutInteraction(ctx, other),
		)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	columns := func() int {
		var c int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'uma_interactions' AND column_name LIKE 'message\_%'`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if got := columns(); got != 7 {
		t.Fatalf("message columns after Migrate = %d, want 7", got)
	}
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 24)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := columns(); got != 0 {
		t.Fatalf("message columns after migrating down = %d, want none", got)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
	if st, err = Open(ctx, pool, Options{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ref  model.ExternalRef
		want []model.Interaction
	}{
		{src, []model.Interaction{unlinked.Interaction}},
		{threaded, []model.Interaction{other.Interaction}},
		{model.ProviderMessageRef(ch, "pm-2"), nil},
	} {
		if err := st.View(ctx, tenant, func(r store.Reader) error {
			p, err := r.UnlinkedInteractions(ctx, in, c.ref, model.PageRequest{Limit: 10})
			if err == nil && !reflect.DeepEqual(p.Items, c.want) {
				t.Errorf("UnlinkedInteractions(%+v) after the backfill = %+v, want %+v", c.ref, p.Items, c.want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	var withRef, isLinked int
	if err := pool.QueryRow(ctx, `SELECT count(message_ext_id), count(*) FILTER (WHERE message_linked) FROM uma_interactions`).Scan(&withRef, &isLinked); err != nil {
		t.Fatal(err)
	}
	if withRef != 3 || isLinked != 1 {
		t.Errorf("backfilled rows: %d with a provider message, %d linked; want 3 and 1", withRef, isLinked)
	}
}
