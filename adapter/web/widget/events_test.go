package widget_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

func TestEventsDeliverToTheirVisitorOnly(t *testing.T) {
	r := newRig(t, widget.Options{})
	alice := r.session("alice", widget.AssuranceVerified)
	bob := r.session("bob", widget.AssuranceVerified)
	// An anonymous visitor choosing a verified user's ID is someone else.
	impostor := r.session("alice", widget.AssuranceAnonymous)
	as, bs, is := r.connect(alice.Token, ""), r.connect(bob.Token, ""), r.connect(impostor.Token, "")
	out := r.send(route(chanA, alice), "for alice")
	if out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("outcome = %+v", out)
	}
	r.send(route(chanA, bob), "for bob")
	r.send(route(chanA, impostor), "for the anonymous one")
	ev := as.next(t)
	if ev.Name != widget.EventMessage || onlyText(ev.Data) != "for alice" || ev.ID != "1" {
		t.Fatalf("alice got %+v", ev)
	}
	if out.References[0] != widget.MessageRef(chanA, ev.Data.Message) {
		t.Fatalf("reference %+v does not name the delivered message %q", out.References[0], ev.Data.Message)
	}
	if ev := bs.next(t); onlyText(ev.Data) != "for bob" {
		t.Fatalf("bob's first event = %+v, want his own message", ev)
	}
	if ev := is.next(t); onlyText(ev.Data) != "for the anonymous one" {
		t.Fatalf("anonymous alice's first event = %+v, want her own message", ev)
	}
}

func TestEventsSecondTabAndReplayFromCursor(t *testing.T) {
	r := newRig(t, widget.Options{})
	alice := r.session("alice", widget.AssuranceVerified)
	tab1 := r.connect(alice.Token, "")
	for i := 1; i <= 3; i++ {
		r.send(route(chanA, alice), "m"+strconv.Itoa(i))
	}
	for i := 1; i <= 3; i++ {
		if ev := tab1.next(t); ev.ID != strconv.Itoa(i) {
			t.Fatalf("event %d id = %q", i, ev.ID)
		}
	}
	tab1.close()
	r.send(route(chanA, alice), "m4")
	tab2 := r.connect(alice.Token, "2")
	for _, want := range []string{"m3", "m4"} {
		if ev := tab2.next(t); onlyText(ev.Data) != want {
			t.Fatalf("replay after cursor 2 = %+v, want %s", ev, want)
		}
	}
	if _, code := r.dial(alice.Token, "x"); code != http.StatusBadRequest {
		t.Fatalf("malformed cursor = %d, want 400", code)
	}
}

func TestEventsResetWhenCursorFellOutOfTheLog(t *testing.T) {
	r := newRig(t, widget.Options{Log: widget.NewMemoryLog(2)})
	alice := r.session("alice", widget.AssuranceVerified)
	for i := 1; i <= 4; i++ {
		r.send(route(chanA, alice), "m"+strconv.Itoa(i))
	}
	s := r.connect(alice.Token, "1")
	if ev := s.next(t); ev.Name != widget.EventReset {
		t.Fatalf("first event = %+v, want reset", ev)
	}
	if ev := s.next(t); ev.ID != "3" || onlyText(ev.Data) != "m3" {
		t.Fatalf("after reset = %+v, want the oldest retained event", ev)
	}
}

func TestEventsSessionRouteReachesThatSessionOnly(t *testing.T) {
	r := newRig(t, widget.Options{})
	one := r.session("alice", widget.AssuranceVerified)
	two := r.session("alice", widget.AssuranceVerified)
	if one.Session == two.Session {
		t.Fatal("two sessions share an ID")
	}
	s1, s2 := r.connect(one.Token, ""), r.connect(two.Token, "")
	room := model.ExternalRef{Channel: chanA, Namespace: widget.NamespaceUser, Kind: model.RefKindRoom, ID: one.Session, ParentID: "alice"}
	r.send(room, "session one only")
	r.send(route(chanA, one), "every session")
	if ev := s1.next(t); onlyText(ev.Data) != "session one only" {
		t.Fatalf("session one got %+v", ev)
	}
	if ev := s2.next(t); onlyText(ev.Data) != "every session" || ev.ID != "2" {
		t.Fatalf("session two got %+v, want only the visitor-wide message", ev)
	}
}

func TestEventsRejectRoutesOutsideTheChannel(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	sender := r.inst.(adapter.Sender)
	for name, to := range map[string]model.ExternalRef{
		"other channel":   {Channel: chanB, Namespace: widget.NamespaceUser, Kind: widget.KindUser, ID: "alice"},
		"other namespace": {Channel: chanA, Namespace: "com.example.chat", Kind: widget.KindUser, ID: "alice"},
		"kind mismatch":   {Channel: chanA, Namespace: widget.NamespaceUser, Kind: widget.KindVisitor, ID: "alice"},
		"room no parent":  {Channel: chanA, Namespace: widget.NamespaceUser, Kind: model.RefKindRoom, ID: s.Session},
	} {
		_, err := sender.Send(t.Context(), adapter.SendRequest{
			Effect: adapter.Effect{Route: to, Audience: model.Audience{Visibility: model.Public}}, Content: text("x"),
		})
		if model.CodeOf(err) != model.Validation {
			t.Errorf("%s: Send = %v, want validation", name, err)
		}
	}
	_, err := sender.Send(t.Context(), adapter.SendRequest{
		Effect: adapter.Effect{Route: route(chanA, s), Audience: model.Audience{Visibility: model.Internal}}, Content: text("note"),
	})
	if model.CodeOf(err) != model.Validation {
		t.Fatalf("internal audience = %v, want validation: visitors never see internal content", err)
	}
}

func TestEventsEphemeralAndSignalExpire(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	to := route(chanA, s)
	recipient := model.ExternalRef{Channel: chanA, Namespace: widget.NamespaceUser, Kind: string(model.KindIdentity), ID: "alice"}
	inst := r.inst
	if _, err := inst.(adapter.EphemeralSender).SendEphemeral(t.Context(), adapter.EphemeralRequest{
		SendRequest: adapter.SendRequest{Effect: adapter.Effect{Route: to, Audience: model.Audience{Visibility: model.Restricted}}, Content: text("only you")},
		Recipient:   recipient, ExpiresAt: r.clock.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("SendEphemeral: %v", err)
	}
	if err := inst.(adapter.Signaler).Signal(t.Context(), adapter.SignalRequest{
		Route: to, Kind: "typing", Audience: model.Audience{Visibility: model.Public}, ExpiresAt: r.clock.Now().Add(5 * time.Second),
	}); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	live := r.connect(s.Token, "")
	if ev := live.next(t); ev.Name != widget.EventEphemeral || onlyText(ev.Data) != "only you" || ev.Expires == nil {
		t.Fatalf("first event = %+v, want the ephemeral reply", ev)
	}
	if ev := live.next(t); ev.Name != widget.EventSignal || ev.Data.Kind != "typing" {
		t.Fatalf("second event = %+v, want typing", ev)
	}
	live.close()
	r.clock.Advance(time.Minute)
	r.send(to, "later")
	replay := r.connect(s.Token, "")
	if ev := replay.next(t); ev.Name != widget.EventMessage || onlyText(ev.Data) != "later" {
		t.Fatalf("replay = %+v, want expired entries skipped", ev)
	}
	_, err := inst.(adapter.EphemeralSender).SendEphemeral(t.Context(), adapter.EphemeralRequest{
		SendRequest: adapter.SendRequest{Effect: adapter.Effect{Route: to}, Content: text("x")},
		Recipient:   model.ExternalRef{Channel: chanA, Namespace: widget.NamespaceUser, Kind: "identity", ID: "bob"},
		ExpiresAt:   r.clock.Now().Add(time.Minute),
	})
	if model.CodeOf(err) != model.Validation {
		t.Fatalf("ephemeral to someone other than the route's visitor = %v, want validation", err)
	}
}

func TestEventsNativeStream(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	st := r.inst.(adapter.Streamer)
	eff := adapter.Effect{Route: route(chanA, s), Audience: model.Audience{Visibility: model.Public}}
	start, err := st.Start(t.Context(), adapter.StreamRequest{Effect: eff, Mode: model.StreamNative})
	if err != nil || start.ProviderStreamRef == "" {
		t.Fatalf("Start = %+v, %v", start, err)
	}
	ref := start.ProviderStreamRef
	if _, err := st.Update(t.Context(), ref, adapter.StreamRequest{Effect: eff, Sequence: 1, Content: text("Hel")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Finish(t.Context(), ref, adapter.StreamRequest{Effect: eff, Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(t.Context(), "s-unknown", adapter.StreamRequest{Effect: eff, Sequence: 2}); model.CodeOf(err) != model.Validation {
		t.Fatalf("update of a foreign stream = %v, want validation", err)
	}
	live := r.connect(s.Token, "")
	for _, want := range []string{widget.EventStreamStart, widget.EventStreamUpdate, widget.EventStreamFinish} {
		ev := live.next(t)
		if ev.Name != want || ev.Data.Message != ref {
			t.Fatalf("event = %+v, want %s of %s", ev, want, ref)
		}
		if want == widget.EventStreamUpdate && (ev.Data.Sequence != 1 || onlyText(ev.Data) != "Hel") {
			t.Fatalf("update = %+v", ev)
		}
	}
}

func TestEventsEndOnCloseAndTokenExpiry(t *testing.T) {
	r := newRig(t, widget.Options{SessionTTL: time.Second})
	s := r.session("alice", widget.AssuranceVerified)
	r.connect(s.Token, "").ended(t)
	s = r.session("alice", widget.AssuranceVerified)
	live := r.connect(s.Token, "")
	r.f.Close()
	live.ended(t)
	if _, code := r.dial(s.Token, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("event stream after Close = %d, want 503", code)
	}
}

func TestEventsBeforeFirstOpen(t *testing.T) {
	f, err := widget.New(widget.Options{Verifier: headerVerifier})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.Events(chanA).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/widget/events", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("event stream before any Open = %d, want 503", rec.Code)
	}
}

func TestEventsRefusedKeyMakesNoDelivery(t *testing.T) {
	log := widget.NewMemoryLog(0)
	r := newRig(t, widget.Options{Log: log})
	s := r.session("alice", widget.AssuranceVerified)
	r.keys.revoke()
	_, err := r.inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{
		Effect: adapter.Effect{Route: route(chanA, s), Audience: model.Audience{Visibility: model.Public}}, Content: text("x"),
	})
	if model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("Send with a revoked key = %v, want unauthorized", err)
	}
	entries, err := log.Read(t.Context(), widget.LogKey{Channel: chanA, Namespace: widget.NamespaceUser, Visitor: "alice"}, 0, 10)
	if err != nil || len(entries) != 0 {
		t.Fatalf("log = %+v, %v; want nothing delivered", entries, err)
	}
}
