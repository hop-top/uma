package google

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// threadEnv is a mailbox with a two-message thread (t1: m1, m2) and a
// one-message thread (t2: m3), every message in the inbox and unread.
func threadEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, Config{})
	for _, m := range []struct{ id, thread string }{{"m1", "t1"}, {"m2", "t1"}, {"m3", "t2"}} {
		e.g.deliver(testMailbox, m.id, m.thread, rawMessage(testPeer, testMailbox, "<"+m.id+"@remote.test>", "hello", "text"))
	}
	return e
}

func items(ids ...string) []model.ExternalRef {
	var out []model.ExternalRef
	for _, id := range ids {
		out = append(out, model.ProviderMessageRef(testChan, id))
	}
	return out
}

func (e *env) labelsOf(id string) []string {
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	l := slices.Clone(e.box.messages[id].labels)
	slices.Sort(l)
	return l
}

func (e *env) has(id, label string) bool { return slices.Contains(e.labelsOf(id), label) }

func (e *env) reqs(prefix string) []string {
	var out []string
	for _, r := range e.g.requests() {
		if strings.HasPrefix(r, prefix) {
			out = append(out, r)
		}
	}
	return out
}

const mbox = "/gmail/v1/users/" + testMailbox

// stateCase is one verb request and the thread request it must become.
type stateCase struct {
	name string
	// call makes the request on items.
	call func(i *instance, items []model.ExternalRef) (adapter.Outcome, error)
	// want is the one thread request; body the label lists it carries.
	want        string
	add, remove []string
	// after checks the labels both messages of the thread end with.
	after func(t *testing.T, e *env, id string)
}

func stateCases() []stateCase {
	eff := effect(route(testPeer, "m1"), "00000000-0000-7000-8000-0000000000a1")
	archive := func(undo bool) func(*instance, []model.ExternalRef) (adapter.Outcome, error) {
		return func(i *instance, it []model.ExternalRef) (adapter.Outcome, error) {
			return i.Archive(context.Background(), adapter.ArchiveRequest{Effect: eff, Items: it, Undo: undo})
		}
	}
	read := func(v bool) func(*instance, []model.ExternalRef) (adapter.Outcome, error) {
		return func(i *instance, it []model.ExternalRef) (adapter.Outcome, error) {
			return i.SetReadState(context.Background(), adapter.ReadStateRequest{Effect: eff, Items: it, Read: v})
		}
	}
	flag := func(kind string, undo bool) func(*instance, []model.ExternalRef) (adapter.Outcome, error) {
		return func(i *instance, it []model.ExternalRef) (adapter.Outcome, error) {
			return i.Flag(context.Background(), adapter.FlagRequest{Effect: eff, Items: it, Kind: kind, Undo: undo})
		}
	}
	trash := func(undo bool) func(*instance, []model.ExternalRef) (adapter.Outcome, error) {
		return func(i *instance, it []model.ExternalRef) (adapter.Outcome, error) {
			return i.Trash(context.Background(), adapter.TrashRequest{Effect: eff, Items: it, Undo: undo})
		}
	}
	move := func(r adapter.MoveRequest) func(*instance, []model.ExternalRef) (adapter.Outcome, error) {
		return func(i *instance, it []model.ExternalRef) (adapter.Outcome, error) {
			r.Effect, r.Items = eff, it
			return i.Move(context.Background(), r)
		}
	}
	lacks := func(l string) func(*testing.T, *env, string) {
		return func(t *testing.T, e *env, id string) {
			if e.has(id, l) {
				t.Errorf("%s labels %v, want no %s", id, e.labelsOf(id), l)
			}
		}
	}
	holds := func(l string) func(*testing.T, *env, string) {
		return func(t *testing.T, e *env, id string) {
			if !e.has(id, l) {
				t.Errorf("%s labels %v, want %s", id, e.labelsOf(id), l)
			}
		}
	}
	return []stateCase{
		{"archive", archive(false), "modify", nil, []string{"INBOX"}, lacks("INBOX")},
		{"unarchive", archive(true), "modify", []string{"INBOX"}, nil, holds("INBOX")},
		{"read", read(true), "modify", nil, []string{"UNREAD"}, lacks("UNREAD")},
		{"unread", read(false), "modify", []string{"UNREAD"}, nil, holds("UNREAD")},
		{"flag star", flag(model.FlagStar, false), "modify", []string{"STARRED"}, nil, holds("STARRED")},
		{"unflag star", flag(model.FlagStar, true), "modify", nil, []string{"STARRED"}, lacks("STARRED")},
		{"flag important", flag(model.FlagImportant, false), "modify", []string{"IMPORTANT"}, nil, holds("IMPORTANT")},
		{"unflag important", flag(model.FlagImportant, true), "modify", nil, []string{"IMPORTANT"}, lacks("IMPORTANT")},
		{"trash", trash(false), "trash", nil, nil, holds("TRASH")},
		{"untrash", trash(true), "untrash", nil, nil, lacks("TRASH")},
		{"move add", move(adapter.MoveRequest{Add: []string{"Label_7"}}), "modify", []string{"Label_7"}, nil, holds("Label_7")},
		{"move remove", move(adapter.MoveRequest{Remove: []string{"UNREAD"}}), "modify", nil, []string{"UNREAD"}, lacks("UNREAD")},
		{"move add and remove", move(adapter.MoveRequest{Add: []string{"Label_7"}, Remove: []string{"INBOX"}}), "modify", []string{"Label_7"}, []string{"INBOX"}, holds("Label_7")},
		{"move to label id", move(adapter.MoveRequest{Destination: model.MoveDestination{ID: "Label_9"}}), "modify", []string{"Label_9"}, nil, holds("Label_9")},
		{"move to inbox", move(adapter.MoveRequest{Destination: model.MoveDestination{Role: model.RoleInbox}}), "modify", []string{"INBOX"}, nil, holds("INBOX")},
		{"move to archive", move(adapter.MoveRequest{Destination: model.MoveDestination{Role: model.RoleArchive}}), "modify", nil, []string{"INBOX"}, lacks("INBOX")},
		{"move to junk", move(adapter.MoveRequest{Destination: model.MoveDestination{Role: model.RoleJunk}}), "modify", []string{"SPAM"}, []string{"INBOX"}, holds("SPAM")},
		{"move to trash", move(adapter.MoveRequest{Destination: model.MoveDestination{Role: model.RoleTrash}}), "trash", nil, nil, holds("TRASH")},
	}
}

// Every verb is one request on the whole thread, never on a message: the
// thread of the item is read first (format=minimal), then the thread is
// changed, and every message of it shows the change.
func TestStateVerbsChangeTheWholeThread(t *testing.T) {
	for _, c := range stateCases() {
		t.Run(c.name, func(t *testing.T) {
			e := threadEnv(t)
			if c.name == "untrash" || c.name == "unflag star" || c.name == "unflag important" {
				e.g.relabelThread(testMailbox, "t1", []string{"TRASH", "STARRED", "IMPORTANT"}, nil)
			}
			before := len(e.g.requests())
			out, err := c.call(e.inst, items("m1"))
			if err != nil || out.State != model.ProviderAccepted || len(out.ReferenceChanges) != 0 {
				t.Fatalf("outcome = %+v, %v; want provider_accepted and no reference change: Gmail ids are stable", out, err)
			}
			got := e.g.requests()[before:]
			want := []string{
				"GET " + mbox + "/messages/m1?format=minimal",
				"POST " + mbox + "/threads/t1/" + c.want,
			}
			if !slices.Equal(got, want) {
				t.Fatalf("requests = %q, want %q", got, want)
			}
			if strings.Contains(strings.Join(got, "\n"), "/messages/m1/modify") {
				t.Fatalf("a message was modified: %q", got)
			}
			if c.want == "modify" {
				if n := len(e.box.modifies); n != 1 || !slices.Equal(e.box.modifies[0].AddLabelIDs, c.add) || !slices.Equal(e.box.modifies[0].RemoveLabelIDs, c.remove) {
					t.Fatalf("modify bodies = %+v, want add %v remove %v", e.box.modifies, c.add, c.remove)
				}
			}
			for _, id := range []string{"m1", "m2"} {
				c.after(t, e, id)
			}
			if !e.has("m3", "INBOX") || !e.has("m3", "UNREAD") || e.has("m3", "TRASH") || e.has("m3", "STARRED") {
				t.Errorf("m3, another thread, changed: %v", e.labelsOf("m3"))
			}
		})
	}
}

// A conversation's items in one thread are one request; in two threads one
// request each, in the order of the items.
func TestStateFansOutOncePerThread(t *testing.T) {
	e := threadEnv(t)
	out, err := e.inst.Archive(t.Context(), adapter.ArchiveRequest{Effect: effect(route(testPeer, "m1"), "a"), Items: items("m1", "m2", "m3")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Archive = %+v, %v", out, err)
	}
	if got, want := e.box.threadOps, []string{"modify t1", "modify t2"}; !slices.Equal(got, want) {
		t.Fatalf("thread requests = %q, want %q", got, want)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		if e.has(id, "INBOX") {
			t.Errorf("%s is still in the inbox", id)
		}
	}
}

func TestStateRefusals(t *testing.T) {
	eff := effect(route(testPeer, "m1"), "a")
	ctx := context.Background()
	for _, c := range []struct {
		name string
		call func(e *env) (adapter.Outcome, error)
		code model.ErrorCode
	}{
		{"no items and no parent", func(e *env) (adapter.Outcome, error) {
			return e.inst.Archive(ctx, adapter.ArchiveRequest{Effect: effect(route(testPeer, ""), "a")})
		}, model.Validation},
		{"item gone", func(e *env) (adapter.Outcome, error) {
			return e.inst.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: items("nope")})
		}, model.NotFound},
		{"flag kind", func(e *env) (adapter.Outcome, error) {
			return e.inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items("m1"), Kind: "pin"})
		}, model.Unsupported},
		{"unresolved role", func(e *env) (adapter.Outcome, error) {
			return e.inst.Move(ctx, adapter.MoveRequest{Effect: eff, Items: items("m1"), Destination: model.MoveDestination{Role: "vault"}})
		}, model.Unsupported},
		{"move names nothing", func(e *env) (adapter.Outcome, error) {
			return e.inst.Move(ctx, adapter.MoveRequest{Effect: eff, Items: items("m1")})
		}, model.Validation},
		{"too many labels", func(e *env) (adapter.Outcome, error) {
			var add []string
			for i := range 101 {
				add = append(add, "L"+strings.Repeat("x", i))
			}
			return e.inst.Move(ctx, adapter.MoveRequest{Effect: eff, Items: items("m1"), Add: add})
		}, model.Validation},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := threadEnv(t)
			out, err := c.call(e)
			if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != c.code {
				t.Fatalf("outcome = %+v, %v; want rejected with %s", out, err, c.code)
			}
			if len(e.box.threadOps) != 0 {
				t.Fatalf("thread requests %q, want none", e.box.threadOps)
			}
		})
	}
}

func TestStateRefusesAnotherChannelsRoute(t *testing.T) {
	e := threadEnv(t)
	r := route(testPeer, "m1")
	r.Channel = model.ChannelRef{Tenant: "t1", ID: "00000000-0000-7000-8000-0000000000c9"}
	_, err := e.inst.Archive(t.Context(), adapter.ArchiveRequest{Effect: effect(r, "a"), Items: items("m1")})
	_ = wantCode(t, err, model.Permanent)
	if len(e.box.threadOps) != 0 {
		t.Fatalf("thread requests %q, want none", e.box.threadOps)
	}
}

// What Gmail answers to the thread request decides the outcome.
func TestStateProviderAnswers(t *testing.T) {
	arch := func(e *env, it ...string) (adapter.Outcome, error) {
		return e.inst.Archive(t.Context(), adapter.ArchiveRequest{Effect: effect(route(testPeer, "m1"), "a"), Items: items(it...)})
	}
	answer := func(status int, reason string) func(w http.ResponseWriter, r *http.Request) bool {
		return func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "POST" && strings.Contains(r.URL.Path, "/threads/") {
				googleError(w, status, reason, "X")
				return true
			}
			return false
		}
	}
	t.Run("400 is a final rejection", func(t *testing.T) {
		e := threadEnv(t)
		e.g.override = answer(400, "invalidArgument")
		out, err := arch(e, "m1")
		if err != nil || out.State != model.Rejected || out.Failure.Code != model.Permanent {
			t.Fatalf("outcome = %+v, %v", out, err)
		}
	})
	t.Run("404 is not found", func(t *testing.T) {
		e := threadEnv(t)
		e.g.override = answer(404, "notFound")
		out, err := arch(e, "m1")
		if err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound {
			t.Fatalf("outcome = %+v, %v", out, err)
		}
	})
	t.Run("403 is unauthorized, never retried", func(t *testing.T) {
		e := threadEnv(t)
		e.g.override = answer(403, "insufficientPermissions")
		_, err := arch(e, "m1")
		if me := wantCode(t, err, model.Unauthorized); me.Retry != model.NoRetry {
			t.Fatalf("retry = %q", me.Retry)
		}
	})
	t.Run("429 is rate limited", func(t *testing.T) {
		e := threadEnv(t)
		e.g.override = answer(429, "rateLimitExceeded")
		_, err := arch(e, "m1")
		_ = wantCode(t, err, model.RateLimited)
	})
	t.Run("5xx is outcome unknown: reconcile reads the thread", func(t *testing.T) {
		e := threadEnv(t)
		e.g.override = answer(503, "backendError")
		_, err := arch(e, "m1")
		_ = wantCode(t, err, model.OutcomeUnknown)
	})
	t.Run("a lost answer is outcome unknown", func(t *testing.T) {
		e := threadEnv(t)
		e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "POST" && strings.Contains(r.URL.Path, "/threads/") {
				panic(http.ErrAbortHandler)
			}
			return false
		}
		_, err := arch(e, "m1")
		_ = wantCode(t, err, model.OutcomeUnknown)
	})
	t.Run("a failure after the first thread is outcome unknown", func(t *testing.T) {
		e := threadEnv(t)
		e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/threads/t2/modify") {
				googleError(w, 400, "invalidArgument", "X")
				return true
			}
			return false
		}
		_, err := arch(e, "m1", "m3")
		_ = wantCode(t, err, model.OutcomeUnknown)
		if e.has("m1", "INBOX") {
			t.Error("the first thread was not archived")
		}
	})
	t.Run("a call without credentials is unauthorized before any request", func(t *testing.T) {
		e := threadEnv(t)
		e.creds.err = model.NewError(model.Unauthorized, "no credential")
		before := len(e.g.requests())
		_, err := arch(e, "m1")
		_ = wantCode(t, err, model.Unauthorized)
		if n := len(e.g.requests()); n != before {
			t.Fatalf("%d requests without a token", n-before)
		}
	})
}

func reconcileReq(c model.Capability, mut func(*adapter.ReconcileRequest)) adapter.ReconcileRequest {
	r := adapter.ReconcileRequest{Effect: effect(route(testPeer, "m1"), "a"), Capability: c, Items: items("m1")}
	mut(&r)
	return r
}

// Reconcile reads the labels of every message of the thread: the change is
// applied when they all show it, and a conversation Gmail no longer holds is
// final.
func TestStateReconcileReadsThreadLabels(t *testing.T) {
	type tc struct {
		name  string
		req   adapter.ReconcileRequest
		setup func(e *env) // makes the thread hold the state
	}
	cases := []tc{
		{"archive", reconcileReq(model.CapabilityArchive, func(*adapter.ReconcileRequest) {}), func(e *env) { e.g.relabelThread(testMailbox, "t1", nil, []string{"INBOX"}) }},
		{"unarchive", reconcileReq(model.CapabilityArchive, func(r *adapter.ReconcileRequest) { r.Undo = true }), func(e *env) {}},
		{"read", reconcileReq(model.CapabilityReadState, func(r *adapter.ReconcileRequest) { r.Read = true }), func(e *env) { e.g.relabelThread(testMailbox, "t1", nil, []string{"UNREAD"}) }},
		{"unread", reconcileReq(model.CapabilityReadState, func(*adapter.ReconcileRequest) {}), func(e *env) {}},
		{"flag", reconcileReq(model.CapabilityFlag, func(r *adapter.ReconcileRequest) { r.FlagKind = model.FlagStar }), func(e *env) { e.g.relabelThread(testMailbox, "t1", []string{"STARRED"}, nil) }},
		{"unflag", reconcileReq(model.CapabilityFlag, func(r *adapter.ReconcileRequest) { r.FlagKind, r.Undo = model.FlagImportant, true }), func(e *env) {}},
		{"trash", reconcileReq(model.CapabilityTrash, func(*adapter.ReconcileRequest) {}), func(e *env) { e.g.relabelThread(testMailbox, "t1", []string{"TRASH"}, nil) }},
		{"untrash", reconcileReq(model.CapabilityTrash, func(r *adapter.ReconcileRequest) { r.Undo = true }), func(e *env) {}},
		{"move labels", reconcileReq(model.CapabilityMove, func(r *adapter.ReconcileRequest) {
			r.AddLabels, r.RemoveLabels = []string{"Label_7"}, []string{"UNREAD"}
		}), func(e *env) {
			e.g.relabelThread(testMailbox, "t1", []string{"Label_7"}, []string{"UNREAD"})
		}},
		{"move to role", reconcileReq(model.CapabilityMove, func(r *adapter.ReconcileRequest) { r.Destination = model.MoveDestination{Role: model.RoleJunk} }), func(e *env) {
			e.g.relabelThread(testMailbox, "t1", []string{"SPAM"}, []string{"INBOX"})
		}},
		{"move to trash", reconcileReq(model.CapabilityMove, func(r *adapter.ReconcileRequest) { r.Destination = model.MoveDestination{Role: model.RoleTrash} }), func(e *env) {
			e.g.relabelThread(testMailbox, "t1", []string{"TRASH"}, nil)
		}},
	}
	for _, c := range cases {
		t.Run(c.name+" applied", func(t *testing.T) {
			e := threadEnv(t)
			c.setup(e)
			out, err := e.inst.Reconcile(t.Context(), c.req)
			if err != nil || out.State != model.ProviderAccepted {
				t.Fatalf("Reconcile = %+v, %v; want applied", out, err)
			}
			if e.box.threadOps != nil {
				t.Fatalf("a reconcile changed the thread: %q", e.box.threadOps)
			}
		})
	}
	for _, c := range cases {
		if strings.HasPrefix(c.name, "un") {
			continue
		}
		t.Run(c.name+" not applied", func(t *testing.T) {
			e := threadEnv(t)
			out, err := e.inst.Reconcile(t.Context(), c.req)
			if err != nil || out.State != model.Failed || out.Failure == nil {
				t.Fatalf("Reconcile = %+v, %v; want failed: the thread does not show the change", out, err)
			}
		})
	}
	t.Run("one message short of the change is not applied", func(t *testing.T) {
		e := threadEnv(t)
		e.g.mu.Lock()
		m := e.box.messages["m1"]
		m.labels = slices.DeleteFunc(m.labels, func(l string) bool { return l == "INBOX" })
		e.g.mu.Unlock()
		out, err := e.inst.Reconcile(t.Context(), cases[0].req)
		if err != nil || out.State != model.Failed {
			t.Fatalf("Reconcile = %+v, %v; want failed: m2 is still in the inbox", out, err)
		}
	})
	t.Run("thread gone", func(t *testing.T) {
		e := threadEnv(t)
		req := cases[0].req
		req.Items = items("nope")
		out, err := e.inst.Reconcile(t.Context(), req)
		if err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound {
			t.Fatalf("Reconcile = %+v, %v; want rejected not_found", out, err)
		}
	})
}

func TestStateCapabilitiesReportLimits(t *testing.T) {
	e := newEnv(t, Config{})
	statuses, err := e.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := map[model.Capability]model.StateLimits{
		model.CapabilityArchive:   {Scope: model.ScopeAccount},
		model.CapabilityReadState: {Scope: model.ScopeAccount},
		model.CapabilityTrash:     {Scope: model.ScopeAccount},
		model.CapabilityFlag:      {Scope: model.ScopeAccount, Kinds: []string{model.FlagStar, model.FlagImportant}},
		model.CapabilityMove: {Scope: model.ScopeAccount, Shape: model.MoveShapeLabels,
			Roles: []string{model.RoleInbox, model.RoleArchive, model.RoleTrash, model.RoleJunk}},
	}
	seen := 0
	for _, s := range statuses {
		l, ok := want[s.Name]
		if !ok {
			continue
		}
		seen++
		got, err := model.ParseStateLimits(s.Limits)
		if err != nil || !s.Available {
			t.Fatalf("%s = %+v, %v; want available with readable limits", s.Name, s, err)
		}
		if got.Scope != l.Scope || got.Shape != l.Shape || !slices.Equal(got.Kinds, l.Kinds) || !slices.Equal(got.Roles, l.Roles) {
			t.Errorf("%s limits = %+v, want %+v", s.Name, got, l)
		}
	}
	if seen != len(want) {
		t.Fatalf("statuses %+v report %d of the %d state capabilities", statuses, seen, len(want))
	}
	d := NewFactory(Options{}).Describe()
	for c := range want {
		if !slices.Contains(d.Capabilities, c) {
			t.Errorf("descriptor omits %s", c)
		}
	}
}
