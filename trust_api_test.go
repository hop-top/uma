package uma_test

import (
	"context"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

// Decisions move the trust counters, which the public methods read and
// reset on the channel the request names, under trust.read and trust.reset.
func TestTrustMethods(t *testing.T) {
	ctx := context.Background()
	clock := portstest.NewFixedClock(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: clock}),
		Authorizer: policy.AuthorizeFunc(func(_ context.Context, a policy.Authorization) (policy.Decision, error) {
			blind := a.Principal.Actor == "blind" && (a.Action == model.ActionTrustRead || a.Action == model.ActionTrustReset || a.Action == model.ActionTrustWrite)
			return policy.Decision{Allow: !blind}, nil
		}),
		Adapters: []adapter.Factory{reactAdapter{textAdapter{sent: make(chan adapter.SendRequest, 4)}}},
		Clock:    clock,
		IDs:      portstest.NewSequenceIDs(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	as := func(actor model.ActorID) uma.Client {
		c, err := e.For(model.Principal{Tenant: "acme", Actor: actor})
		must(err)
		return c
	}
	host, plugin, reviewer := as("host"), as("plugin:triage"), as("reviewer")
	cmd := func(op model.OperationID) model.Command {
		return model.Command{Call: model.Call{Correlation: "t"}, Operation: op}
	}
	ch, err := host.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{Command: cmd("channel"), Channel: model.Channel{
		Ref: model.ChannelRef{Tenant: "acme"}, Type: textType, Configuration: model.Extension{Name: "com.example.text-config", Version: 1},
	}})
	must(err)
	channel, _ := ch.Operation.Result.AsChannel()
	lock := true
	ib, err := host.Configuration().ConfigureInbox(ctx, model.ConfigureInbox{Command: cmd("inbox"), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: "acme"}, Channel: channel, Name: "texts", Destinations: []string{"+15550001"},
		Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
	}})
	must(err)
	inbox, _ := ib.Operation.Result.AsInbox()
	start, err := host.Conversations().Start(ctx, model.StartConversation{
		Command: cmd("start"), Inbox: inbox,
		Recipient: model.ExternalRef{Channel: channel, Namespace: textNS, Kind: "phone", ID: "+15550123"},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
	})
	must(err)

	hold := func(op model.OperationID, key, emoji string) model.Operation {
		c := cmd(op)
		c.Hold = &model.HoldProposal{Reason: "r", TrustKey: key, TrustLabels: map[string]string{"plugin": "triage"}}
		a, err := plugin.Messages().React(ctx, model.Reaction{Command: c, Message: *start.Operation.ResultMessage, Emoji: emoji})
		must(err)
		return a.Operation
	}
	approve := func(op model.Operation) {
		_, err := reviewer.Operations().Approve(ctx, model.DecideHeld{Call: model.Call{Correlation: "d"}, Operation: op.Ref, ExpectedRevision: op.Revision})
		must(err)
	}
	call := model.Call{Correlation: "t"}
	approve(hold("r1", "triage/a", "+1"))
	ops := reviewer.Operations()
	got, err := ops.Trust(ctx, model.TrustLookup{Call: call, Channel: channel, Key: "triage/a"})
	if err != nil || got.Clean != 1 || got.Approvals != 1 || got.Labels["plugin"] != "triage" {
		t.Fatalf("Trust = %+v, %v", got, err)
	}
	if _, err := ops.Trust(ctx, model.TrustLookup{Call: call, Channel: channel, Key: "absent"}); !model.IsCode(err, model.NotFound) {
		t.Fatalf("Trust(absent) = %v, want not found", err)
	}
	approve(hold("r2", "triage/b", "-1"))
	page, err := ops.Trusts(ctx, model.TrustFilter{Call: call, Channel: channel, Page: model.PageRequest{Limit: 1}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Key != "triage/a" || page.Complete || page.Next == "" {
		t.Fatalf("Trusts page 1 = %+v, %v", page, err)
	}
	page, err = ops.Trusts(ctx, model.TrustFilter{Call: call, Channel: channel, Page: model.PageRequest{Limit: 1, After: page.Next}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Key != "triage/b" || !page.Complete {
		t.Fatalf("Trusts page 2 = %+v, %v", page, err)
	}
	filtered, err := ops.Trusts(ctx, model.TrustFilter{Call: call, Channel: channel, Labels: map[string]string{"plugin": "other"}})
	if err != nil || len(filtered.Items) != 0 {
		t.Fatalf("Trusts(other) = %+v, %v", filtered, err)
	}

	reset, err := ops.ResetTrust(ctx, model.ResetTrust{Call: call, Channel: channel, Key: "triage/a"})
	if err != nil || reset.Clean != 0 || reset.Approvals != 1 {
		t.Fatalf("ResetTrust = %+v, %v", reset, err)
	}
	set, err := ops.ResetTrustSet(ctx, model.ResetTrustSet{Call: call, Channel: channel, Keys: []string{"triage/b", "never"}})
	if err != nil || len(set) != 1 || set[0].Key != "triage/b" || set[0].Clean != 0 {
		t.Fatalf("ResetTrustSet = %+v, %v", set, err)
	}

	blind := as("blind").Operations()
	if _, err := blind.Trust(ctx, model.TrustLookup{Call: call, Channel: channel, Key: "triage/a"}); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("Trust without trust.read = %v", err)
	}
	if _, err := blind.ResetTrust(ctx, model.ResetTrust{Call: call, Channel: channel, Key: "triage/a"}); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("ResetTrust without trust.reset = %v", err)
	}
	in := model.ImportTrust{Call: call, Channel: channel, Key: "migrated", Clean: 4, Approvals: 7}
	imported, err := ops.ImportTrust(ctx, in)
	if err != nil || imported.Clean != 4 || imported.Approvals != 7 {
		t.Fatalf("ImportTrust = %+v, %v", imported, err)
	}
	if again, err := ops.ImportTrust(ctx, in); err != nil || again.UpdatedAt != imported.UpdatedAt || again.Approvals != 7 {
		t.Fatalf("ImportTrust again = %+v, %v", again, err)
	}
	if _, err := blind.ImportTrust(ctx, in); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("ImportTrust without trust.write = %v", err)
	}
	in.Key = ""
	if _, err := ops.ImportTrust(ctx, in); !model.IsCode(err, model.Validation) {
		t.Fatalf("ImportTrust(empty key) = %v", err)
	}
	must(e.Close(ctx))
	if _, err := ops.ImportTrust(ctx, in); err == nil {
		t.Fatal("ImportTrust on a closed engine")
	}
	if _, err := ops.Trust(ctx, model.TrustLookup{Call: call, Channel: channel, Key: "triage/a"}); err == nil {
		t.Fatal("Trust on a closed engine")
	}
}
