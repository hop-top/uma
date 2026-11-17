package ingress_test

import (
	"context"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// configCase drives one config row through Configuration.ConfigureInbox.
func configCase(t *testing.T, c lcCase) {
	h := newHarness(t, chatType)
	sel := model.Selection{Mode: c.Mode, LockToSingle: c.Flag, LifecycleRules: c.LifecycleRules}
	in, err := h.ConfigureOn(h.Channel.Ref, sel, dest)
	if c.Expect.Outcome == "reject" {
		requireCode(t, err, expectedCode(c), "")
		if got := all(h, store.Reader.Inboxes, model.Filter{}); len(got) != 0 {
			t.Fatalf("rejected configuration stored %d inboxes", len(got))
		}
		return
	}
	if err != nil {
		t.Fatalf("ConfigureInbox: %v", err)
	}
	if in.Selection.Mode != c.Mode || (in.Selection.LockToSingle == nil) != (c.Flag == nil) ||
		(c.Flag != nil && *in.Selection.LockToSingle != *c.Flag) || in.Selection.LifecycleRules != c.LifecycleRules {
		t.Fatalf("stored selection %+v, want %+v", in.Selection, sel)
	}
}

func TestConfigureInboxRules(t *testing.T) {
	h := newHarness(t, chatType)
	missing := model.ChannelRef{Tenant: tenantA, ID: h.ID()}
	cases := map[string]struct {
		ch   model.ChannelRef
		sel  model.Selection
		code model.ErrorCode
		msg  string
	}{
		"agent context flag":  {h.Channel.Ref, model.Selection{Mode: model.ModePerAgentContext, LockToSingle: enginetest.Flag(true)}, model.Validation, "selection.lock_to_single"},
		"unregistered policy": {h.Channel.Ref, model.Selection{Mode: model.ModePerRoom, Policy: "custom"}, model.Validation, "selection.policy"},
		"unknown mode":        {h.Channel.Ref, model.Selection{Mode: "per_planet"}, model.Validation, "selection.mode"},
		"channel missing":     {missing, model.Selection{Mode: model.ModePerRoom}, model.NotFound, ""},
		"foreign channel":     {model.ChannelRef{Tenant: tenantB, ID: h.Channel.Ref.ID}, model.Selection{Mode: model.ModePerRoom}, model.NotFound, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := h.ConfigureOn(tc.ch, tc.sel, dest)
			requireCode(t, err, tc.code, tc.msg)
		})
	}
}

func TestConfigureInboxUpdate(t *testing.T) {
	cfg := func(c *engine.Config) { c.Selectors["custom"] = ingress.ScopedSelector{} }
	h := newHarness(t, chatType, cfg)
	in := h.Configure(perContact(true), dest)
	enginetest.OpSeq++
	req := model.ConfigureInbox{
		Command: model.Command{Call: model.Call{Correlation: "corr"}, Operation: "op-update", ExpectedRevision: in.Revision},
		Inbox:   in,
	}
	req.Inbox.Selection = perContact(false)
	if _, err := h.Client.Configuration().ConfigureInbox(h.Ctx, req); err != nil {
		t.Fatalf("update: %v", err)
	}
	var got model.Inbox
	h.View(func(r store.Reader) error {
		var err error
		got, err = r.GetInbox(h.Ctx, in.Ref)
		return err
	})
	if *got.Selection.LockToSingle || got.Revision == in.Revision {
		t.Fatalf("updated inbox %+v", got)
	}
	// Repeating the command replays it, although its revision has moved on.
	acc, err := h.Client.Configuration().ConfigureInbox(h.Ctx, req)
	if err != nil || !acc.Replayed {
		t.Fatalf("replay = %+v, %v", acc, err)
	}
	// A new command at the stale revision conflicts.
	req.Operation = "op-stale"
	_, err = h.Client.Configuration().ConfigureInbox(h.Ctx, req)
	requireCode(t, err, model.Conflict, "")
	// An inbox never moves channel.
	req.Operation = "op-move"
	req.ExpectedRevision = got.Revision
	req.Inbox.Channel = h.PutChannel(chatType).Ref
	_, err = h.Client.Configuration().ConfigureInbox(h.Ctx, req)
	requireCode(t, err, model.Validation, "never moves")
	// A registered policy is accepted.
	if _, err := h.ConfigureOn(h.Channel.Ref, model.Selection{Mode: model.ModePerRoom, Policy: "custom"}, "room-dest"); err != nil {
		t.Fatalf("registered policy: %v", err)
	}
}

func TestConfigureInboxAuthorizes(t *testing.T) {
	deny := func(c *engine.Config) {
		c.Authorizer = policy.AuthorizeFunc(func(_ context.Context, a policy.Authorization) (policy.Decision, error) {
			if a.Action == model.ActionChannelReceive {
				return policy.Decision{Allow: true}, nil
			}
			if a.Action != model.ActionInboxConfigure || a.Resource.Kind != model.KindInbox || a.Destination == nil ||
				a.Destination.Kind != model.KindChannel {
				t.Errorf("authorization %+v", a)
			}
			return policy.Decision{}, nil
		})
	}
	h := newHarness(t, chatType, deny)
	_, err := h.ConfigureOn(h.Channel.Ref, model.Selection{Mode: model.ModePerRoom}, dest)
	requireCode(t, err, model.Unauthorized, "")
	if got := all(h, store.Reader.Inboxes, model.Filter{}); len(got) != 0 {
		t.Fatalf("denied configuration stored %d inboxes", len(got))
	}
}

func TestConfigureInboxAfterClose(t *testing.T) {
	h := newHarness(t, chatType)
	if err := h.E.Close(h.Ctx); err != nil {
		t.Fatal(err)
	}
	_, err := h.ConfigureOn(h.Channel.Ref, model.Selection{Mode: model.ModePerRoom}, dest)
	requireCode(t, err, model.Unavailable, "engine closed")
}

// Per-agent-context mode is configurable like the other scoped modes: no
// flag and no rule set.
func TestConfigureInboxPerAgentContextAccepted(t *testing.T) {
	h := newHarness(t, chatType)
	in, err := h.ConfigureOn(h.Channel.Ref, model.Selection{Mode: model.ModePerAgentContext}, dest)
	if err != nil {
		t.Fatalf("ConfigureInbox: %v", err)
	}
	if in.Selection.Mode != model.ModePerAgentContext || in.Selection.LockToSingle != nil || in.Selection.LifecycleRules != "" {
		t.Fatalf("stored selection %+v, want per-agent-context alone", in.Selection)
	}
}
