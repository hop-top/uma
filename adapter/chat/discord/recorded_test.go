package discord

import (
	"context"
	"os"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/testing/xrrtest"
)

// cassettes holds recorded REST exchanges. See TestRecordedBot to record
// them.
const cassettes = "testdata/cassettes/discord"

// cassettesArchive holds the recorded exchanges of the thread archive.
const cassettesArchive = "testdata/cassettes/discord-archive"

type botToken string

func (t botToken) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return ports.Secret{Bytes: []byte(t)}, nil
}

// TestRecordedBot replays recorded exchanges with Discord's REST API, in
// one throwaway channel of a private test guild: a message sent, found
// again by reconciliation, edited, reacted to and the reaction removed, a
// typing indicator, a thread's member added and removed, and the message
// deleted; and, as the subtest "thread archive", the thread archived and
// unarchived (cassettesArchive, its own, so the recordings of the rest
// replay as before). It skips until cassettes exist. To record, with a bot
// application in a private guild (docs/recording/discord.md):
//
//	DISCORD_BOT_TOKEN=... DISCORD_APPLICATION_ID=... DISCORD_BOT_USER_ID=... \
//	DISCORD_CHANNEL_ID=... DISCORD_THREAD_ID=... DISCORD_PARENT_ID=... DISCORD_MEMBER_ID=... \
//	  XRR_MODE=record go test -count=1 -run TestRecordedBot
//
// The IDs are recorded as placeholders and the token is never recorded;
// review the diff of testdata/cassettes before committing.
//
// The gateway is not recorded (a WebSocket session is not an HTTP
// exchange; THREAD_UPDATE, the report of the archive, is a hand-written
// fixture in state_test.go), nor are interactions, which Discord sends, nor
// history, whose request names the time of the call.
func TestRecordedBot(t *testing.T) {
	mode := os.Getenv("XRR_MODE")
	if _, err := os.Stat(cassettes); err != nil && mode != string(xrrtest.ModeRecord) {
		if _, err := os.Stat(cassettesArchive); err != nil {
			t.Skip("no recorded Discord cassettes; record with XRR_MODE=record and a test guild")
		}
	}
	env := func(k, replay string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		if mode == string(xrrtest.ModeRecord) {
			t.Fatalf("recording needs %s", k)
		}
		return replay
	}
	token := env("DISCORD_BOT_TOKEN", "replay-token")
	app, bot := env("DISCORD_APPLICATION_ID", "1000000000000000001"), env("DISCORD_BOT_USER_ID", "1000000000000000002")
	channel, thread := env("DISCORD_CHANNEL_ID", "1000000000000000003"), env("DISCORD_THREAD_ID", "1000000000000000004")
	parent, member := env("DISCORD_PARENT_ID", "1000000000000000005"), env("DISCORD_MEMBER_ID", "1000000000000000006")
	open := func(t *testing.T, dir string) *Instance {
		client := xrrtest.NewClient(t, dir,
			xrrtest.WithPlaceholder(app, "DISCORD_APPLICATION_ID"),
			xrrtest.WithPlaceholder(bot, "DISCORD_BOT_USER_ID"),
			xrrtest.WithPlaceholder(channel, "DISCORD_CHANNEL_ID"),
			xrrtest.WithPlaceholder(thread, "DISCORD_THREAD_ID"),
			xrrtest.WithPlaceholder(parent, "DISCORD_PARENT_ID"),
			xrrtest.WithPlaceholder(member, "DISCORD_MEMBER_ID"),
		)
		cfg := Config{ApplicationID: app, BotUserID: bot}
		inst, err := NewFactory().Open(t.Context(), model.Channel{
			Ref: testChannel, Type: Type, Configuration: cfg.Extension(), CredentialRef: "discord",
		}, adapter.Dependencies{Credentials: botToken(token), HTTP: client, Clock: &fixedClock{testNow}})
		if err != nil {
			t.Fatal(err)
		}
		return inst.(*Instance)
	}
	s := scenario{channel: channel, parent: parent, thread: thread, member: member}
	// The attempt is dated well before the recording: the reads then include
	// the message whenever it runs, and their requests stay the same.
	attempted := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := os.Stat(cassettes); err == nil || mode == string(xrrtest.ModeRecord) {
		botScenario(t, open(t, cassettes), s, attempted)
	}
	t.Run("thread archive", func(t *testing.T) {
		if _, err := os.Stat(cassettesArchive); err != nil && mode != string(xrrtest.ModeRecord) {
			t.Skip("no recorded Discord thread archive cassettes; record with XRR_MODE=record and a test guild")
		}
		archiveScenario(t, open(t, cassettesArchive), s, attempted)
	})
}

type scenario struct{ channel, parent, thread, member string }

// botScenario is what TestRecordedBot records: every call of it must hold
// against Discord and against the stand-in alike.
func botScenario(t *testing.T, x *Instance, s scenario, attempted time.Time) {
	t.Helper()
	ctx := t.Context()
	room := x.roomRef(s.channel)
	eff := adapter.Effect{
		Operation: model.OperationRef{Tenant: testTenant, ID: testOp}, Attempt: testAttempt,
		Route: room, Audience: model.Audience{Visibility: model.Public},
	}
	body := "uma recorded message **ok** <@" + s.member + ">"
	out, err := x.Send(ctx, adapter.SendRequest{Effect: eff, Content: text(body)})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	sent := out.References[0]
	eff.Target = &sent

	rec, err := x.Reconcile(ctx, adapter.ReconcileRequest{
		Effect:     adapter.Effect{Operation: eff.Operation, Attempt: eff.Attempt, Route: room, Audience: eff.Audience},
		Capability: model.CapabilitySend, Content: text(body), AttemptedAt: attempted,
	})
	if err != nil || rec.State != model.ProviderAccepted || len(rec.References) != 1 || rec.References[0] != sent {
		t.Fatalf("Reconcile of the send = %+v, %v; want the message found", rec, err)
	}
	edited := text("uma recorded edit")
	if out, err = x.Edit(ctx, adapter.EditRequest{Effect: eff, Content: edited}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	if rec, err = x.Reconcile(ctx, adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityEdit, Content: edited, AttemptedAt: attempted}); err != nil || rec.State != model.ProviderAccepted {
		t.Fatalf("Reconcile of the edit = %+v, %v", rec, err)
	}
	for _, remove := range []bool{false, true} {
		if out, err = x.React(ctx, adapter.ReactRequest{Effect: eff, Emoji: "👍", Remove: remove}); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("React (remove %v) = %+v, %v", remove, out, err)
		}
		if rec, err = x.Reconcile(ctx, adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityReact, Emoji: "👍", Remove: remove, AttemptedAt: attempted}); err != nil || rec.State != model.ProviderAccepted {
			t.Fatalf("Reconcile of the reaction (remove %v) = %+v, %v", remove, rec, err)
		}
	}
	if err := x.Signal(ctx, adapter.SignalRequest{Route: room, Kind: "typing", Audience: eff.Audience}); err != nil {
		t.Fatalf("Signal = %v", err)
	}
	threadEff := eff
	threadEff.Route, threadEff.Target = x.threadRef(s.parent, s.thread), nil
	who := model.ExternalRef{Channel: testChannel, Namespace: UserNamespace, Kind: string(model.KindIdentity), ID: s.member}
	for _, remove := range []bool{false, true} {
		if out, err = x.ChangeMembership(ctx, adapter.MembershipRequest{Effect: threadEff, Participant: who, Remove: remove}); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("ChangeMembership (remove %v) = %+v, %v", remove, out, err)
		}
		if rec, err = x.Reconcile(ctx, adapter.ReconcileRequest{Effect: threadEff, Capability: model.CapabilityMembership, Participant: who, Remove: remove, AttemptedAt: attempted}); err != nil || rec.State != model.ProviderAccepted {
			t.Fatalf("Reconcile of the membership (remove %v) = %+v, %v", remove, rec, err)
		}
	}
	if out, err = x.Delete(ctx, adapter.DeleteRequest{Effect: eff}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
	if rec, err = x.Reconcile(ctx, adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityDelete, AttemptedAt: attempted}); err != nil || rec.State != model.ProviderAccepted {
		t.Fatalf("Reconcile of the deletion = %+v, %v", rec, err)
	}
}

// archiveScenario is what the "thread archive" subtest records: the thread
// archived and unarchived again, so it ends as it began (a thread archived
// refuses members and messages), each change read back (Get Channel).
func archiveScenario(t *testing.T, x *Instance, s scenario, attempted time.Time) {
	t.Helper()
	ctx := t.Context()
	eff := adapter.Effect{
		Operation: model.OperationRef{Tenant: testTenant, ID: testOp}, Attempt: testAttempt,
		Route: x.threadRef(s.parent, s.thread), Audience: model.Audience{Visibility: model.Public},
	}
	for _, undo := range []bool{false, true} {
		out, err := x.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Undo: undo})
		if err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("Archive (undo %v) = %+v, %v", undo, out, err)
		}
		rec, err := x.Reconcile(ctx, adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityArchive, Undo: undo, AttemptedAt: attempted})
		if err != nil || rec.State != model.ProviderAccepted {
			t.Fatalf("Reconcile of the archive (undo %v) = %+v, %v", undo, rec, err)
		}
	}
}

// The scenario holds against the stand-in too, so a recording that fails
// is Discord's answer, not a slip of the scenario.
func TestRecordedScenarioAgainstTheStandIn(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "seed", "")
	r.f.thread("acct1", testRoom, testThread)
	s := scenario{channel: testRoom, parent: testRoom, thread: testThread, member: testOther}
	botScenario(t, r.x, s, testNow.Add(-time.Hour))
	archiveScenario(t, r.x, s, testNow.Add(-time.Hour))
	if r.acct.channels[testThread].archived {
		t.Fatal("the thread was left archived")
	}
}
