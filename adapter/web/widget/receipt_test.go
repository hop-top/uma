package widget_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func sendReceipt(r *rig, eff adapter.Effect, kind string) (adapter.Outcome, error) {
	return r.inst.(adapter.ReceiptSender).SendReceipt(r.t.Context(), adapter.ReceiptRequest{Effect: eff, Kind: kind})
}

func TestDescribeDeclaresReceipt(t *testing.T) {
	f, _ := widget.New(widget.Options{Verifier: headerVerifier})
	if !slices.Contains(f.Describe().Capabilities, model.CapabilityReceipt) {
		t.Errorf("capabilities = %v, want %s", f.Describe().Capabilities, model.CapabilityReceipt)
	}
	r := newRig(t, widget.Options{})
	statuses, err := r.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(statuses, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityReceipt && s.Available }) {
		t.Errorf("statuses = %+v, want receipt available", statuses)
	}
}

func TestReceiptAppendsReceiptEvent(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	live := r.connect(s.Token, "")
	for i, kind := range []string{model.ReceiptDelivered, model.ReceiptRead} {
		out, err := sendReceipt(r, publicEffect(r, s, visitorMessage), kind)
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 0 {
			t.Fatalf("SendReceipt %s = %+v, %v; want accepted with no reference: a receipt adds none", kind, out, err)
		}
		ev := live.next(t)
		if ev.Name != widget.EventReceipt || ev.Data.Type != widget.EventReceipt || ev.Data.Message != visitorMessage ||
			ev.Data.Kind != kind || len(ev.Data.Parts) != 0 || ev.ID != string(rune('1'+i)) {
			t.Fatalf("event = %+v, want %s of %s with kind %s", ev, widget.EventReceipt, visitorMessage, kind)
		}
	}
}

func TestReceiptKindsTheWidgetCannotExpressAreRefused(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	for _, kind := range []string{"", "typing", "seen", "READ"} {
		out, err := sendReceipt(r, publicEffect(r, s, visitorMessage), kind)
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Unsupported || out.Failure.Retry != model.NoRetry {
			t.Errorf("SendReceipt %q = %+v, %v; want a final unsupported rejection", kind, out, err)
		}
	}
	live := r.connect(s.Token, "")
	select {
	case ev := <-live.events:
		t.Fatalf("event %+v delivered for a refused receipt", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestReceiptTargetsAVisitorMessageOfThisChannel(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	sent := r.send(route(chanA, s), "x").References[0].ID
	good := widget.MessageRef(chanA, visitorMessage)
	bad := map[string]*model.ExternalRef{
		"none":                 nil,
		"widget message":       ptr(widget.MessageRef(chanA, sent)),
		"other channel":        ptr(model.ProviderMessageRef(chanB, visitorMessage)),
		"other namespace":      ptr(model.ExternalRef{Channel: chanA, Namespace: "com.example.chat", Kind: model.RefKindMessage, ID: visitorMessage}),
		"other kind":           ptr(model.ExternalRef{Channel: chanA, Namespace: good.Namespace, Kind: model.RefKindRoom, ID: visitorMessage}),
		"has parent":           ptr(model.ExternalRef{Channel: chanA, Namespace: good.Namespace, Kind: model.RefKindMessage, ID: visitorMessage, ParentID: "p"}),
		"malformed id":         ptr(model.ProviderMessageRef(chanA, "not-a-message")),
		"visitor prefix short": ptr(model.ProviderMessageRef(chanA, "v00")),
		"empty id":             ptr(model.ProviderMessageRef(chanA, "")),
	}
	for name, target := range bad {
		eff := publicEffect(r, s, visitorMessage)
		eff.Target = target
		if _, err := sendReceipt(r, eff, model.ReceiptRead); model.CodeOf(err) != model.Validation {
			t.Errorf("SendReceipt with target %s = %v, want validation", name, err)
		}
	}
	eff := publicEffect(r, s, visitorMessage)
	eff.Audience = model.Audience{Visibility: model.Internal}
	if _, err := sendReceipt(r, eff, model.ReceiptRead); model.CodeOf(err) != model.Validation {
		t.Errorf("SendReceipt for a non-public audience = %v, want validation", err)
	}
	live := r.connect(s.Token, "")
	live.next(t) // the widget's own message
	select {
	case ev := <-live.events:
		t.Fatalf("event %+v delivered for a refused receipt", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// A receipt reaches the visitor of its route only, and for a session route
// only that session.
func TestReceiptReachesOnlyItsVisitorAndSession(t *testing.T) {
	r := newRig(t, widget.Options{})
	one := r.session("alice", widget.AssuranceVerified)
	two := r.session("alice", widget.AssuranceVerified)
	bob := r.session("bob", widget.AssuranceVerified)
	if one.Session == two.Session {
		t.Fatal("sessions must differ")
	}
	liveOne, liveTwo, liveBob := r.connect(one.Token, ""), r.connect(two.Token, ""), r.connect(bob.Token, "")

	eff := publicEffect(r, one, visitorMessage)
	eff.Route = model.ExternalRef{Channel: chanA, Namespace: widget.NamespaceUser, Kind: model.RefKindRoom, ID: one.Session, ParentID: "alice"}
	if out, err := sendReceipt(r, eff, model.ReceiptRead); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("SendReceipt to a session = %+v, %v", out, err)
	}
	if ev := liveOne.next(t); ev.Name != widget.EventReceipt {
		t.Fatalf("session got %+v, want the receipt", ev)
	}
	// A visitor-wide receipt follows, so the quiet streams are known to
	// have read past the session one.
	if out, err := sendReceipt(r, publicEffect(r, one, visitorMessage), model.ReceiptDelivered); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("SendReceipt to the visitor = %+v, %v", out, err)
	}
	if ev := liveTwo.next(t); ev.Name != widget.EventReceipt || ev.Data.Kind != model.ReceiptDelivered {
		t.Fatalf("other session got %+v, want only the visitor-wide receipt", ev)
	}
	if ev := liveOne.next(t); ev.Data.Kind != model.ReceiptDelivered {
		t.Fatalf("session got %+v, want the visitor-wide receipt", ev)
	}
	select {
	case ev := <-liveBob.events:
		t.Fatalf("another visitor received %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestReceiptEventReplaysAfterReconnect(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	if _, err := sendReceipt(r, publicEffect(r, s, visitorMessage), model.ReceiptRead); err != nil {
		t.Fatal(err)
	}
	if ev := r.connect(s.Token, "").next(t); ev.Name != widget.EventReceipt || ev.Data.Message != visitorMessage || ev.ID != "1" {
		t.Fatalf("event = %+v", ev)
	}
	r.connect(s.Token, "1") // resumed past the receipt: nothing replayed
	again := r.connect(s.Token, "")
	again.next(t)
	select {
	case ev := <-again.events:
		t.Fatalf("event %+v: a receipt is one entry", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestReceiptEventWireFormat(t *testing.T) {
	for want, ev := range map[string]widget.Event{
		`{"type":"receipt","message":"v1","kind":"delivered"}`: {Type: widget.EventReceipt, Message: "v1", Kind: model.ReceiptDelivered},
		`{"type":"receipt","message":"v1","kind":"read"}`:      {Type: widget.EventReceipt, Message: "v1", Kind: model.ReceiptRead},
	} {
		got, err := json.Marshal(ev)
		if err != nil || string(got) != want {
			t.Errorf("wire = %s, %v; want %s", got, err, want)
		}
	}
}

func TestReceiptRefusalKeepsItsCode(t *testing.T) {
	for _, code := range []model.ErrorCode{model.Unauthorized, model.NotFound, model.Unsupported} {
		r := newRig(t, widget.Options{Log: refusingLog{MemoryLog: widget.NewMemoryLog(0), err: model.NewError(code, "refused")}})
		s := r.session("alice", widget.AssuranceVerified)
		out, err := sendReceipt(r, publicEffect(r, s, visitorMessage), model.ReceiptRead)
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != code || out.Failure.Retry != model.NoRetry {
			t.Errorf("SendReceipt refused with %s = %+v, %v; want a final rejection with that code", code, out, err)
		}
	}
}

func TestReceiptLostAnswerIsUnknown(t *testing.T) {
	r := newRig(t, widget.Options{Log: refusingLog{MemoryLog: widget.NewMemoryLog(0), err: context.DeadlineExceeded}})
	s := r.session("alice", widget.AssuranceVerified)
	out, err := sendReceipt(r, publicEffect(r, s, visitorMessage), model.ReceiptRead)
	if err != nil || out.State != model.Unknown || out.Failure == nil || out.Failure.Code != model.OutcomeUnknown {
		t.Fatalf("SendReceipt with a lost answer = %+v, %v; want outcome_unknown", out, err)
	}
}

func TestReceiptNeedsKeyAndOpenInstance(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	r.keys.revoke()
	if _, err := sendReceipt(r, publicEffect(r, s, visitorMessage), model.ReceiptRead); model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("SendReceipt with a revoked key = %v, want unauthorized", err)
	}
}

// TestEndToEndReceipt runs a host receipt through the engine: it reaches
// the visitor of the message's conversation over SSE once even when the
// host retries the submission, and no other visitor sees it.
func TestEndToEndReceipt(t *testing.T) {
	ctx := t.Context()
	env := newE2E(t)
	engine, r := env.engine, env.r
	alice := r.session("alice", widget.AssuranceVerified)
	bob := r.session("bob", widget.AssuranceVerified)
	if code := r.message(alice.Token, "c-1", "hello"); code != http.StatusAccepted {
		t.Fatalf("visitor message = %d", code)
	}
	client, err := engine.For(model.Principal{Tenant: tenant, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	var msg model.Message
	err = env.st.View(ctx, tenant, func(rd store.Reader) error {
		p, err := rd.Messages(ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inboxA}}, Page: model.PageRequest{Limit: 10}})
		if err != nil || len(p.Items) != 1 {
			t.Fatalf("messages = %+v, %v", p.Items, err)
		}
		msg = p.Items[0]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	live, other := r.connect(alice.Token, ""), r.connect(bob.Token, "")
	worker := make(chan error, 1)
	go func() {
		worker <- engine.Runtime().Run(ctx, service(), uma.WorkerOptions{Owner: "widget-worker", Limit: 10, Poll: 5 * time.Millisecond})
	}()
	receipt := func(op model.OperationID, kind string) (model.Acceptance, error) {
		return client.Messages().Receipt(ctx, model.Receipt{
			Command: model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + op)}, Operation: op},
			Message: msg.Ref, Kind: kind,
		})
	}
	first, err := receipt("op-delivered", model.ReceiptDelivered)
	if err != nil {
		t.Fatal(err)
	}
	ev := live.next(t)
	if ev.Name != widget.EventReceipt || ev.Data.Kind != model.ReceiptDelivered || ev.Data.Message == "" ||
		widget.MessageRef(chanA, ev.Data.Message) != msg.External[0] {
		t.Fatalf("visitor got %+v, want the delivered receipt of the visitor's message %+v", ev, msg.External)
	}
	again, err := receipt("op-delivered", model.ReceiptDelivered)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("retried submission = %+v, %v; want the same operation replayed", again, err)
	}
	if _, err := receipt("op-read", model.ReceiptRead); err != nil {
		t.Fatal(err)
	}
	if ev := live.next(t); ev.Name != widget.EventReceipt || ev.Data.Kind != model.ReceiptRead {
		t.Fatalf("visitor got %+v, want the read receipt next: the retry sent nothing", ev)
	}
	select {
	case ev := <-other.events:
		t.Fatalf("another visitor received %+v", ev)
	case <-time.After(50 * time.Millisecond):
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
