package microsoft

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// Recorded refusals replay from testdata/cassettes/graph-errors: what Graph
// answers to requests that name a message, a folder, a delta token, a
// credential or a subscription it does not accept. They read, and ask for
// changes Graph refuses, so nothing in the mailbox changes. The one message
// they need to derive an unknown ID from is read from the history window
// (see TestRecordedGraphHistory); to record:
//
//	MS_GRAPH_ACCESS_TOKEN=<token> MS_GRAPH_MAILBOX=<upn> MS_GRAPH_PEER=<address> \
//	  XRR_MODE=record go test -count=1 -run '^TestRecordedGraphErrors$' ./...
const errorsCassetteDir = "testdata/cassettes/graph-errors"

// errorsClock is a day in an empty window, so the delta rounds list no
// mail, and long before the present, so Graph refuses a subscription for
// its expiry before it looks at the notification URL.
var errorsClock = time.Date(2001, time.March, 1, 12, 0, 0, 0, time.UTC)

func TestRecordedGraphErrors(t *testing.T) {
	mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode))
	if mode == "" || mode == xrrtest.ModeReplay {
		if _, err := os.Stat(errorsCassetteDir); os.IsNotExist(err) {
			t.Skip("no recorded Graph error cassettes yet")
		}
	}
	mailbox, peer := envOr("MS_GRAPH_MAILBOX", "mailbox@example.test"), envOr("MS_GRAPH_PEER", "peer@example.test")
	token := envOr("MS_GRAPH_ACCESS_TOKEN", "replay-token")
	client := xrrtest.NewClient(t, errorsCassetteDir, recordingOptions(mailbox, peer)...)
	recordedErrors(t, client, "", mailbox, token)
	if mode == xrrtest.ModeRecord {
		assertNoResidue(t, errorsCassetteDir, mailbox, peer, token)
	}
}

// recordedCreds answers the Graph token and the notification secret.
type recordedCreds struct{ token string }

func (c recordedCreds) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	if r.Purpose == PurposeNotifications {
		return ports.Secret{Bytes: []byte("recorded notification secret")}, nil
	}
	return ports.OAuth2{AccessToken: c.token}, nil
}

// unknownOf is id with one character changed: an ID of the same shape that
// names nothing, which Graph answers 404 rather than 400.
func unknownOf(id string) string {
	i := len(id) - 8
	b := []byte(id)
	b[i] = 'A'
	if id[i] == 'A' {
		b[i] = 'B'
	}
	return string(b)
}

func recordedErrors(t *testing.T, client *http.Client, endpoint, mailbox, token string) {
	t.Helper()
	data, _ := json.Marshal(Config{Mailbox: mailbox, Addresses: []string{mailbox}})
	open := func(creds ports.Credentials, cfg []byte, protocol adapter.ProtocolState) *instance {
		t.Helper()
		inst, err := NewFactory(Options{Endpoint: endpoint}).Open(t.Context(), model.Channel{
			Ref: testChan, Type: Type, CredentialRef: "recorded",
			Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: cfg},
		}, adapter.Dependencies{HTTP: client, Credentials: creds, Protocol: protocol, Clock: portstest.NewFixedClock(errorsClock)})
		if err != nil {
			t.Fatal(err)
		}
		return inst.(*instance)
	}
	in := open(recordedCreds{token}, data, nil)
	ctx := t.Context()

	// A real message of the history window, to derive IDs from.
	page, err := in.History(ctx, adapter.HistoryRequest{
		Since: historySince, Until: historyUntil, Limit: 1,
	})
	if err != nil || len(page.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v", page, err)
	}
	real := page.Envelopes[0].MessageID
	unknown := unknownOf(real)
	eff := effect(model.ExternalRef{Channel: testChan, Namespace: IdentityNamespace, Kind: refKindAddress, ID: "peer@example.test"}, "00000000-0000-7000-8000-0000000000d1")
	refused := func(name string, out adapter.Outcome, err error, code model.ErrorCode) {
		t.Helper()
		if err != nil || out.State != model.Rejected || out.Failure == nil || model.CodeOf(out.Failure) != code {
			t.Fatalf("%s = %+v, %v; want rejected with %s", name, out, err, code)
		}
	}

	// A well-formed ID that names nothing: 404 ErrorItemNotFound.
	items := []model.ExternalRef{model.ProviderMessageRef(testChan, unknown)}
	out, err := in.SetReadState(ctx, adapter.ReadStateRequest{Effect: eff, Items: items, Read: true})
	refused("read on an unknown message", out, err, model.NotFound)
	out, err = in.Reconcile(ctx, adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityReadState, Items: items, Read: true})
	refused("reconcile read on an unknown message", out, err, model.NotFound)
	// The same ID as a destination folder: GET of the folder, 404.
	out, err = in.Reconcile(ctx, adapter.ReconcileRequest{
		Effect: eff, Capability: model.CapabilityMove, Items: []model.ExternalRef{model.ProviderMessageRef(testChan, real)},
		Destination: model.MoveDestination{ID: unknown},
	})
	refused("reconcile move to an unknown folder", out, err, model.NotFound)
	// An ID of no valid shape: 400 ErrorInvalidIdMalformed.
	out, err = in.SetReadState(ctx, adapter.ReadStateRequest{Effect: eff, Items: []model.ExternalRef{model.ProviderMessageRef(testChan, "not-a-message-id")}, Read: true})
	refused("read on a malformed ID", out, err, model.Permanent)

	// A delta link whose token Graph cannot read: 400 "Badly formed
	// token", an error of the round (an expired token is a 410, which
	// restarts the round; see TestDeltaRestartsOnExpiredToken).
	p := &memProtocol{}
	link := in.c.api(in.cfg.mailboxPath()+"/mailFolders('inbox')/messages/delta", url.Values{"$deltatoken": {"corrupt"}})
	st, _ := json.Marshal(receiverState{DeltaLink: link})
	if err := p.CompareAndSwap(ctx, protocolKey, "", model.Extension{Name: stateSchema, Version: stateVersion, Data: st}); err != nil {
		t.Fatal(err)
	}
	if _, err := open(recordedCreds{token}, data, p).round(ctx, &testSink{}); model.CodeOf(err) != model.Permanent {
		t.Fatalf("round on a corrupt delta token = %v, want permanent", err)
	}

	// A token Graph does not accept: 401, again after a fresh resolve.
	if _, err := open(recordedCreds{"not-a-token"}, data, nil).round(ctx, &testSink{}); model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("round with a rejected token = %v, want unauthorized", err)
	}

	// A subscription Graph refuses (400 InvalidRequest): the round goes on
	// with delta tracking and lists the empty window.
	sub, _ := json.Marshal(Config{Mailbox: mailbox, Addresses: []string{mailbox}, NotificationURL: "https://uma-recording.invalid/graph"})
	sink := &testSink{}
	if _, err := open(recordedCreds{token}, sub, nil).round(ctx, sink); err != nil {
		t.Fatalf("round with a refused subscription: %v", err)
	}
	if len(mail(sink.got)) != 0 {
		t.Fatalf("empty window delivered %d messages", len(mail(sink.got)))
	}
}
