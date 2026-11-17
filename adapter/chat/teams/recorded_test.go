package teams

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/testing/xrrtest"
)

// cassettes holds recorded Bot Framework exchanges. See TestRecordedBot to
// record them.
const cassettes = "testdata/cassettes/teams"

// connectorToken is a Bot Connector access token the recording shell
// minted; the adapter asks the host for it as ports.OAuth2.
type connectorToken string

func (t connectorToken) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return ports.OAuth2{AccessToken: string(t)}, nil
}

// TestRecordedBot replays recorded exchanges with the Bot Framework for a
// bot installed for one test user: the signing keys read, a personal chat
// created and a message sent in it, the message edited, the 👍 reaction
// added and removed, a typing indicator sent, and the message deleted. It
// skips until cassettes exist. To record (docs/recording/teams.md):
//
//	TEAMS_CONNECTOR_TOKEN=... TEAMS_APP_ID=... TEAMS_TENANT_ID=... TEAMS_SERVICE_URL=https://smba.trafficmanager.net/teams/ \
//	  TEAMS_USER_ID=29:... TEAMS_CONVERSATION_ID=a:... XRR_MODE=record go test -count=1 -run TestRecordedBot
//
// The IDs and the service URL are recorded as placeholders and the token is
// never recorded; review the diff of testdata/cassettes before committing.
// TEAMS_CONVERSATION_ID is optional: the personal chat's ID, which the first
// recording returns from the conversation creation.
//
// Activities Teams posts to the bot (messages, edits, reactions, invokes)
// are not recorded: they arrive from Teams, signed with a token no replay
// can produce.
func TestRecordedBot(t *testing.T) {
	mode := os.Getenv("XRR_MODE")
	if _, err := os.Stat(cassettes); err != nil && mode != string(xrrtest.ModeRecord) {
		t.Skip("no recorded Teams cassettes; record with XRR_MODE=record and a test tenant")
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
	token := env("TEAMS_CONNECTOR_TOKEN", "replay-token")
	app := env("TEAMS_APP_ID", "00000000-0000-4000-8000-00000000a99a")
	tenant := env("TEAMS_TENANT_ID", "00000000-0000-4000-8000-00000000e99e")
	service := env("TEAMS_SERVICE_URL", "https://teams-service.invalid/teams/")
	user := env("TEAMS_USER_ID", "29:1replay-user")
	// The personal chat's ID is only known once Teams created it; set it
	// when re-recording so it is recorded as a placeholder as well.
	conversation := os.Getenv("TEAMS_CONVERSATION_ID")
	client := xrrtest.NewClient(t, cassettes,
		xrrtest.WithPlaceholder(app, "TEAMS_APP_ID"),
		xrrtest.WithPlaceholder(tenant, "TEAMS_TENANT_ID"),
		xrrtest.WithPlaceholder(strings.TrimRight(strings.TrimPrefix(service, "https://"), "/"), "TEAMS_SERVICE_HOST_AND_PATH"),
		xrrtest.WithPlaceholder(user, "TEAMS_USER_ID"),
		xrrtest.WithPlaceholder(conversation, "a:TEAMS_CONVERSATION_ID"),
	)
	cfg := Config{AppID: app, TenantID: tenant, ServiceURL: service}
	inst, err := NewFactory(Options{}).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: Type, Configuration: cfg.Extension(), CredentialRef: "teams",
	}, adapter.Dependencies{Credentials: connectorToken(token), HTTP: client, Clock: &fixedClock{testNow}})
	if err != nil {
		t.Fatal(err)
	}
	botScenario(t, inst.(*Instance), user, DefaultOpenIDMetadataURL)
}

// botScenario is what TestRecordedBot records: every call of it must hold
// against the Bot Framework and against the stand-in alike.
func botScenario(t *testing.T, x *Instance, user, metadataURL string) {
	t.Helper()
	ctx := t.Context()
	keys, err := fetchKeys(ctx, x.http, metadataURL)
	if err != nil {
		t.Fatalf("signing keys: %v", err)
	}
	endorsed := false
	for _, k := range keys {
		endorsed = endorsed || slices.Contains(k.endorsements, TeamsChannelID)
	}
	if !endorsed {
		t.Fatal("no signing key is endorsed for msteams")
	}

	route := x.userRef(user)
	eff := adapter.Effect{
		Operation: model.OperationRef{Tenant: testTenant, ID: testOp}, Attempt: testAttempt,
		Route: route, Audience: model.Audience{Visibility: model.Public},
	}
	out, err := x.Send(ctx, adapter.SendRequest{Effect: eff, Content: text("uma recorded message <ok> & more")})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	sent := out.References[0]
	eff.Target = &sent

	if out, err = x.Edit(ctx, adapter.EditRequest{Effect: eff, Content: text("uma recorded edit")}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	for _, remove := range []bool{false, true} {
		if out, err = x.React(ctx, adapter.ReactRequest{Effect: eff, Emoji: "👍", Remove: remove}); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("React (remove %v) = %+v, %v", remove, out, err)
		}
	}
	if err := x.Signal(ctx, adapter.SignalRequest{Route: route, Kind: "typing", Audience: eff.Audience}); err != nil {
		t.Fatalf("Signal = %v", err)
	}
	if out, err = x.Delete(ctx, adapter.DeleteRequest{Effect: eff}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
}

// The scenario holds against the stand-in too, so a recording that fails
// is the Bot Framework's answer, not a slip of the scenario.
func TestRecordedScenarioAgainstTheStandIn(t *testing.T) {
	r := newRig(t)
	r.f.install(testPrefix, testUser)
	botScenario(t, r.x, testUser, fakeMetadataURL)
}
