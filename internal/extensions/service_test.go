package extensions_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/extensions"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

const unknownFields = `{"message":"m1","x_future":{"nested":[1,2.50,"<b>"]},"x_flag":null}`

// Execute queues the action durably; the worker hands it to the adapter
// with every field intact, on the conversation's stored route, under the
// request's audience, and the delivery event carries that audience.
func TestExecuteRoundTripsActionAndAudience(t *testing.T) {
	x := newEnv(t)
	a, err := x.c.Extensions().Execute(x.ctx, execReq("op-pin", x.conv.Ref.Ref(), unknownFields))
	if err != nil {
		t.Fatal(err)
	}
	if a.Operation.State != model.Queued || a.Replayed {
		t.Fatalf("acceptance = %+v, want a new queued operation", a)
	}
	if n := len(x.f.executed()); n != 0 {
		t.Fatalf("adapter executed %d actions before Runtime.Run, want 0", n)
	}
	checks := x.pol.all()
	if len(checks) != 1 || checks[0].Action != pinAction || checks[0].Audience != audience ||
		checks[0].Resource != x.conv.Ref.Ref() {
		t.Fatalf("policy checks = %+v, want the definition's action on the target under the audience", checks)
	}
	if n := x.step(x.rt); n != 1 {
		t.Fatalf("executed %d items, want 1", n)
	}
	op := x.op(a.Operation.Ref)
	enginetest.RequireState(t, op, model.ProviderAccepted, "")
	got := x.f.executed()
	if len(got) != 1 {
		t.Fatalf("adapter executions = %d, want 1", len(got))
	}
	r := got[0]
	if r.Operation != a.Operation.Ref || r.Route != x.conv.ReplyRoute || r.Audience != audience {
		t.Fatalf("effect = %+v, want the operation on the stored route under the audience", r.Effect)
	}
	if r.Action.Name != pinName || r.Action.Version != 1 || !jsonEqual(t, r.Action.Data, []byte(unknownFields)) {
		t.Fatalf("action = %s v%d %s, want every field of %s", r.Action.Name, r.Action.Version, r.Action.Data, unknownFields)
	}
	if len(op.ProviderReferences) != 1 || op.ProviderReferences[0].ID != "p1" {
		t.Fatalf("provider references = %+v", op.ProviderReferences)
	}
	if checks := x.pol.all(); checks[len(checks)-2].Action != pinAction {
		t.Fatalf("policy checks = %+v, want the action reauthorized at execution", checks)
	}
	events := x.events()
	if len(events) != 1 || events[0].Kind != model.EventDeliveryUpdated || events[0].Audience != audience {
		t.Fatalf("events = %+v, want one delivery.updated under the action's audience", events)
	}
}

// A channel or inbox target executes on that channel with a channel-level
// route.
func TestExecuteOnChannelAndInbox(t *testing.T) {
	x := newEnv(t)
	for i, target := range []model.Ref{x.ch.Ref.Ref(), x.inbox.Ref.Ref()} {
		if _, err := x.c.Extensions().Execute(x.ctx, execReq(model.OperationID("op-"+string(target.Kind)), target, `{}`)); err != nil {
			t.Fatalf("%s: %v", target.Kind, err)
		}
		x.step(x.rt)
		got := x.f.executed()
		if len(got) != i+1 || got[i].Route != (model.ExternalRef{Channel: x.ch.Ref}) {
			t.Fatalf("%s: executions = %+v, want one on the channel route", target.Kind, got)
		}
	}
}

// Replaying an operation ID with the same payload returns the same
// operation, before and after execution, and never executes again; the
// adapter saw the operation the caller holds. Another payload conflicts.
func TestExecuteReplayKeepsActionCorrelation(t *testing.T) {
	x := newEnv(t)
	req := execReq("op-replay", x.conv.Ref.Ref(), unknownFields)
	first, err := x.c.Extensions().Execute(x.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := x.c.Extensions().Execute(x.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("replay = %+v, want operation %v replayed", again, first.Operation.Ref)
	}
	x.step(x.rt)
	after, err := x.c.Extensions().Execute(x.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Replayed || after.Operation.Ref != first.Operation.Ref || after.Operation.State != model.ProviderAccepted {
		t.Fatalf("replay after execution = %+v, want the settled operation", after)
	}
	if n := x.step(x.rt); n != 0 {
		t.Fatalf("replay queued %d items, want 0", n)
	}
	got := x.f.executed()
	if len(got) != 1 || got[0].Operation != first.Operation.Ref {
		t.Fatalf("executions = %+v, want one carrying %v", got, first.Operation.Ref)
	}
	for name, edit := range map[string]func(*model.ExecuteExtension){
		"data":     func(r *model.ExecuteExtension) { r.Action.Data = json.RawMessage(`{"message":"m2"}`) },
		"audience": func(r *model.ExecuteExtension) { r.Audience.PolicyRef = "everyone" },
	} {
		other := execReq("op-replay", x.conv.Ref.Ref(), unknownFields)
		edit(&other)
		_, err := x.c.Extensions().Execute(x.ctx, other)
		if !model.IsCode(err, model.Conflict) {
			t.Fatalf("%s changed: error = %v, want conflict", name, err)
		}
	}
}

// Unknown, incompatible or unservable actions are rejected before
// authorization, storage or any adapter call.
func TestIncompatibleExecuteNeverRuns(t *testing.T) {
	x := newEnv(t)
	conv := x.conv.Ref.Ref()
	cases := []struct {
		name string
		edit func(*model.ExecuteExtension)
		code model.ErrorCode
		want string
	}{
		{"unknown name", func(r *model.ExecuteExtension) { r.Action.Name = "com.example.chat.messages.archive" },
			model.Unsupported, "extension not registered"},
		{"unknown version", func(r *model.ExecuteExtension) { r.Action.Version = 2 },
			model.SchemaMismatch, "extension version not registered"},
		{"read as effect", func(r *model.ExecuteExtension) { r.Action.Name = lookupName },
			model.SchemaMismatch, "extension is not an effect"},
		{"event as effect", func(r *model.ExecuteExtension) { r.Action.Name = pinnedName },
			model.SchemaMismatch, "extension is not an effect"},
		{"adapter without extensions", func(r *model.ExecuteExtension) { r.Target = x.plainC.Ref.Ref() },
			model.Unsupported, "does not declare extension_execute"},
		{"unsupported target kind", func(r *model.ExecuteExtension) {
			r.Target = model.Ref{Tenant: enginetest.TenantA, Kind: model.KindContact, ID: convID}
		}, model.Unsupported, "must be a channel, inbox or conversation"},
		{"foreign tenant", func(r *model.ExecuteExtension) { r.Target.Tenant = enginetest.TenantB },
			model.NotFound, "not found"},
		{"missing conversation", func(r *model.ExecuteExtension) { r.Target.ID = plainID },
			model.NotFound, "not found"},
		{"invalid action data", func(r *model.ExecuteExtension) { r.Action.Data = json.RawMessage(`{"a":`) },
			model.Validation, "action.data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := execReq(model.OperationID("op-"+strings.ReplaceAll(tc.name, " ", "-")), conv, `{}`)
			tc.edit(&req)
			_, err := x.c.Extensions().Execute(x.ctx, req)
			enginetest.RequireCode(t, err, tc.code, tc.want)
		})
	}
	if n := len(x.pol.all()); n != 0 {
		t.Fatalf("policy consulted %d times, want 0", n)
	}
	if n := x.step(x.rt); n != 0 {
		t.Fatalf("rejected actions queued %d items", n)
	}
	if n := len(x.f.executed()); n != 0 {
		t.Fatalf("adapter executed %d actions", n)
	}
}

// A stored action whose definition the executing engine no longer
// registers, or registers as another kind, fails without retry and without
// reaching the adapter.
func TestExecutionRechecksDefinition(t *testing.T) {
	for name, tc := range map[string]struct {
		defs []adapter.ExtensionDefinition
		code model.ErrorCode
	}{
		"removed": {definitions()[1:], model.Unsupported},
		"now a read": {func() []adapter.ExtensionDefinition {
			d := definitions()
			d[0].Kind, d[0].RequiredCapability = model.ExtensionKindRead, model.CapabilityExtensionQuery
			return d
		}(), model.SchemaMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			x := newEnv(t)
			a, err := x.c.Extensions().Execute(x.ctx, execReq("op-recheck", x.conv.Ref.Ref(), `{}`))
			if err != nil {
				t.Fatal(err)
			}
			restarted := runtime.New(x.compose(tc.defs), extensions.Effector())
			x.step(restarted)
			op := x.op(a.Operation.Ref)
			enginetest.RequireState(t, op, model.Failed, tc.code)
			if op.Failure.Retry != model.NoRetry {
				t.Fatalf("retry = %q, want none", op.Failure.Retry)
			}
			if n := len(x.f.executed()); n != 0 {
				t.Fatalf("adapter executed %d actions", n)
			}
		})
	}
}

// Execution reauthorizes the originating actor: a denial at execution
// time fails the operation without an adapter call.
func TestExecutionRechecksPolicy(t *testing.T) {
	x := newEnv(t)
	a, err := x.c.Extensions().Execute(x.ctx, execReq("op-deny", x.conv.Ref.Ref(), `{}`))
	if err != nil {
		t.Fatal(err)
	}
	x.pol.set(true)
	x.step(x.rt)
	x.pol.set(false)
	enginetest.RequireState(t, x.op(a.Operation.Ref), model.Failed, model.Unauthorized)
	if n := len(x.f.executed()); n != 0 {
		t.Fatalf("adapter executed %d actions", n)
	}
}

// A withdrawn extension permission fails the attempt before the adapter.
func TestExecutionRechecksCapability(t *testing.T) {
	x := newEnv(t)
	a, err := x.c.Extensions().Execute(x.ctx, execReq("op-withdrawn", x.conv.Ref.Ref(), `{}`))
	if err != nil {
		t.Fatal(err)
	}
	x.f.withdraw(model.CapabilityExtensionExecute)
	x.step(x.rt)
	enginetest.RequireState(t, x.op(a.Operation.Ref), model.Failed, model.Unauthorized)
	if n := len(x.f.executed()); n != 0 {
		t.Fatalf("adapter executed %d actions", n)
	}
}

// An expected revision must match the target's current revision.
func TestExecuteExpectedRevision(t *testing.T) {
	x := newEnv(t)
	stale := execReq("op-stale", x.conv.Ref.Ref(), `{}`)
	stale.ExpectedRevision = "cv0"
	_, err := x.c.Extensions().Execute(x.ctx, stale)
	enginetest.RequireCode(t, err, model.Conflict, "target revision changed")
	current := execReq("op-current", x.conv.Ref.Ref(), `{}`)
	current.ExpectedRevision = x.conv.Revision
	if _, err := x.c.Extensions().Execute(x.ctx, current); err != nil {
		t.Fatal(err)
	}
}

// A denied Execute stores nothing and reveals no replay.
func TestExecuteDenied(t *testing.T) {
	x := newEnv(t)
	x.pol.set(true)
	_, err := x.c.Extensions().Execute(x.ctx, execReq("op-denied", x.conv.Ref.Ref(), `{}`))
	enginetest.RequireCode(t, err, model.Unauthorized, "")
	x.pol.set(false)
	if n := x.step(x.rt); n != 0 {
		t.Fatalf("denied action queued %d items", n)
	}
}

// Query authorizes the read under its audience, hands the adapter the
// query unchanged and returns its result with every field intact.
func TestQueryRoundTripsResult(t *testing.T) {
	x := newEnv(t)
	result := `{"pinned":true,"x_future":{"by":"staff","at":[1,2]}}`
	x.f.result = func(model.QueryExtension) (model.Extension, error) {
		return model.Extension{Name: resultName, Version: 1, Data: json.RawMessage(result)}, nil
	}
	req := queryReq(x.conv.Ref.Ref())
	out, err := x.c.Extensions().Query(x.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != resultName || out.Version != 1 || !jsonEqual(t, out.Data, []byte(result)) {
		t.Fatalf("result = %s v%d %s, want %s", out.Name, out.Version, out.Data, result)
	}
	q := x.f.queried()
	if len(q) != 1 || q[0].Target != req.Target || q[0].Audience != audience || !jsonEqual(t, q[0].Query.Data, req.Query.Data) {
		t.Fatalf("adapter queries = %+v, want the request unchanged", q)
	}
	checks := x.pol.all()
	if len(checks) != 1 || checks[0].Action != model.ActionConversationRead || checks[0].Audience != audience {
		t.Fatalf("policy checks = %+v, want the definition's action under the audience", checks)
	}
	if n := x.step(x.rt); n != 0 {
		t.Fatalf("a read queued %d items", n)
	}
}

// Results outside the declared schema never pass through.
func TestQueryRejectsUndeclaredResults(t *testing.T) {
	cases := map[string]struct {
		out  model.Extension
		code model.ErrorCode
	}{
		"other schema":  {model.Extension{Name: "com.example.chat.messages.other", Version: 1}, model.SchemaMismatch},
		"other version": {model.Extension{Name: resultName, Version: 2}, model.SchemaMismatch},
		"invalid json":  {model.Extension{Name: resultName, Version: 1, Data: json.RawMessage(`{"a":`)}, model.Permanent},
		"oversized": {model.Extension{Name: resultName, Version: 1, Data: json.RawMessage(
			`"` + strings.Repeat("a", model.DefaultContentLimits().MaxStructuredBytes) + `"`)}, model.Permanent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			x := newEnv(t)
			x.f.result = func(model.QueryExtension) (model.Extension, error) { return tc.out, nil }
			_, err := x.c.Extensions().Query(x.ctx, queryReq(x.conv.Ref.Ref()))
			enginetest.RequireCode(t, err, tc.code, "extension result")
		})
	}
}

// Unknown, incompatible, unauthorized or unavailable reads never return
// adapter data.
func TestQueryRejections(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*env, *model.QueryExtension)
		code  model.ErrorCode
		want  string
		calls int
	}{
		{"unknown name", func(_ *env, r *model.QueryExtension) { r.Query.Name = "com.example.chat.messages.history" },
			model.Unsupported, "extension not registered", 0},
		{"unknown version", func(_ *env, r *model.QueryExtension) { r.Query.Version = 3 },
			model.SchemaMismatch, "extension version not registered", 0},
		{"effect as read", func(_ *env, r *model.QueryExtension) { r.Query.Name = pinName },
			model.SchemaMismatch, "extension is not a read", 0},
		{"adapter without extensions", func(x *env, r *model.QueryExtension) { r.Target = x.plainC.Ref.Ref() },
			model.Unsupported, "does not declare extension_query", 0},
		{"foreign tenant", func(_ *env, r *model.QueryExtension) { r.Target.Tenant = enginetest.TenantB },
			model.NotFound, "not found", 0},
		{"denied", func(x *env, _ *model.QueryExtension) { x.pol.set(true) },
			model.Unauthorized, "denied", 0},
		{"withdrawn", func(x *env, _ *model.QueryExtension) { x.f.withdraw(model.CapabilityExtensionQuery) },
			model.Unauthorized, "withdrawn", 0},
		{"adapter error text hidden", func(x *env, _ *model.QueryExtension) {
			x.f.result = func(model.QueryExtension) (model.Extension, error) {
				return model.Extension{}, errSecret
			}
		}, model.Unavailable, "unavailable", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newEnv(t)
			req := queryReq(x.conv.Ref.Ref())
			tc.setup(x, &req)
			_, err := x.c.Extensions().Query(x.ctx, req)
			enginetest.RequireCode(t, err, tc.code, tc.want)
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks adapter text: %v", err)
			}
			if n := len(x.f.queried()); n != tc.calls {
				t.Fatalf("adapter queries = %d, want %d", n, tc.calls)
			}
		})
	}
}

var errSecret = errorString("provider secret token-123")

type errorString string

func (e errorString) Error() string { return string(e) }

// needsMessage is an adapter rule: an action sets a message and, being
// internal here, never a public audience.
func needsMessage(a model.Audience, x model.Extension) []model.ValidationIssue {
	var d map[string]json.RawMessage
	_ = json.Unmarshal(x.Data, &d)
	var issues []model.ValidationIssue
	if a.Visibility == model.Public {
		issues = append(issues, model.ValidationIssue{Field: "audience", Code: string(model.Validation), Message: "never public"})
	}
	if _, ok := d["message"]; !ok {
		issues = append(issues, model.ValidationIssue{Field: "data.message", Code: string(model.Validation), Message: "required"})
	}
	return issues
}

// An adapter that validates actions refuses invalid fields at the call:
// a model.Validation error, nothing stored, no policy check, and the same
// operation ID free for a valid action.
func TestExecuteRefusesInvalidFieldsBeforeStoring(t *testing.T) {
	x := newEnv(t)
	x.f.rule = needsMessage
	conv := x.conv.Ref.Ref()
	_, err := x.c.Extensions().Execute(x.ctx, execReq("op-fields", conv, `{"x_future":1}`))
	enginetest.RequireCode(t, err, model.Validation, "action.data.message: required")
	if n := len(x.pol.all()); n != 0 {
		t.Fatalf("policy consulted %d times, want 0", n)
	}
	if n := x.step(x.rt); n != 0 {
		t.Fatalf("refused action queued %d items", n)
	}
	a, err := x.c.Extensions().Execute(x.ctx, execReq("op-fields", conv, unknownFields))
	if err != nil || a.Replayed || a.Operation.State != model.Queued {
		t.Fatalf("Execute after refusal = %+v, %v; want a new queued operation", a, err)
	}
	x.step(x.rt)
	enginetest.RequireState(t, x.op(a.Operation.Ref), model.ProviderAccepted, "")
}

// The adapter's rule sees the request's audience and the action as sent,
// and a refusal of the audience is as final as one of the fields.
func TestExecuteValidationSeesAudienceAndAction(t *testing.T) {
	x := newEnv(t)
	x.f.rule = needsMessage
	req := execReq("op-aud", x.conv.Ref.Ref(), unknownFields)
	req.Audience = model.Audience{Visibility: model.Public}
	_, err := x.c.Extensions().Execute(x.ctx, req)
	enginetest.RequireCode(t, err, model.Validation, "action.audience: never public")
	got := x.f.validatedActions()
	if len(got) != 1 || got[0].Name != pinName || !jsonEqual(t, got[0].Data, []byte(unknownFields)) {
		t.Fatalf("validated = %+v, want the action as sent", got)
	}
}

// A valid action is validated once per call, before it is stored, and not
// again when the attempt runs: the attempt is the adapter's own check.
func TestExecuteValidatesEachCallOnce(t *testing.T) {
	x := newEnv(t)
	x.f.rule = needsMessage
	req := execReq("op-once", x.conv.Ref.Ref(), unknownFields)
	if _, err := x.c.Extensions().Execute(x.ctx, req); err != nil {
		t.Fatal(err)
	}
	if n := len(x.f.validatedActions()); n != 1 {
		t.Fatalf("validated %d times for one call, want 1", n)
	}
	x.step(x.rt)
	if n := len(x.f.validatedActions()); n != 1 {
		t.Fatalf("validated %d times after execution, want 1: the attempt is the adapter's own check", n)
	}
}

// An adapter without the hook keeps the late path: the action is stored
// and the adapter validates at the attempt.
func TestExecuteWithoutValidationHookStoresFirst(t *testing.T) {
	x := newEnv(t)
	x.f.rule = needsMessage
	e := enginetest.MustNew(t, engine.Config{
		Store: x.store, Authorizer: x.pol, Adapters: []adapter.Factory{bare{x.f}, x.plain},
		Extensions: definitions(), Clock: x.clock, IDs: portstest.NewSequenceIDs(9),
	})
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	c, err := enginetest.For(e, enginetest.PrincipalA())
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Extensions().Execute(x.ctx, execReq("op-bare", x.conv.Ref.Ref(), `{}`))
	if err != nil || a.Operation.State != model.Queued {
		t.Fatalf("Execute = %+v, %v; want a queued operation", a, err)
	}
	if n := len(x.f.validatedActions()); n != 0 {
		t.Fatalf("hook ran %d times behind a wrapper hiding it", n)
	}
}
