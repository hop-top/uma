package slack

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// The view effects against the documented shapes of fakeviews_test.go.

const (
	testTrigger = "398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c"
	// secretText is a value no message, issue or error may repeat.
	secretText = "SECRET-VIEW-TEXT"
)

func plain(s string) map[string]any { return map[string]any{"type": "plain_text", "text": s} }

// modal is a valid modal view, adjusted by mod; a nil value deletes a field.
func modal(mod ...func(map[string]any)) map[string]any {
	v := map[string]any{
		"type": "modal", "callback_id": "feedback", "title": plain("Feedback"), "submit": plain("Send"), "close": plain("Cancel"),
		"private_metadata": "ctx-1",
		"blocks": []any{map[string]any{
			"type": "input", "block_id": "note", "label": plain(secretText),
			"element": map[string]any{"type": "plain_text_input", "action_id": "text"},
		}},
	}
	for _, m := range mod {
		m(v)
	}
	return v
}

func ext(name string, data map[string]any) model.Extension {
	b, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return model.Extension{Name: name, Version: ExtensionVersion, Data: b}
}

// openAction is a views.open effect whose trigger_id lives until two
// seconds after testNow.
func openAction(mod ...func(map[string]any)) model.Extension {
	d := map[string]any{"trigger_id": testTrigger, "expires_at": testNow.Add(2 * time.Second).Format(time.RFC3339Nano), "view": modal()}
	for _, m := range mod {
		m(d)
	}
	return ext(ExtensionViewsOpen, d)
}

func pushAction(mod ...func(map[string]any)) model.Extension {
	a := openAction(mod...)
	a.Name = ExtensionViewsPush
	return a
}

func updateAction(mod ...func(map[string]any)) model.Extension {
	d := map[string]any{"view_id": "V1001", "view": modal(func(v map[string]any) { v["title"] = plain("Updated") })}
	for _, m := range mod {
		m(d)
	}
	return ext(ExtensionViewsUpdate, d)
}

func (r *rig) execute(t *testing.T, a model.Extension) (adapter.Outcome, error) {
	t.Helper()
	return r.x.Execute(t.Context(), adapter.ExecuteRequest{Effect: r.effect(r.x.roomRef(testRoom)), Action: a})
}

// issue registers a trigger_id Slack sent a second before testNow.
func (r *rig) issue(from string) { r.acct.trigger(testTrigger, testNow.Add(-time.Second), from) }

func TestDescriptorDeclaresTheViewEffects(t *testing.T) {
	f := NewFactory(Options{})
	if !hasCapability(f.Describe().Capabilities, model.CapabilityExtensionExecute) {
		t.Fatal("extension_execute is not declared")
	}
	defs := f.Extensions()
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
		if d.Kind != model.ExtensionKindEffect || d.Version != ExtensionVersion || d.RequiredCapability != model.CapabilityExtensionExecute || d.AuthorizeAs != model.ActionExtensionExecute {
			t.Errorf("definition %+v, want an effect of version 1 authorized as extension.execute", d)
		}
	}
	for _, n := range []string{"slack.chat.views.open", "slack.chat.views.update", "slack.chat.views.push"} {
		if !names[n] {
			t.Errorf("effects %v, want %s", names, n)
		}
	}
	r := newRig(t)
	st, _ := r.x.Capabilities(t.Context())
	for _, s := range st {
		if s.Name == model.CapabilityExtensionExecute && !s.Available {
			t.Errorf("extension_execute reported unavailable: %+v", s)
		}
	}
}

func hasCapability(caps []model.Capability, c model.Capability) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

func TestOpenSendsTheTriggerAndTheViewOnce(t *testing.T) {
	r := newRig(t)
	r.issue("")
	a := openAction(func(d map[string]any) { d["conformance_unknown"] = "kept-out" })
	out, err := r.execute(t, a)
	wantAccepted(t, out, err)
	calls := r.callsOf("views.open")
	if len(calls) != 1 || !calls[0].Applied {
		t.Fatalf("calls = %+v, want one applied views.open", calls)
	}
	if calls[0].Auth != "Bearer "+testToken {
		t.Errorf("authorization = %q, want the bot token", calls[0].Auth)
	}
	args := calls[0].args(t)
	if args["trigger_id"] != testTrigger {
		t.Errorf("trigger_id = %v, want the action's", args["trigger_id"])
	}
	if _, ok := args["view"].(map[string]any); !ok {
		t.Errorf("view = %T, want the modal as a JSON object", args["view"])
	}
	for _, k := range []string{"expires_at", "conformance_unknown", "view_id", "hash"} {
		if _, ok := args[k]; ok {
			t.Errorf("arguments carry %s: %v", k, args)
		}
	}
	if len(out.References) != 1 || out.References[0] != (model.ExternalRef{Channel: testChannel, Namespace: Namespace, Kind: RefKindView, ID: "V1001"}) {
		t.Errorf("references = %+v, want the opened view", out.References)
	}
	if v := r.acct.view("V1001"); v == nil || v.view["callback_id"] != "feedback" {
		t.Errorf("fake holds %+v, want the modal opened", v)
	}
}

func TestOpenOnAChannelTarget(t *testing.T) {
	// A channel or inbox target routes to the channel itself.
	r := newRig(t)
	r.issue("")
	out, err := r.x.Execute(t.Context(), adapter.ExecuteRequest{Effect: r.effect(model.ExternalRef{Channel: testChannel}), Action: openAction()})
	wantAccepted(t, out, err)
}

func TestExpiredTriggerIsRefusedWithoutACall(t *testing.T) {
	for _, name := range []string{"open", "push"} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.issue("")
			mk := openAction
			if name == "push" {
				mk = pushAction
			}
			r.x.now = func() time.Time { return testNow.Add(2 * time.Second) } // exactly expires_at
			out, err := r.execute(t, mk())
			wantRejected(t, out, err, model.DeadlineExpired)
			if n := len(r.f.calls("acct1")); n != 0 {
				t.Fatalf("%d calls reached Slack, want none: the trigger_id is spent", n)
			}
			// A nanosecond earlier it still lives: the call is made.
			r.x.now = func() time.Time { return testNow.Add(2*time.Second - time.Nanosecond) }
			if name == "open" {
				out, err = r.execute(t, mk())
				wantAccepted(t, out, err)
			} else {
				r.acct.trigger(testTrigger, testNow.Add(-time.Second), "V9") // from no modal: Slack refuses, after a call
				_, _ = r.execute(t, mk())
				if n := len(r.callsOf("views.push")); n != 1 {
					t.Errorf("%d push calls, want the one made while the trigger_id lived", n)
				}
			}
		})
	}
}

func TestExpiresAtBeyondALifetimeIsRefused(t *testing.T) {
	r := newRig(t)
	r.issue("")
	edge := testNow.Add(TriggerLifetime + triggerSkew)
	out, err := r.execute(t, openAction(func(d map[string]any) { d["expires_at"] = edge.Add(time.Nanosecond).Format(time.RFC3339Nano) }))
	wantRejected(t, out, err, model.Validation)
	if n := len(r.f.calls("acct1")); n != 0 {
		t.Fatalf("%d calls, want none for an expires_at no trigger_id lives to", n)
	}
	out, err = r.execute(t, openAction(func(d map[string]any) { d["expires_at"] = edge.Format(time.RFC3339Nano) }))
	wantAccepted(t, out, err)
}

func TestSlackRefusalsAreFinalRejections(t *testing.T) {
	cases := []struct {
		code string
		want model.ErrorCode
	}{
		{"expired_trigger_id", model.DeadlineExpired}, {"exchanged_trigger_id", model.Conflict}, {"invalid_trigger_id", model.Validation},
		{"view_too_large", model.Validation}, {"duplicate_external_id", model.Conflict}, {"push_limit_reached", model.Conflict},
		{"invalid_arguments", model.Validation}, {"invalid_blocks", model.Validation},
		{"missing_scope", model.Unauthorized}, {"something_new", model.Permanent},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			r := newRig(t)
			r.issue("")
			r.f.respond("acct1", failWith(c.code))
			out, err := r.execute(t, openAction())
			wantRejected(t, out, err, c.want)
			if strings.Contains(out.Failure.SafeMessage, testTrigger) || strings.Contains(out.Failure.SafeMessage, secretText) {
				t.Errorf("message %q repeats a value", out.Failure.SafeMessage)
			}
			if n := len(r.callsOf("views.open")); n != 1 {
				t.Errorf("%d calls, want exactly the one attempt", n)
			}
		})
	}
}

func TestSlackExpiredTriggerThroughTheFake(t *testing.T) {
	// Slack's own clock says the trigger_id is gone though expires_at says
	// it lives: the refusal is Slack's, final.
	r := newRig(t)
	r.acct.trigger(testTrigger, testNow.Add(-10*time.Second), "")
	out, err := r.execute(t, openAction())
	wantRejected(t, out, err, model.DeadlineExpired)
}

func TestTriggerIsUsedOnce(t *testing.T) {
	r := newRig(t)
	r.issue("")
	out, err := r.execute(t, openAction())
	wantAccepted(t, out, err)
	out, err = r.execute(t, openAction(func(d map[string]any) { d["view"] = modal(func(v map[string]any) { v["callback_id"] = "again" }) }))
	wantRejected(t, out, err, model.Conflict)
}

func TestLostAnswerIsOutcomeUnknownAndNeverReconciled(t *testing.T) {
	r := newRig(t)
	r.issue("")
	r.f.respond("acct1", answerLost)
	out, err := r.execute(t, openAction())
	me := errorOf(t, out, err, model.OutcomeUnknown)
	if me.Retry == model.RetrySameOperation {
		t.Errorf("retry = %v, want an unknown outcome the engine reconciles first", me.Retry)
	}
	if n := len(r.callsOf("views.open")); n != 1 {
		t.Fatalf("%d calls, want one: a spent trigger_id is never sent again", n)
	}
	_, err = r.x.Reconcile(t.Context(), adapter.ReconcileRequest{
		Effect: r.effect(r.x.roomRef(testRoom)), Capability: model.CapabilityExtensionExecute, Action: openAction(),
	})
	if re, ok := errors.AsType[*model.Error](err); !ok || re.Code != model.Unsupported || re.Retry != model.NoRetry {
		t.Fatalf("Reconcile = %v, want a final unsupported: the operation settles outcome_unknown", err)
	}
	if n := len(r.f.calls("acct1")); n != 1 {
		t.Errorf("reconciling made calls: %d total, want still 1", n)
	}
}

func TestServerErrorIsOutcomeUnknown(t *testing.T) {
	r := newRig(t)
	r.issue("")
	r.f.respond("acct1", answer{status: http.StatusBadGateway})
	out, err := r.execute(t, openAction())
	wantError(t, out, err, model.OutcomeUnknown)
}

func TestRateLimitAndUnreachableRetryWhileTheTriggerLives(t *testing.T) {
	r := newRig(t)
	r.issue("")
	r.f.respond("acct1", rateLimitedFor(time.Second), unreachable)
	out, err := r.execute(t, openAction())
	if me := errorOf(t, out, err, model.RateLimited); me.Retry != model.RetrySameOperation {
		t.Fatalf("retry = %v, want the same operation: nothing was applied", me.Retry)
	}
	out, err = r.execute(t, openAction())
	wantError(t, out, err, model.Transient)
	// The retry the engine makes is the same attempt again: it works while
	// the trigger_id lives, and is refused locally once it does not.
	out, err = r.execute(t, openAction())
	wantAccepted(t, out, err)
	r.x.now = func() time.Time { return testNow.Add(3 * time.Second) }
	before := len(r.f.calls("acct1"))
	out, err = r.execute(t, openAction())
	wantRejected(t, out, err, model.DeadlineExpired)
	if n := len(r.f.calls("acct1")); n != before {
		t.Errorf("an expired retry made %d calls", n-before)
	}
}

func TestUpdateByViewIDAndExternalID(t *testing.T) {
	r := newRig(t)
	r.issue("")
	open := openAction(func(d map[string]any) { d["view"] = modal(func(v map[string]any) { v["external_id"] = "feedback-1" }) })
	wantAccepted2(t, r, open)
	first := r.acct.view("V1001").hash

	out, err := r.execute(t, updateAction())
	wantAccepted(t, out, err)
	call := r.callsOf("views.update")[0].args(t)
	if call["view_id"] != "V1001" || call["trigger_id"] != nil || call["external_id"] != nil {
		t.Errorf("arguments = %v, want the view ID alone and no trigger_id", call)
	}
	if r.acct.view("V1001").view["title"].(map[string]any)["text"] != "Updated" {
		t.Errorf("fake view = %+v, want the new title", r.acct.view("V1001").view)
	}
	if len(out.References) != 1 || out.References[0].ID != "V1001" || out.References[0].Kind != RefKindView {
		t.Errorf("references = %+v, want the updated view", out.References)
	}

	out, err = r.execute(t, updateAction(func(d map[string]any) { delete(d, "view_id"); d["external_id"] = "feedback-1" }))
	wantAccepted(t, out, err)

	out, err = r.execute(t, updateAction(func(d map[string]any) { d["hash"] = first }))
	wantRejected(t, out, err, model.Conflict) // the hash moved on with the updates

	current := r.acct.view("V1001").hash
	out, err = r.execute(t, updateAction(func(d map[string]any) { d["hash"] = current }))
	wantAccepted(t, out, err)
	if got := r.callsOf("views.update")[3].args(t)["hash"]; got != current {
		t.Errorf("hash = %v, want the one given, %v", got, current)
	}

	out, err = r.execute(t, updateAction(func(d map[string]any) { d["view_id"] = "V9999" }))
	wantRejected(t, out, err, model.NotFound)
}

func wantAccepted2(t *testing.T, r *rig, a model.Extension) {
	t.Helper()
	out, err := r.execute(t, a)
	wantAccepted(t, out, err)
}

func TestPushStacksViewsUpToTheLimit(t *testing.T) {
	r := newRig(t)
	r.issue("")
	wantAccepted2(t, r, openAction())
	for i, id := range []string{"1.2.aaa", "1.2.bbb"} {
		r.acct.trigger(id, testNow.Add(-time.Second), "V1001")
		out, err := r.execute(t, pushAction(func(d map[string]any) { d["trigger_id"] = id }))
		wantAccepted(t, out, err)
		if len(out.References) != 1 || out.References[0].ID != "V"+strconv.Itoa(1002+i) {
			t.Errorf("push %d references = %+v", i, out.References)
		}
	}
	r.acct.trigger("1.2.ccc", testNow.Add(-time.Second), "V1001")
	out, err := r.execute(t, pushAction(func(d map[string]any) { d["trigger_id"] = "1.2.ccc" }))
	wantRejected(t, out, err, model.Conflict)
	if n := len(r.callsOf("views.push")); n != 3 {
		t.Errorf("%d pushes, want 3 attempts", n)
	}
}

func TestUnknownActionsAreRefusedBeforeAnyCall(t *testing.T) {
	r := newRig(t)
	r.issue("")
	a := openAction()
	a.Version = 2
	out, err := r.execute(t, a)
	wantRejected(t, out, err, model.SchemaMismatch)
	a = openAction()
	a.Name = "slack.chat.views.publish"
	out, err = r.execute(t, a)
	wantRejected(t, out, err, model.Unsupported)
	if n := len(r.f.calls("acct1")); n != 0 {
		t.Errorf("%d calls, want none", n)
	}
	other := model.ChannelRef{Tenant: testTenant, ID: "00000ad0-0000-7000-8000-0000000000bb"}
	out, err = r.x.Execute(t.Context(), adapter.ExecuteRequest{Effect: r.effect(model.ExternalRef{Channel: other}), Action: openAction()})
	wantError(t, out, err, model.Validation)
}

func TestClosedInstanceIsUnavailable(t *testing.T) {
	r := newRig(t)
	_ = r.x.Close(t.Context())
	out, err := r.execute(t, openAction())
	wantError(t, out, err, model.Unavailable)
}

func TestNoCredentialsPortIsUnavailable(t *testing.T) {
	x := open(t, testConfig(), func(d *adapterDeps) { d.Credentials = nil })
	_, err := x.Execute(t.Context(), adapter.ExecuteRequest{Effect: adapter.Effect{Route: x.roomRef(testRoom)}, Action: openAction()})
	if !model.IsCode(err, model.Unavailable) {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

func TestViewFieldsArePassedThroughUnchanged(t *testing.T) {
	r := newRig(t)
	r.issue("")
	a := openAction(func(d map[string]any) {
		d["view"] = modal(func(v map[string]any) { v["future_field"] = map[string]any{"a": []any{1, 2.5, "<x>"}} })
	})
	wantAccepted2(t, r, a)
	view := r.callsOf("views.open")[0].args(t)["view"].(map[string]any)
	want := map[string]any{"a": []any{1.0, 2.5, "<x>"}}
	if !reflect.DeepEqual(view["future_field"], want) {
		t.Errorf("future_field = %v, want %v: sent as given", view["future_field"], want)
	}
}

// badViews are actions Slack's references say it refuses. Each is judged
// alike at the call (ValidateAction) and at the attempt (Execute), with a
// message that repeats no value.
func badViews() map[string]model.Extension {
	long := func(n int) string { return strings.Repeat("é", n) } // characters, not bytes
	set := func(k string, v any) func(map[string]any) {
		return func(d map[string]any) {
			d["view"] = modal(func(m map[string]any) {
				if v == nil {
					delete(m, k)
				} else {
					m[k] = v
				}
			})
		}
	}
	return map[string]model.Extension{
		"data not an object":     {Name: ExtensionViewsOpen, Version: 1, Data: []byte(`[]`)},
		"data null":              {Name: ExtensionViewsOpen, Version: 1, Data: []byte(`null`)},
		"no trigger":             openAction(func(d map[string]any) { delete(d, "trigger_id") }),
		"trigger format":         openAction(func(d map[string]any) { d["trigger_id"] = secretText }),
		"trigger not a string":   openAction(func(d map[string]any) { d["trigger_id"] = 5 }),
		"no expiry":              openAction(func(d map[string]any) { delete(d, "expires_at") }),
		"expiry format":          openAction(func(d map[string]any) { d["expires_at"] = "soon" }),
		"no view":                openAction(func(d map[string]any) { delete(d, "view") }),
		"view not an object":     openAction(func(d map[string]any) { d["view"] = "x" }),
		"view null":              openAction(func(d map[string]any) { d["view"] = nil }),
		"type not modal":         openAction(set("type", "home")),
		"no type":                openAction(set("type", nil)),
		"no title":               openAction(set("title", nil)),
		"title not plain_text":   openAction(set("title", map[string]any{"type": "mrkdwn", "text": "x"})),
		"title empty":            openAction(set("title", plain(""))),
		"title 25 characters":    openAction(set("title", plain(long(25)))),
		"close 25 characters":    openAction(set("close", plain(long(25)))),
		"submit 25 characters":   openAction(set("submit", plain(long(25)))),
		"submit not plain_text":  openAction(set("submit", "Send")),
		"input without submit":   openAction(set("submit", nil)),
		"no blocks":              openAction(set("blocks", nil)),
		"blocks not an array":    openAction(set("blocks", map[string]any{})),
		"101 blocks":             openAction(set("blocks", make([]any, 101))),
		"callback_id 256":        openAction(set("callback_id", long(256))),
		"callback_id not string": openAction(set("callback_id", 5)),
		"external_id 256":        openAction(set("external_id", long(256))),
		"metadata 3001":          openAction(set("private_metadata", long(3001))),
		"clear_on_close string":  openAction(set("clear_on_close", "yes")),
		"notify_on_close string": openAction(set("notify_on_close", "yes")),
		"view over 250 KiB":      openAction(set("private_metadata", nil), func(d map[string]any) { d["view"].(map[string]any)["x"] = strings.Repeat("a", 250<<10) }),
		"update no id":           updateAction(func(d map[string]any) { delete(d, "view_id") }),
		"update both ids":        updateAction(func(d map[string]any) { d["external_id"] = "x" }),
		"update view_id format":  updateAction(func(d map[string]any) { d["view_id"] = secretText }),
		"update external 256":    updateAction(func(d map[string]any) { delete(d, "view_id"); d["external_id"] = long(256) }),
		"update hash format":     updateAction(func(d map[string]any) { d["hash"] = secretText + " !" }),
		"update hash type":       updateAction(func(d map[string]any) { d["hash"] = 5 }),
		"update no view":         updateAction(func(d map[string]any) { delete(d, "view") }),
		"push bad trigger":       pushAction(func(d map[string]any) { d["trigger_id"] = "no" }),
	}
}

func TestInvalidViewActionsAreRefusedAtTheCallAndTheAttempt(t *testing.T) {
	f := NewFactory(Options{})
	r := newRig(t)
	r.issue("")
	for name, a := range badViews() {
		t.Run(name, func(t *testing.T) {
			issues := f.ValidateAction(model.Audience{Visibility: model.Public}, a)
			if len(issues) != 1 || issues[0].Code != string(model.Validation) || !strings.HasPrefix(issues[0].Field, "data") {
				t.Fatalf("issues = %+v, want one validation issue under data", issues)
			}
			out, err := r.execute(t, a)
			wantRejected(t, out, err, model.Validation)
			if out.Failure.SafeMessage != issues[0].Message {
				t.Errorf("attempt says %q, call says %q: one set of rules", out.Failure.SafeMessage, issues[0].Message)
			}
			for _, leak := range []string{secretText, testTrigger} {
				if strings.Contains(issues[0].Message, leak) || strings.Contains(out.Failure.SafeMessage, leak) {
					t.Errorf("message %q repeats a value", issues[0].Message)
				}
			}
			if n := len(r.f.calls("acct1")); n != 0 {
				t.Fatalf("%d calls reached Slack for an action it would refuse", n)
			}
		})
	}
}

func TestViewLimitsAreInclusive(t *testing.T) {
	f := NewFactory(Options{})
	long := func(n int) string { return strings.Repeat("é", n) }
	ok := map[string]model.Extension{
		"title 24 characters (48 bytes)": openAction(func(d map[string]any) { d["view"] = modal(func(v map[string]any) { v["title"] = plain(long(24)) }) }),
		"100 blocks": openAction(func(d map[string]any) {
			d["view"] = modal(func(v map[string]any) {
				blocks := make([]any, 100)
				for i := range blocks {
					blocks[i] = map[string]any{"type": "divider"}
				}
				v["blocks"], v["submit"] = blocks, nil
			})
		}),
		"metadata 3000": openAction(func(d map[string]any) {
			d["view"] = modal(func(v map[string]any) { v["private_metadata"] = long(3000) })
		}),
		"callback_id 255": openAction(func(d map[string]any) { d["view"] = modal(func(v map[string]any) { v["callback_id"] = long(255) }) }),
		"no submit, no input": openAction(func(d map[string]any) {
			d["view"] = modal(func(v map[string]any) { delete(v, "submit"); v["blocks"] = []any{map[string]any{"type": "divider"}} })
		}),
		"null optionals": openAction(func(d map[string]any) {
			d["view"] = modal(func(v map[string]any) { v["close"], v["callback_id"], v["clear_on_close"] = nil, nil, nil })
		}),
		"push":              pushAction(),
		"update by hash":    updateAction(func(d map[string]any) { d["hash"] = "156772938.1827394" }),
		"update no hash":    updateAction(func(d map[string]any) { d["hash"] = nil }),
		"update external":   updateAction(func(d map[string]any) { delete(d, "view_id"); d["external_id"] = "feedback-1" }),
		"unknown top field": openAction(func(d map[string]any) { d["anything"] = []any{1} }),
	}
	for name, a := range ok {
		t.Run(name, func(t *testing.T) {
			if issues := f.ValidateAction(model.Audience{}, a); len(issues) != 0 {
				t.Fatalf("issues = %+v, want none", issues)
			}
		})
	}
	big := openAction(func(d map[string]any) {
		d["view"] = modal(func(v map[string]any) { v["private_metadata"] = nil })
	})
	var d map[string]json.RawMessage
	_ = json.Unmarshal(big.Data, &d)
	// A view of exactly MaxViewBytes passes, one byte more does not.
	for _, c := range []struct {
		extra int
		want  bool
	}{{0, true}, {1, false}} {
		view := `{"type":"modal","title":{"type":"plain_text","text":"t"},"blocks":[],"pad":"` + strings.Repeat("a", (250<<10)-len(`{"type":"modal","title":{"type":"plain_text","text":"t"},"blocks":[],"pad":""}`)+c.extra) + `"}`
		d["view"] = json.RawMessage(view)
		data, _ := json.Marshal(d)
		issues := f.ValidateAction(model.Audience{}, model.Extension{Name: ExtensionViewsOpen, Version: 1, Data: data})
		if (len(issues) == 0) != c.want {
			t.Errorf("view of %d bytes: issues %+v, want ok=%v", len(view), issues, c.want)
		}
	}
}

func TestValidateActionJudgesOnlyItsOwnEffects(t *testing.T) {
	f := NewFactory(Options{})
	if issues := f.ValidateAction(model.Audience{}, model.Extension{Name: "org.example.other", Version: 1, Data: []byte(`{}`)}); issues != nil {
		t.Errorf("issues = %+v for another name, want none: core refuses it", issues)
	}
	a := openAction(func(d map[string]any) { delete(d, "view") })
	a.Version = 7
	if issues := f.ValidateAction(model.Audience{}, a); issues != nil {
		t.Errorf("issues = %+v for another version, want none: core refuses it", issues)
	}
}

func TestAnyAudienceIsAccepted(t *testing.T) {
	// A modal is shown to the person who acted and is no message: the
	// audience is policy's to judge, never the adapter's.
	r := newRig(t)
	r.issue("")
	e := r.effect(r.x.roomRef(testRoom))
	e.Audience = model.Audience{Visibility: model.Internal, PolicyRef: "staff"}
	out, err := r.x.Execute(t.Context(), adapter.ExecuteRequest{Effect: e, Action: openAction()})
	wantAccepted(t, out, err)
}

func TestTriggerOfReadsWhatTheAdapterReported(t *testing.T) {
	deadline := testNow.Add(AckDeadline)
	rec := func(id string, mod ...func(*model.Interaction)) model.Interaction {
		in := model.Interaction{
			Source: Type, Deadline: deadline,
			External: []model.ExternalRef{model.ProviderInteractionRef(testChannel, id)},
		}
		for _, m := range mod {
			m(&in)
		}
		return in
	}
	for _, c := range []struct {
		name string
		in   model.Interaction
		want bool
	}{
		{"action", rec("action:" + testTrigger), true},
		{"command", rec("command:" + testTrigger), true},
		{"submission", rec("view:V0123ABC:" + testTrigger), true},
		{"digest of a payload without one", rec("action:sha256-0123456789abcdef0123456789abcdef"), false},
		{"unknown prefix", rec("shortcut:" + testTrigger), false},
		{"no trigger", rec("action:"), false},
		{"another adapter", rec("action:"+testTrigger, func(i *model.Interaction) { i.Source = "discord.chat" }), false},
		{"no reference", rec("", func(i *model.Interaction) { i.External = nil }), false},
		{"not an interaction reference", rec("action:"+testTrigger, func(i *model.Interaction) { i.External[0].Kind = model.RefKindMessage }), false},
		{"no deadline", rec("action:"+testTrigger, func(i *model.Interaction) { i.Deadline = time.Time{} }), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr, ok := TriggerOf(c.in)
			if ok != c.want {
				t.Fatalf("TriggerOf = %+v, %v; want ok=%v", tr, ok, c.want)
			}
			if ok && (tr.ID != testTrigger || !tr.ExpiresAt.Equal(deadline)) {
				t.Errorf("trigger = %+v, want %s until %v", tr, testTrigger, deadline)
			}
		})
	}
}

// TestTriggerOfAgreesWithWhatTheHandlerRecords ties the reader to the
// producer: whatever interaction ID the adapter reports for each payload,
// the trigger_id of that payload is what TriggerOf reads back from the
// record the engine builds from it (its External reference, its Deadline).
func TestTriggerOfAgreesWithWhatTheHandlerRecords(t *testing.T) {
	const trigger = "13345224609.738474920.8088930838d88f008e0"
	for name, req := range map[string]request{
		"button": interactionPayload(buttonPayload(func(p map[string]any) { p["trigger_id"] = trigger })),
		"command": formRequest(url.Values{
			"team_id": {testTeam}, "api_app_id": {testApp}, "user_id": {testPerson}, "command": {"/deploy"}, "text": {"api"},
			"channel_id": {testRoom}, "trigger_id": {trigger}, "response_url": {hookURL},
		}),
		"submission": interactionPayload(map[string]any{
			"type": "view_submission", "api_app_id": testApp, "trigger_id": trigger,
			"team": map[string]any{"id": testTeam}, "user": map[string]any{"id": testPerson, "team_id": testTeam},
			"view": map[string]any{"id": "V0123ABC", "callback_id": "feedback", "state": map[string]any{"values": map[string]any{}}, "response_urls": []any{
				map[string]any{"channel_id": testRoom, "response_url": hookURL},
			}},
		}),
	} {
		t.Run(name, func(t *testing.T) {
			x, s := open(t, testConfig()), &sinkRecorder{}
			serve(t, x, s, req)
			in := wantOne(t, s).Interaction
			rec := model.Interaction{Source: Type, Deadline: in.Deadline, External: []model.ExternalRef{model.ProviderInteractionRef(testChannel, in.ID)}}
			tr, ok := TriggerOf(rec)
			if !ok || tr.ID != trigger || !tr.ExpiresAt.Equal(testNow.Add(TriggerLifetime)) {
				t.Fatalf("TriggerOf(%q) = %+v, %v; want %s until three seconds after the request", in.ID, tr, ok, trigger)
			}
		})
	}
}

func TestTriggerLifetimeIsTheAcknowledgementDeadline(t *testing.T) {
	// TriggerOf reports the acknowledgement deadline as the instant the
	// trigger_id stops working: both are three seconds.
	if TriggerLifetime != AckDeadline {
		t.Fatalf("trigger lifetime %v, acknowledgement deadline %v", TriggerLifetime, AckDeadline)
	}
}

func TestErrorsNeverRepeatTheTriggerOrTheView(t *testing.T) {
	r := newRig(t)
	r.issue("")
	r.x.now = func() time.Time { return testNow.Add(time.Hour) }
	for _, err := range []error{
		func() error { _, e := r.execute(t, openAction()); return e }(),
	} {
		if err != nil && (strings.Contains(err.Error(), testTrigger) || strings.Contains(err.Error(), secretText)) {
			t.Errorf("error %q repeats a value", err)
		}
	}
	r.x.now = func() time.Time { return testNow }
	r.f.respond("acct1", answerLost)
	_, err := r.execute(t, openAction())
	if me, ok := errors.AsType[*model.Error](err); !ok || strings.Contains(me.Error(), testTrigger) || strings.Contains(me.SafeMessage, secretText) {
		t.Errorf("error = %v", err)
	}
}
