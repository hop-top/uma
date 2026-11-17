package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

const hookURL = "https://hooks.example.test/graph"

func round(t *testing.T, e *env, sink *testSink) error {
	t.Helper()
	_, err := e.inst.round(t.Context(), sink)
	return err
}

func onlySub(t *testing.T, e *env) gSubscription {
	t.Helper()
	subs := e.g.subscriptions()
	if len(subs) != 1 {
		t.Fatalf("subscriptions = %+v, want one", subs)
	}
	return subs[0]
}

func TestReceiverSubscribesInbox(t *testing.T) {
	e := newEnv(t, Config{NotificationURL: hookURL})
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	s := onlySub(t, e)
	want := gSubscription{
		ID: s.ID, ChangeType: "created", NotificationURL: hookURL, LifecycleNotificationURL: hookURL,
		Resource: "users/" + testMailbox + "/mailFolders('inbox')/messages", ClientState: clientStateOf(t, e),
		ExpirationDateTime: e.clock.Now().Add(defaultSubscriptionLifetime),
	}
	if !s.ExpirationDateTime.Equal(want.ExpirationDateTime) {
		t.Fatalf("expiration %v, want %v", s.ExpirationDateTime, want.ExpirationDateTime)
	}
	s.ExpirationDateTime = want.ExpirationDateTime
	if s != want {
		t.Fatalf("subscription %+v, want %+v", s, want)
	}
}

func TestReceiverRenewsBeforeExpiry(t *testing.T) {
	e := newEnv(t, Config{NotificationURL: hookURL})
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	id := onlySub(t, e).ID
	margin := renewMargin(defaultSubscriptionLifetime)
	e.clock.Advance(defaultSubscriptionLifetime - margin - time.Minute)
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if n := e.g.count("PATCH /v1.0/subscriptions/"); n != 0 {
		t.Fatalf("renewed %d times outside the renewal margin", n)
	}
	e.clock.Advance(2 * time.Minute)
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	s := onlySub(t, e)
	if e.g.count("PATCH /v1.0/subscriptions/"+id) != 1 || s.ID != id {
		t.Fatalf("requests %q, want one renewal of %s", e.g.requests(), id)
	}
	if want := e.clock.Now().Add(defaultSubscriptionLifetime); !s.ExpirationDateTime.Equal(want) {
		t.Fatalf("renewed to %v, want %v", s.ExpirationDateTime, want)
	}
}

func TestReceiverRecreatesExpiredOrRemovedSubscription(t *testing.T) {
	e := newEnv(t, Config{NotificationURL: hookURL})
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	first := onlySub(t, e).ID
	e.inst.state.notify(lifecycleRecreate)
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if s := onlySub(t, e); s.ID == first {
		t.Fatal("subscription not recreated after subscriptionRemoved")
	}
	e.inst.state.notify(lifecycleRenew)
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if e.g.count("PATCH /v1.0/subscriptions/") != 1 {
		t.Fatalf("reauthorizationRequired did not renew: %q", e.g.requests())
	}
}

func TestReceiverReplacesStaleDuplicateSubscription(t *testing.T) {
	e := newEnv(t, Config{NotificationURL: hookURL})
	e.g.subs["stale"] = &gSubscription{ID: "stale", ChangeType: "created", NotificationURL: hookURL,
		Resource: "/users/" + testMailbox + "/mailFolders('inbox')/messages", ClientState: "old"}
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if s := onlySub(t, e); s.ID == "stale" || s.ClientState != clientStateOf(t, e) {
		t.Fatalf("subscription %+v, want a fresh one with this channel's client state", s)
	}
}

func TestDeltaHoldsLinkUntilEveryMessageAccepted(t *testing.T) {
	e := newEnv(t, Config{})
	for _, id := range []string{"m1", "m2", "m3"} {
		e.inbound(id, "peer@example.test", "body "+id)
	}
	refusing := &testSink{refuse: func(in model.Ingress) error {
		if in.Envelope.MessageID == "m3" {
			return model.NewError(model.Unavailable, "store down")
		}
		return nil
	}}
	wantCode(t, round(t, e, refusing), model.Unavailable)
	st, _ := e.inst.loadState(t.Context())
	if !strings.Contains(st.DeltaLink, "$skiptoken=2") {
		t.Fatalf("delta link %q, want the page after m1 and m2 only", st.DeltaLink)
	}
	sink := &testSink{}
	if err := round(t, e, sink); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.ids(), []string{"m3"}) {
		t.Fatalf("accepted %v, want the refused m3 again", sink.ids())
	}
	e.inbound("m4", "peer@example.test", "later")
	sink = &testSink{}
	if err := round(t, e, sink); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.ids(), []string{"m4"}) || e.g.count("GET /v1.0/users/"+testMailbox+"/mailFolders/inbox/messages/delta?$deltatoken=3") != 1 {
		t.Fatalf("accepted %v; requests %q", sink.ids(), e.g.requests())
	}
}

func TestDeltaFirstRoundIsBounded(t *testing.T) {
	e := newEnv(t, Config{InitialSyncWindowMS: int64(2 * time.Hour / time.Millisecond)})
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	want := "GET /v1.0/users/" + testMailbox + "/mailFolders/inbox/messages/delta?$filter=receivedDateTime ge 2026-10-11T10:00:00Z&$select=id&changeType=created"
	if e.g.count(want) != 1 {
		t.Fatalf("requests %q, want %q", e.g.requests(), want)
	}
}

func TestDeltaRestartsOnExpiredToken(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", "peer@example.test", "hi")
	_ = e.inst.saveState(t.Context(), receiverState{DeltaLink: e.g.endpoint() + "/users/" + testMailbox + "/mailFolders/inbox/messages/delta?$deltatoken=expired"})
	sink := &testSink{}
	if err := round(t, e, sink); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.ids(), []string{"m1"}) {
		t.Fatalf("accepted %v after a 410, want a new round", sink.ids())
	}
}

func TestDeltaNeverFollowsAForeignLink(t *testing.T) {
	e := newEnv(t, Config{})
	_ = e.inst.saveState(t.Context(), receiverState{DeltaLink: "https://graph.attacker.test/v1.0/delta?$deltatoken=1"})
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if e.g.count("GET /v1.0/users/"+testMailbox+"/mailFolders/inbox/messages/delta?$filter") != 1 {
		t.Fatalf("a stored foreign link was not replaced by a new round: %q", e.g.requests())
	}
	if e.inst.c.sameOrigin("https://graph.attacker.test/v1.0/x") || !e.inst.c.sameOrigin(e.g.endpoint()+"/x") {
		t.Fatal("origin check")
	}
}

func TestDeltaStateInProtocolStore(t *testing.T) {
	p := &memProtocol{}
	e := newEnv(t, Config{}, withProtocol(p))
	e.inbound("m1", "peer@example.test", "hi")
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	ext := p.ext[protocolKey]
	var st receiverState
	if ext.Name != stateSchema || json.Unmarshal(ext.Data, &st) != nil || !strings.Contains(st.DeltaLink, "$deltatoken=1") {
		t.Fatalf("protocol state %+v", ext)
	}
	if e.inst.state.mem.DeltaLink != "" {
		t.Fatal("state also kept in process memory")
	}
}

func TestRunWakesOnNotification(t *testing.T) {
	e := newEnv(t, Config{PollIntervalMS: int64(time.Hour / time.Millisecond)})
	sink := &testSink{}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- e.inst.Run(ctx, sink) }()
	waitFor(t, func() bool { return e.g.count("GET /v1.0/users/"+testMailbox+"/mailFolders/inbox/messages/delta") >= 1 })
	e.inbound("m1", "peer@example.test", "hi")
	e.inst.state.notify(0)
	waitFor(t, func() bool { return len(sink.ids()) == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the context's cancellation", err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
