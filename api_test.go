package uma_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/events"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/services"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

// implemented lists service and runtime methods with behaviour; the
// unsupported-stub test skips them.
var implemented = map[string]bool{
	"Messages.Reply": true, "Operations.Get": true, "Runtime.Run": true, "Runtime.Drain": true, "Runtime.Step": true,
	"Runtime.Webhook": true, "Runtime.Receive": true, "Runtime.ReceiverStatus": true, "Runtime.ReceiverState": true, "Runtime.Purge": true, "Operations.Replay": true, "Operations.Approve": true, "Operations.Reject": true, "Operations.Edit": true, "Operations.Trust": true, "Operations.Trusts": true, "Operations.ImportTrust": true, "Operations.ResetTrust": true, "Operations.ResetTrustSet": true, "Runtime.Import": true,
	"Configuration.ConfigureInbox": true, "Configuration.ConfigureChannel": true,
	"Configuration.Channels": true, "Configuration.Inboxes": true, "Configuration.Capabilities": true,
	"Configuration.ChannelByKey": true, "Configuration.InboxByKey": true,
	"Conversations.Get": true, "Conversations.List": true, "Conversations.Transition": true,
	"Messages.Get": true, "Messages.Raw": true, "Messages.List": true, "Messages.AddInternalNote": true,
	"Messages.Edit": true, "Messages.Delete": true, "Messages.React": true, "Messages.Receipt": true,
	"Events.Open": true, "Events.Poll": true, "Events.Ack": true, "Events.Close": true, "Events.List": true, "Events.Get": true,
	"Attachments.Prepare": true, "Attachments.Upload": true, "Attachments.Open": true,
	"Extensions.Query": true, "Extensions.Execute": true,
	"Interactions.Get": true, "Interactions.List": true, "Interactions.Respond": true,
	"Interactions.Ephemeral": true, "Interactions.Signal": true,
	"Streams.Get": true, "Streams.Start": true, "Streams.Update": true, "Streams.End": true,
	"WorkItems.Get": true, "WorkItems.List": true, "WorkItems.Act": true,
	"Tasks.Get": true, "Tasks.List": true, "Tasks.Act": true,
	"Privacy.Redact": true, "Privacy.Export": true, "Privacy.Open": true,
	"Contacts.List": true, "Contacts.Identities": true, "Contacts.History": true,
	"Contacts.Link": true, "Contacts.Unlink": true,
	"Contacts.Get": true, "Contacts.Create": true, "Contacts.Update": true,
	"Contacts.Merge": true, "Contacts.PreviewMerge": true,
	"Conversations.Start": true, "Operations.Lookup": true, "Operations.Reconcile": true, "Operations.List": true,
	"Conversations.AddLabels": true, "Conversations.RemoveLabels": true,
	"Conversations.AddParticipant": true, "Conversations.RemoveParticipant": true,
	"Conversations.Archive": true, "Conversations.Move": true, "Conversations.SetRead": true, "Conversations.Flag": true, "Conversations.Trash": true,
	"Labels.Configure": true, "Labels.Get": true, "Labels.List": true, "Labels.ByName": true, "Labels.Delete": true,
	"Conversations.Capabilities": true, "Conversations.Assign": true, "Conversations.Unassign": true,
	"Conversations.Snooze": true, "Conversations.Unsnooze": true, "Conversations.Link": true,
	"Conversations.Unlink": true, "Conversations.MarkRead": true, "Conversations.MarkUnread": true,
	"Conversations.ReadMarker": true,
}

func newEngine(t *testing.T) uma.Engine {
	t.Helper()
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Clock: portstest.NewFixedClock(time.Unix(0, 0)),
		IDs:   portstest.NewSequenceIDs(1),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

var principal = model.Principal{Tenant: "tenant-a", Actor: "actor-1"}

// Every service and runtime method not implemented yet returns a typed
// unsupported error instead of panicking.
func TestUnimplementedMethodsReturnUnsupported(t *testing.T) {
	e := newEngine(t)
	c, err := e.For(principal)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range unimplementedMethods(c, e.Runtime()) {
		if implemented[m.name] {
			continue
		}
		t.Run(m.name, func(t *testing.T) {
			requireCode(t, m.call(), model.Unsupported, m.name+": not implemented")
		})
	}
}

// After Close every service and runtime method refuses with the closed
// engine error, implemented or not.
func TestClosedEngineRefusesEveryMethod(t *testing.T) {
	e := newEngine(t)
	c, err := e.For(principal)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, m := range unimplementedMethods(c, e.Runtime()) {
		if m.noError {
			continue
		}
		requireCode(t, m.call(), model.Unavailable, "engine closed")
	}
}

// The public constants and the implementation's copies are one value each.
func TestConstantsMatchImplementation(t *testing.T) {
	pairs := []struct {
		name      string
		pub, impl any
	}{
		{"DefaultWebhookBodyLimit", uma.DefaultWebhookBodyLimit, engine.DefaultWebhookBodyLimit},
		{"DefaultAttachmentSizeLimit", uma.DefaultAttachmentSizeLimit, engine.DefaultAttachmentSizeLimit},
		{"DefaultMaxAttempts", uma.DefaultMaxAttempts, engine.DefaultMaxAttempts},
		{"DefaultBaseDelay", uma.DefaultBaseDelay, engine.DefaultBaseDelay},
		{"DefaultMaxDelay", uma.DefaultMaxDelay, engine.DefaultMaxDelay},
		{"DefaultMaxClaims", uma.DefaultMaxClaims, engine.DefaultMaxClaims},
		{"DefaultMaxInFlight", uma.DefaultMaxInFlight, engine.DefaultMaxInFlight},
		{"FingerprintVersion1", uma.FingerprintVersion1, operations.FingerprintVersion1},
		{"WorkKindOperation", uma.WorkKindOperation, operations.WorkKindOperation},
		{"DefaultPageLimit", uma.DefaultPageLimit, services.DefaultPageLimit},
		{"MaxPageLimit", uma.MaxPageLimit, services.MaxPageLimit},
		{"DefaultEventListLimit", uma.DefaultEventListLimit, events.DefaultListLimit},
		{"MaxEventListLimit", uma.MaxEventListLimit, events.MaxListLimit},
	}
	for _, p := range pairs {
		if p.pub != p.impl {
			t.Errorf("%s = %v, implementation uses %v", p.name, p.pub, p.impl)
		}
	}
}

type method struct {
	name    string
	call    func() error
	noError bool // the method has no error result to refuse with
}

// unimplementedMethods enumerates every method of every Client service
// interface and of Runtime, each called with zero arguments and a live
// context.
func unimplementedMethods(c uma.Client, rt uma.Runtime) []method {
	var out []method
	add := func(prefix string, v reflect.Value) {
		for i := 0; i < v.NumMethod(); i++ {
			name := prefix + "." + v.Type().Method(i).Name
			fn := v.Method(i)
			// A method without an error result, such as the memory-only
			// Runtime.ReceiverState, has nothing to refuse with.
			if ft := fn.Type(); ft.NumOut() == 0 || ft.Out(ft.NumOut()-1) != reflect.TypeFor[error]() {
				out = append(out, method{name: name, call: func() error { return nil }, noError: true})
				continue
			}
			out = append(out, method{name: name, call: func() error { return callZero(fn) }})
		}
	}
	cv := reflect.ValueOf(c)
	ct := reflect.TypeFor[uma.Client]()
	for i := 0; i < ct.NumMethod(); i++ {
		svc := cv.MethodByName(ct.Method(i).Name).Call(nil)[0]
		add(ct.Method(i).Name, svc)
	}
	add("Runtime", reflect.ValueOf(&rt).Elem())
	return out
}

func callZero(fn reflect.Value) error {
	ctxType := reflect.TypeFor[context.Context]()
	args := make([]reflect.Value, fn.Type().NumIn())
	for i := range args {
		in := fn.Type().In(i)
		if in == ctxType {
			args[i] = reflect.ValueOf(context.Background())
			continue
		}
		args[i] = reflect.Zero(in)
	}
	call := fn.Call
	if fn.Type().IsVariadic() {
		call = fn.CallSlice
	}
	res := call(args)
	last := res[len(res)-1]
	if last.IsNil() {
		return nil
	}
	return last.Interface().(error)
}

func requireCode(t *testing.T, err error, code model.ErrorCode, want string) {
	t.Helper()
	var me *model.Error
	if !errors.As(err, &me) || me.Code != code {
		t.Fatalf("error = %v (%T), want *model.Error %s", err, err, code)
	}
	if want != "" && !strings.Contains(me.SafeMessage, want) {
		t.Fatalf("message = %q, want it to contain %q", me.SafeMessage, want)
	}
}

// Contacts.Link and Contacts.Unlink are part of the Contacts surface and
// implemented: a zero request is invalid, not unsupported.
func TestContactsLinkAndUnlinkImplemented(t *testing.T) {
	e := newEngine(t)
	c, err := e.For(principal)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, m := range unimplementedMethods(c, e.Runtime()) {
		if m.name == "Contacts.Link" || m.name == "Contacts.Unlink" {
			found++
			if !implemented[m.name] {
				t.Fatalf("%s not listed as implemented", m.name)
			}
			requireCode(t, m.call(), model.Validation, "")
		}
	}
	if found != 2 {
		t.Fatal("Contacts surface lacks Link or Unlink")
	}
}

// Each client service exposes exactly its method set with its request and
// result types: stream lifecycle lives on Streams under the stream policy
// actions, and Interactions holds interaction records, responses,
// ephemeral replies and signals.
func TestClientServices(t *testing.T) {
	ctx := reflect.TypeFor[context.Context]()
	acceptance := reflect.TypeFor[model.Acceptance]()
	errType := reflect.TypeFor[error]()
	type sig struct{ in, out []reflect.Type }
	want := map[string]map[string]sig{
		"Streams": {
			"Get":    {[]reflect.Type{ctx, reflect.TypeFor[model.Lookup[model.StreamRef]]()}, []reflect.Type{reflect.TypeFor[model.Stream](), errType}},
			"Start":  {[]reflect.Type{ctx, reflect.TypeFor[model.StartStream]()}, []reflect.Type{acceptance, errType}},
			"Update": {[]reflect.Type{ctx, reflect.TypeFor[model.StreamUpdate]()}, []reflect.Type{acceptance, errType}},
			"End":    {[]reflect.Type{ctx, reflect.TypeFor[model.EndStream]()}, []reflect.Type{acceptance, errType}},
		},
		"Interactions": {
			"Get":       {[]reflect.Type{ctx, reflect.TypeFor[model.Lookup[model.InteractionRef]]()}, []reflect.Type{reflect.TypeFor[model.Interaction](), errType}},
			"List":      {[]reflect.Type{ctx, reflect.TypeFor[model.ListRequest]()}, []reflect.Type{reflect.TypeFor[model.Page[model.Interaction]](), errType}},
			"Respond":   {[]reflect.Type{ctx, reflect.TypeFor[model.Respond]()}, []reflect.Type{acceptance, errType}},
			"Ephemeral": {[]reflect.Type{ctx, reflect.TypeFor[model.Ephemeral]()}, []reflect.Type{acceptance, errType}},
			"Signal":    {[]reflect.Type{ctx, reflect.TypeFor[model.Signal]()}, []reflect.Type{errType}},
		},
	}
	client := reflect.TypeFor[uma.Client]()
	for svc, methods := range want {
		acc, ok := client.MethodByName(svc)
		if !ok {
			t.Errorf("Client has no %s()", svc)
			continue
		}
		it := acc.Type.Out(0)
		got := map[string]bool{}
		for i := 0; i < it.NumMethod(); i++ {
			got[it.Method(i).Name] = true
		}
		for name, s := range methods {
			m, ok := it.MethodByName(name)
			if !ok {
				t.Errorf("%s has no %s", svc, name)
				continue
			}
			if !sameTypes(m.Type, s.in, s.out) {
				t.Errorf("%s.%s = %v, want in %v out %v", svc, name, m.Type, s.in, s.out)
			}
			delete(got, name)
		}
		for name := range got {
			t.Errorf("%s has unexpected method %s", svc, name)
		}
	}
	if model.ActionStreamRead != "stream.read" || model.ActionStreamWrite != "stream.write" {
		t.Errorf("stream policy actions = %q, %q; want stream.read, stream.write", model.ActionStreamRead, model.ActionStreamWrite)
	}
}

func sameTypes(fn reflect.Type, in, out []reflect.Type) bool {
	if fn.NumIn() != len(in) || fn.NumOut() != len(out) {
		return false
	}
	for i, x := range in {
		if fn.In(i) != x {
			return false
		}
	}
	for i, x := range out {
		if fn.Out(i) != x {
			return false
		}
	}
	return true
}
