package postgres

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// Commands contending in one tenant through the engine. Every label change
// rewrites the conversation row: concurrent units writing one row at
// SERIALIZABLE lose to the first to commit and are rerun by the engine.
// Replies write new rows only, and insert them without looking their keys
// up first, so concurrent replies leave one another no predicate lock to
// conflict with. At the concurrency below no caller sees model.Transient.
// Each test logs the server's serialization failures by statement, with
// the reason the server gave, and the latency of each call, reruns
// included. The statement is where the server noticed the unit had to
// fail, which is not always the read or write that made it fail.

const contentionTenant model.TenantID = "postgres-contention-tenant"

// contentionCallers and contentionCalls are the concurrency the tests hold
// to: callers in step, each issuing its calls back to back.
const (
	contentionCallers = 4
	contentionCalls   = 5
)

// abortTracer counts statements the server failed with a serialization
// failure, by statement name, server message and reason code.
type abortTracer struct {
	mu     sync.Mutex
	aborts map[string]int
}

type traceSQL struct{}

func (a *abortTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceSQL{}, d.SQL)
}

func (a *abortTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	var pe *pgconn.PgError
	if !errors.As(d.Err, &pe) || pe.Code != "40001" {
		return
	}
	sql, _ := ctx.Value(traceSQL{}).(string)
	name, _, _ := strings.Cut(sql, "\n")
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.aborts == nil {
		a.aborts = map[string]int{}
	}
	a.aborts[strings.TrimSpace(name)+": "+pe.Message+" ("+pe.Detail+")"]++
}

func (a *abortTracer) log(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, k := range slices.Sorted(maps.Keys(a.aborts)) {
		t.Logf("serialization failures x%d: %s", a.aborts[k], k)
	}
}

// total is the number of serialization failures counted.
func (a *abortTracer) total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, v := range a.aborts {
		n += v
	}
	return n
}

// unitCounter counts units and the units that failed with model.Transient.
type unitCounter struct {
	store.Store
	units, lost atomic.Int64
}

func (c *unitCounter) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	c.units.Add(1)
	err := c.Store.Transact(ctx, tenant, fn)
	if model.IsCode(err, model.Transient) {
		c.lost.Add(1)
	}
	return err
}

// contentionRig is an engine on a traced store with a send-capable channel,
// an inbox and conversations routed to it.
type contentionRig struct {
	tr    *abortTracer
	uc    *unitCounter
	e     uma.Engine
	convs []model.ConversationRef
}

func newContentionRig(t *testing.T, conversations int) *contentionRig {
	t.Helper()
	return contentionRigOn(t, newDatabase(t, true).Config(), conversations, 2*contentionCallers)
}

// contentionRigOn is a contention rig on the database cfg connects to, with
// a pool of conns connections.
func contentionRigOn(t testing.TB, base *pgxpool.Config, conversations int, conns int32) *contentionRig {
	t.Helper()
	ctx := t.Context()
	tr := &abortTracer{}
	cfg := base.Copy()
	cfg.ConnConfig.Tracer = tr
	cfg.MaxConns = conns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st, err := Open(ctx, pool, Options{})
	if err != nil {
		t.Fatal(err)
	}
	uc := &unitCounter{Store: st}
	e, err := uma.New(uma.Config{
		Store: uc, Authorizer: benchAllow(), Adapters: []adapter.Factory{&benchSender{sent: make(chan time.Time, 1)}},
		Clock: benchClock{}, IDs: portstest.NewSequenceIDs(0xc1),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	ch := model.ChannelRef{Tenant: contentionTenant, ID: benchID(0xc, 1)}
	in := model.InboxRef{Tenant: contentionTenant, ID: benchID(0xc, 2)}
	r := &contentionRig{tr: tr, uc: uc, e: e}
	if err := st.Transact(ctx, contentionTenant, func(tx store.Tx) error {
		if err := firstErr(
			tx.PutChannel(ctx, model.Channel{
				Ref: ch, Type: benchSendType, Configuration: model.Extension{Name: string(benchSendType) + ".config", Version: 1}, Revision: "ch1",
			}, ""),
			tx.PutInbox(ctx, model.Inbox{Ref: in, Channel: ch, Name: "contention", Revision: "in1"}, ""),
		); err != nil {
			return err
		}
		for i := range conversations {
			conv := model.ConversationRef{Inbox: in, ID: benchID(0xd, i)}
			r.convs = append(r.convs, conv)
			if err := tx.PutConversation(ctx, model.Conversation{
				Ref: conv, Mode: model.ModePerRoom, Status: model.ConversationOpen,
				ReplyRoute: model.ExternalRef{Channel: ch, Namespace: string(benchSendType), Kind: "room", ID: fmt.Sprintf("room-%d", i)},
				CreatedAt:  time.Now(), Revision: "cv1",
			}, ""); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return r
}

func (r *contentionRig) client(t testing.TB, actor string) uma.Client {
	t.Helper()
	cl, err := r.e.For(model.Principal{Tenant: contentionTenant, Actor: model.ActorID(actor)})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// race runs contentionCallers callers in step, each making contentionCalls
// calls back to back, and fails on any call that does not succeed: a
// model.Transient reaching a caller included.
func (r *contentionRig) race(t *testing.T, call func(caller, i int) error) {
	t.Helper()
	start := make(chan struct{})
	lat := make([]time.Duration, contentionCallers*contentionCalls)
	var wg sync.WaitGroup
	for c := range contentionCallers {
		wg.Go(func() {
			<-start
			for i := range contentionCalls {
				began := time.Now()
				err := call(c, i)
				lat[c*contentionCalls+i] = time.Since(began)
				if err != nil {
					t.Errorf("caller %d call %d: %v", c, i, err)
				}
			}
		})
	}
	began := time.Now()
	close(start)
	wg.Wait()
	wall := time.Since(began)
	r.tr.log(t)
	t.Logf("%d calls: %d units, %d lost to concurrent units; latency p50 %v p95 %v p99 %v; wall %v (%.0f calls/s)",
		len(lat), r.uc.units.Load(), r.uc.lost.Load(), percentile(lat, 0.5), percentile(lat, 0.95), percentile(lat, 0.99),
		wall, float64(len(lat))/wall.Seconds())
}

func contentionCommand(op string) model.Command {
	return model.Command{Call: model.Call{Correlation: model.CorrelationID(op)}, Operation: model.OperationID(op)}
}

func (r *contentionRig) replies(t *testing.T, conv func(caller int) model.ConversationRef) {
	t.Helper()
	clients := make([]uma.Client, contentionCallers)
	for i := range clients {
		clients[i] = r.client(t, fmt.Sprintf("agent-%d", i))
	}
	r.race(t, func(c, i int) error {
		_, err := clients[c].Messages().Reply(t.Context(), model.Reply{
			Command:      contentionCommand(fmt.Sprintf("reply-%d-%d", c, i)),
			Conversation: conv(c),
			Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "reply"}}},
		})
		return err
	})
	// A reply writes new rows only, inserted without a lookup of their
	// keys: concurrent replies leave one another nothing to conflict with.
	if n := r.tr.total(); n != 0 || r.uc.lost.Load() != 0 {
		t.Errorf("%d serialization failures, %d units lost among concurrent replies; want none", n, r.uc.lost.Load())
	}
}

// TestConcurrentRepliesToOneConversation: callers replying to one
// conversation at once all have their replies accepted, none of their
// units lost to another.
func TestConcurrentRepliesToOneConversation(t *testing.T) {
	r := newContentionRig(t, 1)
	r.replies(t, func(int) model.ConversationRef { return r.convs[0] })
}

// TestConcurrentRepliesAcrossConversations: callers replying to
// different conversations of one tenant at once all have their replies
// accepted, none of their units lost to another.
func TestConcurrentRepliesAcrossConversations(t *testing.T) {
	r := newContentionRig(t, contentionCallers)
	r.replies(t, func(c int) model.ConversationRef { return r.convs[c] })
}

// TestConcurrentLabelChangesOfOneConversation: callers adding and
// removing labels of one conversation at once all commit, and the
// conversation ends holding exactly the labels last added.
func TestConcurrentLabelChangesOfOneConversation(t *testing.T) {
	r := newContentionRig(t, 1)
	ctx := t.Context()
	admin := r.client(t, "admin")
	labels := make([]model.LabelRef, contentionCallers)
	for i := range labels {
		acc, err := admin.Labels().Configure(ctx, model.ConfigureLabel{
			Command: contentionCommand(fmt.Sprintf("label-%d", i)),
			Label:   model.Label{Ref: model.LabelRef{Tenant: contentionTenant}, Name: fmt.Sprintf("l%d", i)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if labels[i], err = acc.Operation.Result.AsLabel(); err != nil {
			t.Fatal(err)
		}
	}
	r.race(t, func(c, i int) error {
		req := model.ConversationLabels{
			Command:      contentionCommand(fmt.Sprintf("labels-%d-%d", c, i)),
			Conversation: r.convs[0], Labels: []model.LabelRef{labels[c]},
		}
		var err error
		if i%2 == 0 {
			_, err = admin.Conversations().AddLabels(ctx, req)
		} else {
			_, err = admin.Conversations().RemoveLabels(ctx, req)
		}
		return err
	})
	conv, err := admin.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "get"}, Ref: r.convs[0]})
	if err != nil {
		t.Fatal(err)
	}
	// contentionCalls is odd: each caller's last call adds its label.
	if !slices.Equal(conv.Labels, labels) {
		t.Fatalf("labels = %v, want %v", conv.Labels, labels)
	}
}
