package widget_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

var inboxA = model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000c1"}

func service() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: "widget-service"}, Call: model.Call{Correlation: "widget"}}
}

// e2e is the widget channel served through a real engine: the receiver
// from Runtime.Webhook and the event stream over httptest.
type e2e struct {
	engine uma.Engine
	st     store.Store
	clock  *portstest.FixedClock
	f      *widget.Factory
	r      *rig
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	ctx := t.Context()
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	st := memory.New(memory.Options{Clock: clock})
	f, err := widget.New(widget.Options{Verifier: headerVerifier})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{f},
		Credentials: newKeyHost(),
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0xe2e),
	})
	if err != nil {
		t.Fatal(err)
	}
	lock := true
	err = st.Transact(ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(ctx, model.Channel{
			Ref: chanA, Type: widget.Type, Configuration: widget.Configuration("support"), CredentialRef: "widget-key", Revision: "r1",
		}, ""); err != nil {
			return err
		}
		return tx.PutInbox(ctx, model.Inbox{
			Ref: inboxA, Channel: chanA, Name: "support", Destinations: []string{"support"}, Revision: "r1",
			Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
		}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	hd, err := engine.Runtime().Webhook(ctx, service(), chanA)
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/widget/", hd)
	mux.Handle("GET /widget/events", f.Events(chanA))
	srv := httptest.NewServer(mux)
	r := &rig{t: t, ch: chanA, clock: clock, f: f, srv: srv}
	t.Cleanup(func() {
		f.Close()
		srv.Close()
		_ = engine.Close(context.Background())
	})
	return &e2e{engine: engine, st: st, clock: clock, f: f, r: r}
}

// TestEndToEnd runs the widget through the engine: a visitor message over
// Runtime.Webhook, an operator reply executed by Runtime.Run, a typing signal
// from the Interactions service and a native stream from the Streams
// service, all reaching the visitor over SSE, then a reconnect resuming
// from its cursor.
func TestEndToEnd(t *testing.T) {
	ctx := t.Context()
	env := newE2E(t)
	engine, st, clock, r := env.engine, env.st, env.clock, env.r
	alice := r.session("alice", widget.AssuranceVerified)
	bob := r.session("bob", widget.AssuranceVerified)
	if code := r.message(alice.Token, "c-1", "hello"); code != http.StatusAccepted {
		t.Fatalf("visitor message = %d", code)
	}
	client, err := engine.For(model.Principal{Tenant: tenant, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	var conv model.Conversation
	err = st.View(ctx, tenant, func(rd store.Reader) error {
		p, err := rd.Conversations(ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inboxA}}, Page: model.PageRequest{Limit: 10}})
		if err != nil || len(p.Items) != 1 {
			t.Fatalf("conversations = %+v, %v", p.Items, err)
		}
		conv = p.Items[0]
		m, err := rd.Messages(ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inboxA}}, Page: model.PageRequest{Limit: 10}})
		if err != nil || len(m.Items) != 1 || m.Items[0].Author.Assurance != widget.AssuranceVerified {
			t.Fatalf("messages = %+v, %v", m.Items, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := route(chanA, alice); conv.ReplyRoute != want {
		t.Fatalf("reply route = %+v, want the visitor %+v", conv.ReplyRoute, want)
	}

	live := r.connect(alice.Token, "")
	other := r.connect(bob.Token, "")
	worker := make(chan error, 1)
	go func() {
		worker <- engine.Runtime().Run(ctx, service(), uma.WorkerOptions{Owner: "widget-worker", Limit: 10, Poll: 5 * time.Millisecond})
	}()

	cmd := func(op model.OperationID) model.Command {
		return model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + op)}, Operation: op}
	}
	reply, err := client.Messages().Reply(ctx, model.Reply{Command: cmd("op-reply"), Conversation: conv.Ref, Content: text("how can I help?")})
	if err != nil {
		t.Fatal(err)
	}
	first := live.next(t)
	if first.Name != widget.EventMessage || onlyText(first.Data) != "how can I help?" {
		t.Fatalf("visitor got %+v, want the reply", first)
	}
	if err := client.Interactions().Signal(ctx, model.Signal{
		Call: model.Call{Correlation: "c-typing"}, Conversation: conv.Ref, Kind: "typing",
		Audience: model.Audience{Visibility: model.Public}, ExpiresAt: clock.Now().Add(5 * time.Second),
	}); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if ev := live.next(t); ev.Name != widget.EventSignal || ev.Data.Kind != "typing" {
		t.Fatalf("visitor got %+v, want typing", ev)
	}
	started, err := client.Streams().Start(ctx, model.StartStream{
		Command: cmd("op-start"), Conversation: conv.Ref, Audience: model.Audience{Visibility: model.Public}, Mode: model.StreamNative,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := *started.Operation.ResultStream
	if _, err := client.Streams().Update(ctx, model.StreamUpdate{Command: cmd("op-update"), Stream: ref, Sequence: 1, Content: text("Thinking")}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Streams().End(ctx, model.EndStream{Command: cmd("op-end"), Stream: ref, FinalSequence: 1}); err != nil {
		t.Fatal(err)
	}
	var streamed []string
	for range 3 {
		ev := live.next(t)
		streamed = append(streamed, ev.Name)
	}
	if streamed[0] != widget.EventStreamStart || streamed[1] != widget.EventStreamUpdate || streamed[2] != widget.EventStreamFinish {
		t.Fatalf("stream events = %v", streamed)
	}

	op, err := client.Operations().Get(ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: reply.Operation.Ref})
	if err != nil || op.State != model.ProviderAccepted || len(op.ProviderReferences) != 1 ||
		op.ProviderReferences[0] != widget.MessageRef(chanA, first.Data.Message) {
		t.Fatalf("reply operation = %+v, %v", op, err)
	}

	// Engine.Close does not wait on open event streams: they are not
	// engine work.
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := engine.Runtime().Drain(closeCtx, service()); err != nil {
		t.Fatal(err)
	}
	if err := <-worker; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := engine.Close(closeCtx); err != nil {
		t.Fatalf("Engine.Close with streams open: %v", err)
	}

	resumed := r.connect(alice.Token, first.ID)
	if ev := resumed.next(t); ev.Name != widget.EventSignal {
		t.Fatalf("resumed after %s with %+v, want the event after the cursor", first.ID, ev)
	}
	select {
	case ev := <-other.events:
		t.Fatalf("another visitor received %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestEndToEndInteraction runs a control through the engine: the operator
// replies with controls, the visitor presses a button and submits a form,
// the engine records both as interactions, and the operator's responses in
// each mode reach the visitor as interaction.response events that name the
// interaction by the ID the receiver returned.
func TestEndToEndInteraction(t *testing.T) {
	ctx := t.Context()
	env := newE2E(t)
	engine, r := env.engine, env.r
	alice := r.session("alice", widget.AssuranceVerified)
	if code := r.message(alice.Token, "c-1", "hello"); code != http.StatusAccepted {
		t.Fatalf("visitor message = %d", code)
	}
	client, err := engine.For(model.Principal{Tenant: tenant, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	var conv model.Conversation
	err = env.st.View(ctx, tenant, func(rd store.Reader) error {
		p, err := rd.Conversations(ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inboxA}}, Page: model.PageRequest{Limit: 10}})
		if err != nil || len(p.Items) != 1 {
			t.Fatalf("conversations = %+v, %v", p.Items, err)
		}
		conv = p.Items[0]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	live := r.connect(alice.Token, "")
	worker := make(chan error, 1)
	go func() {
		worker <- engine.Runtime().Run(ctx, service(), uma.WorkerOptions{Owner: "widget-worker", Limit: 10, Poll: 5 * time.Millisecond})
	}()
	cmd := func(op model.OperationID) model.Command {
		return model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + op)}, Operation: op}
	}

	controls := model.Content{Parts: []model.Part{
		{Kind: model.PartText, Text: "Approve?"},
		structured(widget.ControlsSchema, widget.ControlsVersion, `{"buttons":[{"action":"approve","label":"Approve"}],"form":{"schema":"org.example.feedback","submit":"Send","fields":[{"name":"email","label":"Email","type":"email"}]}}`),
	}}
	if _, err := client.Messages().Reply(ctx, model.Reply{Command: cmd("op-controls"), Conversation: conv.Ref, Content: controls}); err != nil {
		t.Fatal(err)
	}
	shown := live.next(t)
	if shown.Name != widget.EventMessage || len(shown.Data.Parts) != 2 || shown.Data.Parts[1].Structured == nil ||
		shown.Data.Parts[1].Structured.Name != widget.ControlsSchema {
		t.Fatalf("visitor got %+v, want the message with its controls", shown)
	}

	code, body := r.post("/widget/actions", alice.Token, widget.ActionRequest{ID: "press-1", Action: "approve", Message: shown.Data.Message})
	var pressed widget.AcceptedInteraction
	if code != http.StatusAccepted || json.Unmarshal([]byte(body), &pressed) != nil || pressed.Interaction == "" {
		t.Fatalf("action = %d %s", code, body)
	}
	if code, body := r.post("/widget/actions", alice.Token, widget.ActionRequest{ID: "press-1", Action: "approve", Message: shown.Data.Message}); code != http.StatusAccepted ||
		!strings.Contains(body, `"duplicate":true`) {
		t.Fatalf("retried action = %d %s, want a duplicate", code, body)
	}
	code, body = r.post("/widget/submissions", alice.Token, widget.SubmissionRequest{
		ID: "form-1", Message: shown.Data.Message, Fields: model.Extension{Name: "org.example.feedback", Version: 1, Data: json.RawMessage(`{"email":"a@b.c"}`)},
	})
	var submitted widget.AcceptedInteraction
	if code != http.StatusAccepted || json.Unmarshal([]byte(body), &submitted) != nil || submitted.Interaction == pressed.Interaction {
		t.Fatalf("submission = %d %s", code, body)
	}

	page, err := client.Interactions().List(ctx, model.ListRequest{Call: model.Call{Correlation: "list"}, Filter: model.Filter{Inboxes: []model.InboxRef{inboxA}}, Page: model.PageRequest{Limit: 10}})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("interactions = %+v, %v; want the action once and the submission", page.Items, err)
	}
	byID := map[string]model.Interaction{}
	for _, in := range page.Items {
		for _, ext := range in.External {
			byID[ext.ID] = in
		}
	}
	action, form := byID[pressed.Interaction], byID[submitted.Interaction]
	if action.Kind != model.InteractionAction || action.Action != "approve" || action.Ref.Conversation != conv.Ref ||
		action.Actor.External.ID != "alice" || action.Actor.External.Namespace != widget.NamespaceUser {
		t.Fatalf("action interaction = %+v, want the verified visitor's press in the conversation", action)
	}
	if form.Kind != model.InteractionSubmission || form.Fields.Name != "org.example.feedback" || form.Ref.Conversation != conv.Ref {
		t.Fatalf("submission interaction = %+v", form)
	}

	pub, restricted := model.Audience{Visibility: model.Public}, model.Audience{Visibility: model.Restricted}
	window := env.clock.Now().Add(time.Minute)
	respond := func(op model.OperationID, in model.Interaction, mode string, aud model.Audience, follow bool, say string) {
		t.Helper()
		_, err := client.Interactions().Respond(ctx, model.Respond{
			Command: cmd(op), Interaction: in.Ref, Mode: mode, Audience: aud, Content: text(say), FollowUp: follow, ExpiresAt: window,
		})
		if err != nil {
			t.Fatalf("Respond %s: %v", op, err)
		}
	}
	respond("op-initial", action, model.ResponseModeUpdate, pub, false, "Approved")
	respond("op-follow", action, model.ResponseModeEphemeral, restricted, true, "Only you")
	respond("op-thanks", form, model.ResponseModeMessage, pub, false, "Thanks")

	got := map[string]sseEvent{}
	for range 3 {
		ev := live.next(t)
		if ev.Name != widget.EventInteractionResponse {
			t.Fatalf("visitor got %+v, want an interaction response", ev)
		}
		got[onlyText(ev.Data)] = ev
	}
	if ev := got["Approved"]; ev.Data.Mode != model.ResponseModeUpdate || ev.Data.Interaction != pressed.Interaction || ev.Data.Message != shown.Data.Message || ev.Data.FollowUp {
		t.Errorf("update = %+v, want the control's message replaced, answering the press", ev)
	}
	if ev := got["Only you"]; ev.Data.Mode != model.ResponseModeEphemeral || ev.Data.Interaction != pressed.Interaction || !ev.Data.FollowUp || ev.Expires == nil {
		t.Errorf("follow-up = %+v, want an expiring ephemeral follow-up to the press", ev)
	}
	if ev := got["Thanks"]; ev.Data.Mode != model.ResponseModeMessage || ev.Data.Interaction != submitted.Interaction || ev.Data.Message == "" {
		t.Errorf("message = %+v, want a message answering the submission", ev)
	}

	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := engine.Runtime().Drain(closeCtx, service()); err != nil {
		t.Fatal(err)
	}
	if err := <-worker; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
