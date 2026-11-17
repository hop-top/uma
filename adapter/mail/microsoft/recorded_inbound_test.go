package microsoft

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// Recorded inbound mail replays from testdata/cassettes/graph-inbound. The
// receiver reads mail the peer wrote through delta rounds over a fixed
// window (2001-02-01, a day before any real mail the mailbox holds) in
// which the recording creates its own messages: no mail is sent to create
// them (see docs/recording/microsoft-graph.md). To record:
//
//	MS_GRAPH_ACCESS_TOKEN=<token> MS_GRAPH_MAILBOX=<upn> MS_GRAPH_PEER=<address> \
//	  XRR_MODE=record go test -count=1 -run '^TestRecordedGraphInbound$' ./...
const inboundCassetteDir = "testdata/cassettes/graph-inbound"

// inboundClock is the fixed clock of the scenario: the first delta round
// starts one hour before it, so the messages are dated inside that hour.
var inboundClock = time.Date(2001, time.February, 1, 12, 0, 0, 0, time.UTC)

const inboundAttachment = "uma inbound recording attachment\n"

func TestRecordedGraphInbound(t *testing.T) {
	mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode))
	if mode == "" || mode == xrrtest.ModeReplay {
		if _, err := os.Stat(inboundCassetteDir); os.IsNotExist(err) {
			t.Skip("no recorded Graph inbound cassettes yet")
		}
	}
	mailbox, peer := envOr("MS_GRAPH_MAILBOX", "mailbox@example.test"), envOr("MS_GRAPH_PEER", "peer@example.test")
	token := envOr("MS_GRAPH_ACCESS_TOKEN", "replay-token")
	client := xrrtest.NewClient(t, inboundCassetteDir, recordingOptions(mailbox, peer)...)
	var seed func(n int)
	if mode == xrrtest.ModeRecord {
		d := directGraph{t: t, token: token, mailbox: mailbox}
		d.requireEmptyWindow()
		seed = func(n int) { d.seed(n, peer) }
	}
	recordedInbound(t, client, "", mailbox, peer, token, seed)
	if mode == xrrtest.ModeRecord {
		assertNoResidue(t, inboundCassetteDir, mailbox, peer, token)
	}
}

// TestRecordedInboundOverFake runs the scenario against the scripted
// service, so the wiring stays working without cassettes.
func TestRecordedInboundOverFake(t *testing.T) {
	g := newFakeGraph(t)
	const mailbox, peer = "mailbox@example.test", "peer@example.test"
	g.mailbox(mailbox, testChan)
	seed := func(n int) {
		id := "in-" + strconv.Itoa(n)
		m := gMessage{
			ID: id, InternetMessageID: "<uma-inbound-" + strconv.Itoa(n) + "@recording.invalid>", ConversationID: "conv-1",
			Subject:          "uma inbound recording " + strconv.Itoa(n),
			ReceivedDateTime: inboundClock.Add(-time.Duration(60-10*n) * time.Minute),
			From:             &gRecipient{EmailAddress: gEmailAddress{Address: peer}},
			ToRecipients:     []gRecipient{{EmailAddress: gEmailAddress{Address: mailbox}}},
			Body:             &gItemBody{ContentType: "text", Content: "Message " + strconv.Itoa(n) + " of the uma inbound recording."},
		}
		var atts []gAttachment
		if n == 2 {
			atts = []gAttachment{{ODataType: fileType, ID: "att-1", Name: "notes.txt", ContentType: "text/plain", Size: int64(len(inboundAttachment))}}
			g.files["att-1"] = []byte(inboundAttachment)
		}
		g.deliver(mailbox, m, atts...)
	}
	opts := append(recordingOptions(mailbox, peer),
		xrrtest.WithMode(xrrtest.ModePassthrough), xrrtest.WithTransport(g.srv.Client().Transport))
	recordedInbound(t, xrrtest.NewClient(t, t.TempDir(), opts...), g.endpoint(), mailbox, peer, "tok-1", seed)
}

// recordedInbound runs the receiver over the messages seed creates, one
// call per message (1 and 2 before the first round, 3 before the second;
// nil on replay, where the cassettes hold them). The first round lists
// messages 1 and 2 with their bodies and the attachment bytes of message 2,
// the second round follows the delta link and reports only message 3. The
// scenario then replies to message 1 (createReply, update, send), reconciles
// the reply in Sent Items, trashes message 1 and runs a last round, which
// reports nothing new: the delta of a message that left the inbox does not
// reach the sink.
func recordedInbound(t *testing.T, client *http.Client, endpoint, mailbox, peer, token string, seed func(n int)) {
	t.Helper()
	data, _ := json.Marshal(Config{Mailbox: mailbox, Addresses: []string{mailbox}})
	inst, err := NewFactory(Options{Endpoint: endpoint}).Open(t.Context(), model.Channel{
		Ref: testChan, Type: Type, CredentialRef: "recorded",
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
	}, adapter.Dependencies{HTTP: client, Credentials: staticToken(token), Clock: portstest.NewFixedClock(inboundClock)})
	if err != nil {
		t.Fatal(err)
	}
	in := inst.(*instance)
	do := func(n int) {
		if seed != nil {
			seed(n)
		}
	}
	do(1)
	do(2)
	sink := &stagingSink{}
	if _, err := in.round(t.Context(), sink); err != nil {
		t.Fatalf("first round: %v", err)
	}
	first := mail(sink.got)
	if len(first) != 2 {
		t.Fatalf("first round accepted %d messages, want 2", len(first))
	}
	byN := map[int]model.Ingress{}
	for _, g := range first {
		n := subjectNumber(t, g)
		byN[n] = g
		env := g.Envelope
		if env.MessageID == "" || env.Destination != mailbox || env.Kind != model.EnvelopeMessage || env.Sender.ID != peer || env.Sender.ParentID != env.MessageID {
			t.Fatalf("envelope %+v", env)
		}
		if want := "Message " + strconv.Itoa(n) + " of the uma inbound recording."; !strings.Contains(textOf(g), want) {
			t.Fatalf("message %d text %q, want it to hold %q", n, textOf(g), want)
		}
		if env.OccurredAt.Before(inboundClock.Add(-time.Hour)) || env.OccurredAt.After(inboundClock) {
			t.Fatalf("message %d occurred at %v", n, env.OccurredAt)
		}
	}
	if byN[1].Envelope.MessageID == "" || byN[2].Envelope.MessageID == "" || byN[1].Envelope.MessageID == byN[2].Envelope.MessageID {
		t.Fatalf("first round messages %+v", byN)
	}
	if len(stagedRefs(byN[1].Envelope.Content)) != 0 || len(stagedRefs(byN[2].Envelope.Content)) != 1 ||
		len(sink.reqs) != 1 || sink.bodies[0] != inboundAttachment || sink.reqs[0].Metadata.Name != "notes.txt" {
		t.Fatalf("staged %+v bodies %q", sink.reqs, sink.bodies)
	}

	do(3)
	before := len(sink.got)
	if _, err := in.round(t.Context(), sink); err != nil {
		t.Fatalf("second round: %v", err)
	}
	if got := mail(sink.got[before:]); len(got) != 1 || subjectNumber(t, got[0]) != 3 {
		t.Fatalf("second round accepted %d new messages %+v, want message 3 only", len(got), got)
	}

	// The reply: createReply on message 1, the draft updated with the
	// text, the send, and the Sent Items lookup by the attempt's property.
	route := byN[1].Envelope.Sender
	eff := effect(route, "00000000-0000-7000-8000-0000000000b1")
	out, err := in.Send(t.Context(), adapter.SendRequest{Effect: eff, Content: textContent("uma inbound recording reply")})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) == 0 {
		t.Fatalf("reply Send = %+v, %v", out, err)
	}
	if xrrtest.Mode(os.Getenv(xrrtest.EnvMode)) == xrrtest.ModeRecord {
		time.Sleep(15 * time.Second)
	}
	rec, err := in.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: eff})
	if err != nil && model.CodeOf(err) != model.Transient {
		t.Fatalf("reply Reconcile = %+v, %v", rec, err)
	}
	if err == nil && rec.State != model.ProviderAccepted && (rec.State != model.Failed || model.CodeOf(rec.Failure) != model.Transient) {
		t.Fatalf("reply Reconcile = %+v", rec)
	}

	// Message 1 leaves the inbox for Deleted Items; the receiver's next
	// round must not deliver it again.
	eff.Operation.ID = "00000000-0000-7000-8000-0000000000b2"
	tr, err := in.Trash(t.Context(), adapter.TrashRequest{Effect: eff, Items: []model.ExternalRef{model.ProviderMessageRef(testChan, byN[1].Envelope.MessageID)}})
	if err != nil || tr.State != model.ProviderAccepted {
		t.Fatalf("Trash = %+v, %v", tr, err)
	}
	before = len(sink.got)
	if _, err := in.round(t.Context(), sink); err != nil {
		t.Fatalf("last round: %v", err)
	}
	if n := len(mail(sink.got[before:])); n != 0 {
		t.Fatalf("last round accepted %d messages, want none", n)
	}
}

// mail is the messages among the accepted ingress: the state reports of
// the watched folders arrive through the same sink.
func mail(all []model.Ingress) []model.Ingress {
	var out []model.Ingress
	for _, in := range all {
		if in.Envelope.State == nil {
			out = append(out, in)
		}
	}
	return out
}

// subjectNumber is the N of "uma inbound recording N" in the message's
// payload.
func subjectNumber(t *testing.T, in model.Ingress) int {
	t.Helper()
	var p messagePayload
	if err := json.Unmarshal(in.Envelope.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(p.Subject, "uma inbound recording ")
	n, err := strconv.Atoi(rest)
	if !ok || err != nil {
		t.Fatalf("subject %q", p.Subject)
	}
	return n
}

func textOf(in model.Ingress) string {
	var out []string
	for _, p := range in.Envelope.Content.Parts {
		if p.Kind == model.PartText {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

// directGraph calls Graph outside the recorded client, to place the
// messages of the window before a recording. Nothing it does reaches a
// cassette.
type directGraph struct {
	t              *testing.T
	token, mailbox string
}

func (d directGraph) call(method, path string, body any) []byte {
	d.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(d.t.Context(), method, defaultEndpoint+"/users/"+url.PathEscape(d.mailbox)+path, rd)
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, strings.SplitN(path, "?", 2)[0], err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		d.t.Fatalf("%s %s answered %d", method, strings.SplitN(path, "?", 2)[0], res.StatusCode)
	}
	return b
}

// requireEmptyWindow fails unless the inbox holds nothing dated in the
// window: a recording over earlier messages would list them too.
func (d directGraph) requireEmptyWindow() {
	d.t.Helper()
	q := url.Values{"$filter": {"receivedDateTime ge 2001-02-01T00:00:00Z and receivedDateTime lt 2001-02-02T00:00:00Z"}, "$select": {"id"}}
	var page gMessagePage
	if err := json.Unmarshal(d.call("GET", "/mailFolders/inbox/messages?"+q.Encode(), nil), &page); err != nil {
		d.t.Fatal(err)
	}
	if len(page.Value) != 0 {
		d.t.Fatalf("the inbox holds %d messages dated 2001-02-01: move them to Deleted Items before recording", len(page.Value))
	}
}

// seed creates message n in the inbox as received mail from peer (see the
// extended properties in docs/recording/microsoft-graph.md); message 2 has
// a small file attachment. No mail is sent.
func (d directGraph) seed(n int, peer string) {
	d.t.Helper()
	num := strconv.Itoa(n)
	at := inboundClock.Add(-time.Duration(60-10*n) * time.Minute).Format(time.RFC3339)
	msg := map[string]any{
		"subject":           "uma inbound recording " + num,
		"body":              map[string]any{"contentType": "text", "content": "Message " + num + " of the uma inbound recording."},
		"from":              map[string]any{"emailAddress": map[string]any{"address": peer}},
		"sender":            map[string]any{"emailAddress": map[string]any{"address": peer}},
		"toRecipients":      []any{map[string]any{"emailAddress": map[string]any{"address": d.mailbox}}},
		"internetMessageId": "<uma-inbound-" + num + "@recording.invalid>",
		"singleValueExtendedProperties": []any{
			map[string]any{"id": "Integer 0x0E07", "value": "0"},
			map[string]any{"id": "SystemTime 0x0E06", "value": at},
			map[string]any{"id": "SystemTime 0x0039", "value": at},
		},
	}
	if n == 2 {
		msg["attachments"] = []any{map[string]any{
			"@odata.type": fileAttachmentType, "name": "notes.txt", "contentType": "text/plain",
			"contentBytes": "dW1hIGluYm91bmQgcmVjb3JkaW5nIGF0dGFjaG1lbnQK",
		}}
	}
	d.call("POST", "/mailFolders/inbox/messages", msg)
}
