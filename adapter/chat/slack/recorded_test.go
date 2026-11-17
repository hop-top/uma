package slack

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/testing/xrrtest"
)

// cassettes holds recorded Web API exchanges. See TestRecordedWorkspace to
// record them.
const cassettes = "testdata/cassettes/slack"

type botToken string

func (t botToken) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return ports.Secret{Bytes: []byte(t)}, nil
}

// TestRecordedWorkspace replays recorded exchanges with a Slack workspace,
// on one throwaway channel: a message sent, found again by reconciliation,
// edited, reacted to and the reaction removed, deleted, and the channel
// archived and unarchived (conversations.archive and unarchive, each read
// back with conversations.info). It skips until
// cassettes exist. To record, against a development workspace and a
// channel the app's bot is in (docs/recording/slack.md):
//
//	SLACK_BOT_TOKEN=xoxb-... SLACK_TEAM_ID=T... SLACK_BOT_USER_ID=U... SLACK_CHANNEL_ID=C... \
//	  XRR_MODE=record go test -count=1 -run TestRecordedWorkspace
//
// The IDs are recorded as placeholders and the token is never recorded;
// review the diff of testdata/cassettes before committing.
//
// Reading history is not recorded: its request names the time of the call,
// which a replay cannot match. Nor are interactions and events, which Slack
// sends: see TestRecordedArchiveEvents.
func TestRecordedWorkspace(t *testing.T) {
	mode := os.Getenv("XRR_MODE")
	if _, err := os.Stat(cassettes); err != nil && mode != string(xrrtest.ModeRecord) {
		t.Skip("no recorded Slack cassettes; record with XRR_MODE=record and a development workspace")
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
	token := env("SLACK_BOT_TOKEN", "xoxb-replay")
	team, bot, channel := env("SLACK_TEAM_ID", "T0REPLAY"), env("SLACK_BOT_USER_ID", "U0REPLAY"), env("SLACK_CHANNEL_ID", "C0REPLAY")
	client := xrrtest.NewClient(t, cassettes,
		xrrtest.WithPlaceholder(team, "SLACK_TEAM_ID"),
		xrrtest.WithPlaceholder(bot, "SLACK_BOT_USER_ID"),
		xrrtest.WithPlaceholder(channel, "SLACK_CHANNEL_ID"),
		xrrtest.WithPlaceholder(os.Getenv("SLACK_BOT_ID"), "SLACK_BOT_ID"),
	)
	cfg := Config{TeamID: team, BotUserID: bot}
	inst, err := NewFactory(Options{}).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: Type, Configuration: cfg.Extension(), CredentialRef: "slack",
	}, adapter.Dependencies{Credentials: botToken(token), HTTP: client, Clock: &fixedClock{testNow}})
	if err != nil {
		t.Fatal(err)
	}
	// The attempt is dated well before the recording: the reads then include
	// the message whenever it runs, and their requests stay the same.
	workspaceScenario(t, inst.(*Instance), channel, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
}

// workspaceScenario is what TestRecordedWorkspace records: every call of it
// must hold against Slack and against the scripted workspace alike.
func workspaceScenario(t *testing.T, x *Instance, channel string, attempted time.Time) {
	t.Helper()
	ctx := t.Context()
	room := x.roomRef(channel)
	eff := adapter.Effect{
		Operation: model.OperationRef{Tenant: testTenant, ID: testOp}, Attempt: testAttempt,
		Route: room, Audience: model.Audience{Visibility: model.Public},
	}
	out, err := x.Send(ctx, adapter.SendRequest{Effect: eff, Content: text("uma recorded message <ok> & more")})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	sent := out.References[0]
	eff.Target = &sent

	rec, err := x.Reconcile(ctx, adapter.ReconcileRequest{
		Effect:     adapter.Effect{Operation: eff.Operation, Attempt: eff.Attempt, Route: room, Audience: eff.Audience},
		Capability: model.CapabilitySend, Content: text("uma recorded message <ok> & more"), AttemptedAt: attempted,
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

	if out, err = x.Delete(ctx, adapter.DeleteRequest{Effect: eff}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
	if rec, err = x.Reconcile(ctx, adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityDelete, AttemptedAt: attempted}); err != nil || rec.State != model.ProviderAccepted {
		t.Fatalf("Reconcile of the deletion = %+v, %v", rec, err)
	}

	// The channel is archived, read back, and unarchived again: it ends as it
	// began, open, so the recording leaves the throwaway channel usable.
	room = x.roomRef(channel)
	arch := adapter.Effect{Operation: eff.Operation, Attempt: eff.Attempt, Route: room, Audience: eff.Audience}
	for _, undo := range []bool{false, true} {
		if out, err = x.Archive(ctx, adapter.ArchiveRequest{Effect: arch, Undo: undo}); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("Archive (undo %v) = %+v, %v", undo, out, err)
		}
		if rec, err = x.Reconcile(ctx, adapter.ReconcileRequest{Effect: arch, Capability: model.CapabilityArchive, Undo: undo, AttemptedAt: attempted}); err != nil || rec.State != model.ProviderAccepted {
			t.Fatalf("Reconcile of the archive (undo %v) = %+v, %v", undo, rec, err)
		}
	}
}

// eventFixtures holds the event_callback bodies Slack sent for the
// recorded channel's archive and unarchive, captured from the app's request
// URL or Socket Mode (docs/recording/slack.md). The IDs in them are the
// placeholders TestRecordedArchiveEvents replaces.
const eventFixtures = "testdata/events/slack"

// TestRecordedArchiveEvents replays the channel_archive and
// channel_unarchive events Slack sent while the archive steps of
// TestRecordedWorkspace ran. It skips until the bodies are captured.
func TestRecordedArchiveEvents(t *testing.T) {
	for _, tc := range []struct {
		file     string
		archived bool
	}{{"channel_archive.json", true}, {"channel_unarchive.json", false}} {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(eventFixtures, tc.file))
			if err != nil {
				t.Skip("no captured Slack event; capture it as docs/recording/slack.md says")
			}
			body := strings.NewReplacer(
				"SLACK_TEAM_ID", testTeam, "SLACK_APP_ID", testApp, "SLACK_CHANNEL_ID", testRoom, "SLACK_USER_ID", testPerson,
			).Replace(string(raw))
			x := open(t, testConfig())
			s := &sinkRecorder{}
			req := jsonRequest(nil)
			req.body = []byte(body)
			if res := serve(t, x, s, req); res.Code != http.StatusOK {
				t.Fatalf("answer = %d, want 200", res.Code)
			}
			e := wantOne(t, s)
			if e.Kind != model.EnvelopeConversationState || e.State == nil || e.State.Conversation != x.roomRef(testRoom) || e.State.State.Archived != tc.archived {
				t.Fatalf("envelope = %+v, want the room's conversation state, archived %v", e, tc.archived)
			}
		})
	}
}

// The scenario holds against the scripted workspace too, so a recording
// that fails is Slack's answer, not a slip of the scenario.
func TestRecordedScenarioAgainstTheScriptedWorkspace(t *testing.T) {
	r := newRig(t)
	workspaceScenario(t, r.x, testRoom, testNow)
}
