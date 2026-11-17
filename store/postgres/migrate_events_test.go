package postgres

import (
	"encoding/json"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

// migrateBelow migrates pool to the schema before version.
func migrateBelow(t *testing.T, version int) fstest.MapFS {
	t.Helper()
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0]); err == nil && n < version {
			b, err := fs.ReadFile(migrations(), e.Name())
			if err != nil {
				t.Fatal(err)
			}
			before[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	return before
}

// TestEventFilterColumnsBackfill stores events of every shape under schema
// 21, migrates, and requires the conversation column to equal what
// store.EventConversation resolves from each stored body, the RecordedAt
// columns to hold each body's instant exactly, and the running maximum to
// follow the log per tenant into the head row; appends after the upgrade
// continue that maximum. Then it migrates down and up again.
func TestEventFilterColumnsBackfill(t *testing.T) {
	pool := newDatabase(t, false)
	ctx := t.Context()
	if err := migrate(ctx, pool, migrateBelow(t, 22)); err != nil {
		t.Fatalf("migrate to schema 21: %v", err)
	}
	const tenant, other, empty model.TenantID = "backfill", "backfill-other", "backfill-empty"
	n := 0
	id := func() model.ID {
		n++
		return model.ID("00000000-0000-7000-8000-" + strings.Repeat("0", 12-len(strconv.Itoa(n))) + strconv.Itoa(n))
	}
	in := model.InboxRef{Tenant: tenant, ID: id()}
	far := model.InboxRef{Tenant: tenant, ID: id()}
	cA, cO := model.ConversationRef{Inbox: in, ID: id()}, model.ConversationRef{Inbox: far, ID: id()}
	mA, mO := model.MessageRef{Conversation: cA, ID: id()}, model.MessageRef{Conversation: cO, ID: id()}
	task := model.TaskRef{Conversation: cA, ID: id()}
	work := model.WorkItemRef{Conversation: cA, ID: id()}
	ix := model.InteractionRef{Conversation: cA, ID: id()}
	op := model.OperationRef{Tenant: tenant, ID: id()}
	contact := model.ContactRef{Tenant: tenant, ID: id()}
	east := time.FixedZone("east", 2*60*60)
	base := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	payload := func(schema string, data any) model.Extension {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		return model.Extension{Name: schema, Version: model.PayloadVersion1, Data: raw}
	}
	shapes := []struct {
		res      model.Ref
		kind     string
		payload  model.Extension
		recorded time.Time
	}{
		{cA.Ref(), "com.example.pinned", model.Extension{}, base.Add(5 * time.Second)},
		{mA.Ref(), model.EventMessageReceived, payload(model.SchemaMessageReceived, model.MessageReceivedPayload{Message: mA}),
			base.Add(time.Second + 123456789).In(east)},
		{cA.Ref(), model.EventConversationLinked, payload(model.SchemaConversationLinked, model.ConversationLinkPayload{Conversation: cA}),
			base.Add(9 * time.Second)},
		{task.Ref(), model.EventTaskUpdated, payload(model.SchemaTaskUpdated, model.TaskUpdatedPayload{Task: task}), base.Add(3 * time.Second)},
		{work.Ref(), model.EventWorkItemUpdated, payload(model.SchemaWorkItemUpdated, model.WorkItemUpdatedPayload{WorkItem: work}),
			base.Add(9*time.Second + 1)},
		{ix.Ref(), model.EventInteractionUpdated, payload(model.SchemaInteractionUpdated, model.InteractionUpdatedPayload{Interaction: ix}),
			time.Time{}},
		{mA.Ref(), model.EventMessageReceived, payload(model.SchemaMessageReceived, model.MessageReceivedPayload{Message: mO}), base},
		{mO.Ref(), model.EventMessageCreated, payload(model.SchemaMessageCreated, model.MessageCreatedPayload{Message: mO}), base},
		{mA.Ref(), "com.example.noted", payload("com.example.noted", model.MessageReceivedPayload{Message: mA}), base},
		{task.Ref(), model.EventTaskUpdated, payload(model.SchemaTaskUpdated, map[string]any{"task": "x"}), base},
		{op.Ref(), model.EventDeliveryUpdated, payload(model.SchemaDeliveryUpdated, model.DeliveryUpdatedPayload{Operation: op}), base},
		{contact.Ref(), model.EventContactCreated, payload(model.SchemaContactCreated, model.ContactCreatedPayload{Contact: contact}), base},
		{in.Ref(), "com.example.inbox", model.Extension{}, base.Add(2 * time.Second)},
	}
	v2 := payload(model.SchemaMessageReceived, model.MessageReceivedPayload{Message: mA})
	v2.Version = 2
	shapes = append(shapes, struct {
		res      model.Ref
		kind     string
		payload  model.Extension
		recorded time.Time
	}{mA.Ref(), model.EventMessageReceived, v2, base})
	insert := func(tn model.TenantID, pos int, ev model.Event) {
		body, err := encode(&ev)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO uma_events (tenant_id, position, id, kind, inbox_id, body, written_at)
			VALUES ($1, $2, $3, $4, $5, $6, now())`, string(tn), pos, string(ev.ID), ev.Kind, optID(ev.Inbox), body); err != nil {
			t.Fatalf("insert event %d: %v", pos, err)
		}
	}
	for i, s := range shapes {
		insert(tenant, i+1, model.Event{
			Version: 1, ID: id(), Kind: s.kind, Tenant: tenant, Inbox: s.res.Inbox, Resource: s.res, Actor: "backfill",
			RecordedAt: s.recorded, Payload: s.payload,
		})
	}
	insert(other, 1, model.Event{Version: 1, ID: id(), Kind: "com.example.pinned", Tenant: other,
		Resource: model.ContactRef{Tenant: other, ID: id()}.Ref(), Actor: "backfill", RecordedAt: base.Add(-time.Hour)})
	for _, q := range []string{
		`INSERT INTO uma_event_heads (tenant_id, position) VALUES ('backfill', ` + strconv.Itoa(len(shapes)) + `), ('backfill-other', 1), ('backfill-empty', 4)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT tenant_id, body, coalesce(conversation_id::text, ''), recorded_unix, recorded_nanos,
		recorded_max_unix, recorded_max_nanos FROM uma_events ORDER BY tenant_id, position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	maxOf := map[string]time.Time{}
	seen, withConv := 0, 0
	for rows.Next() {
		var tn, conv string
		var body []byte
		var ru, mu int64
		var rn, mn int32
		if err := rows.Scan(&tn, &body, &conv, &ru, &rn, &mu, &mn); err != nil {
			t.Fatal(err)
		}
		ev, err := decode[model.Event](body)
		if err != nil {
			t.Fatal(err)
		}
		if want := store.EventConversation(ev); conv != string(want) {
			t.Errorf("event %s (%s) backfilled with conversation %q, store.EventConversation resolves %q", ev.ID, ev.Kind, conv, want)
		}
		if conv != "" {
			withConv++
		}
		if wu, wn := instant(ev.RecordedAt); ru != wu || rn != wn {
			t.Errorf("event %s recorded at (%d, %d), body says %s = (%d, %d)", ev.ID, ru, rn, ev.RecordedAt, wu, wn)
		}
		if ev.RecordedAt.After(maxOf[tn]) {
			maxOf[tn] = ev.RecordedAt
		}
		if wu, wn := instant(maxOf[tn]); mu != wu || mn != wn {
			t.Errorf("event %s running maximum (%d, %d), want %s", ev.ID, mu, mn, maxOf[tn])
		}
		seen++
	}
	rows.Close()
	if seen != len(shapes)+1 || withConv != 7 {
		t.Fatalf("read %d events, %d with a conversation; want %d and 7", seen, withConv, len(shapes)+1)
	}
	head := func(tn model.TenantID) time.Time {
		var u int64
		var n int32
		if err := pool.QueryRow(ctx, `SELECT recorded_max_unix, recorded_max_nanos FROM uma_event_heads WHERE tenant_id = $1`,
			string(tn)).Scan(&u, &n); err != nil {
			t.Fatal(err)
		}
		return time.Unix(u, int64(n))
	}
	for tn, want := range map[model.TenantID]time.Time{tenant: maxOf[string(tenant)], other: maxOf[string(other)], empty: {}} {
		if got := head(tn); !got.Equal(want) {
			t.Errorf("head maximum of %s = %s, want %s", tn, got, want)
		}
	}

	// An append after the upgrade continues the tenant's maximum, and Since
	// reads across backfilled and new rows.
	st, err := Open(ctx, pool, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	late := model.Event{Version: 1, ID: id(), Kind: "com.example.pinned", Tenant: tenant, Inbox: in.ID, Resource: cA.Ref(),
		Actor: "backfill", RecordedAt: base.Add(4 * time.Second), Audience: model.Audience{Visibility: model.Internal}}
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{late}) }); err != nil {
		t.Fatal(err)
	}
	var mu int64
	var mn int32
	if err := pool.QueryRow(ctx, `SELECT recorded_max_unix, recorded_max_nanos FROM uma_events WHERE tenant_id = $1 AND id = $2`,
		string(tenant), string(late.ID)).Scan(&mu, &mn); err != nil {
		t.Fatal(err)
	}
	if got := time.Unix(mu, int64(mn)); !got.Equal(maxOf[string(tenant)]) {
		t.Errorf("appended event's running maximum = %s, want the backfilled %s", got, maxOf[string(tenant)])
	}
	since := base.Add(4 * time.Second)
	if err := st.View(ctx, tenant, func(r store.Reader) error {
		p, err := r.Events(ctx, model.Filter{Since: &since, Conversation: &cA}, "", 100)
		if err != nil {
			return err
		}
		var kinds []string
		for _, ev := range p.Items {
			kinds = append(kinds, ev.Kind)
		}
		if want := []string{"com.example.pinned", model.EventConversationLinked, model.EventWorkItemUpdated, "com.example.pinned"}; strings.Join(kinds, ",") != strings.Join(want, ",") {
			t.Errorf("since and conversation across the upgrade = %v, want %v", kinds, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 21)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
}

// TestEventFiltersUseTheirIndexes holds a large log and requires a
// conversation read to range over uma_events_conversation and the Since
// start to read one entry of uma_events_recorded_max, never a scan of the
// table or a sort; and the indexes to go with their migration.
func TestEventFiltersUseTheirIndexes(t *testing.T) {
	pool := newDatabase(t, true)
	ctx := t.Context()
	const tenant = "events-plan"
	uuid := func(prefix, expr string) string {
		return "('" + prefix + "' || lpad(to_hex(" + expr + "), 12, '0'))::uuid"
	}
	// 20000 events over 40 inboxes of 25 conversations; one recorded per
	// second, so the running maximum is the event's own time.
	seed := `INSERT INTO uma_events (tenant_id, position, id, kind, inbox_id, body, written_at, conversation_id,
			recorded_unix, recorded_nanos, recorded_max_unix, recorded_max_nanos)
		SELECT '` + tenant + `', i, ` + uuid("00000000-0000-7000-8000-", "i") + `, 'message.received',
			` + uuid("00000000-0000-7000-8001-", "i % 40") + `, '{}', now(), ` + uuid("00000000-0000-7000-8002-", "i % 1000") + `,
			1700000000 + i, 0, 1700000000 + i, 0
		FROM generate_series(1, 20000) AS i`
	for _, q := range []string{seed, `INSERT INTO uma_event_heads (tenant_id, position) VALUES ('` + tenant + `', 20000)`, `ANALYZE uma_events`} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	x := &explainer{DBTX: pool}
	q := db.New(x)
	inbox := "00000000-0000-7000-8001-000000000007"
	conv := "00000000-0000-7000-8002-000000000007"
	since := int64(1700015000)
	t.Run("conversation", func(t *testing.T) {
		rows, err := q.ListConversationEvents(ctx, db.ListConversationEventsParams{
			TenantID: tenant, InboxID: &inbox, ConversationID: &conv, After: 0, Head: 20000, Kinds: []string{}, Lim: 11,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 11 || rows[0].Position != 7 || rows[1].Position != 1007 {
			t.Errorf("rows = %d starting %v, want 11 from position 7 every 1000", len(rows), rows)
		}
		requirePlan(t, x.plan.String(), "uma_events_conversation")
	})
	t.Run("conversation and since", func(t *testing.T) {
		nanos := int32(0)
		_, err := q.ListConversationEvents(ctx, db.ListConversationEventsParams{
			TenantID: tenant, InboxID: &inbox, ConversationID: &conv, After: 0, Head: 20000, Kinds: []string{},
			SinceUnix: &since, SinceNanos: &nanos, Lim: 11,
		})
		if err != nil {
			t.Fatal(err)
		}
		requirePlan(t, x.plan.String(), "uma_events_conversation")
	})
	t.Run("since start", func(t *testing.T) {
		got, err := q.EventSinceStart(ctx, db.EventSinceStartParams{TenantID: tenant, SinceUnix: since, SinceNanos: 0})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != 15000 {
			t.Errorf("since start = %v, want position 15000", got)
		}
		requirePlan(t, x.plan.String(), "uma_events_recorded_max")
	})

	indexes := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname IN ('uma_events_conversation', 'uma_events_recorded_max')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := indexes(); got != 2 {
		t.Fatalf("event filter indexes after Migrate = %d, want 2", got)
	}
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 21)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := indexes(); got != 0 {
		t.Errorf("event filter indexes after migrating down = %d, want 0", got)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
}

func requirePlan(t *testing.T, plan, index string) {
	t.Helper()
	if !strings.Contains(plan, index) || strings.Contains(plan, "Seq Scan") || strings.Contains(plan, "Sort") {
		t.Errorf("plan does not range over %s:\n%s", index, plan)
	}
}
