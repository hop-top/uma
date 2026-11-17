package whatsapp_test

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/testing/xrrtest"
)

// cassetteDir holds exchanges recorded against the Cloud API.
const cassetteDir = "testdata/cassettes/meta"

// Recording reads, from the environment only in record mode:
//
//	WHATSAPP_ACCESS_TOKEN     access token of the test business account
//	WHATSAPP_PHONE_NUMBER_ID  the test business phone number's ID
//	WHATSAPP_RECIPIENT        a recipient allowed on the test number
//
// and sends the hello_world template, which test numbers can send at any
// time:
//
//	XRR_MODE=record go test ./whatsapp -run TestRecorded
//	git diff testdata/cassettes   # review: no token, no real numbers
//	go test ./whatsapp -run TestRecorded
//
// The recipient is replaced in every form Meta writes it (+E.164, digits
// only as in wa_id, percent-encoded), and the phone-bearing fields of
// answers (input, wa_id, recipient_id, from, display_phone_number) are
// replaced by name, since Meta may resolve a different wa_id than the
// number sent. A recording fails if a real value survives in a cassette.
//
// Replay needs none of them and skips while no cassette is recorded.
func recordedInstance(t *testing.T) (*whatsapp.Instance, adapter.Effect) {
	t.Helper()
	pnid, recipient, token := "PHONE_NUMBER_ID", "RECIPIENT", "replay-token"
	switch mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode)); mode {
	case xrrtest.ModeRecord, xrrtest.ModePassthrough:
		pnid, recipient, token = os.Getenv("WHATSAPP_PHONE_NUMBER_ID"), os.Getenv("WHATSAPP_RECIPIENT"), os.Getenv("WHATSAPP_ACCESS_TOKEN")
		if pnid == "" || recipient == "" || token == "" {
			t.Fatalf("%s=%s needs WHATSAPP_ACCESS_TOKEN, WHATSAPP_PHONE_NUMBER_ID and WHATSAPP_RECIPIENT", xrrtest.EnvMode, mode)
		}
	default:
		if files, _ := filepath.Glob(filepath.Join(cassetteDir, "*.yaml")); len(files) == 0 {
			t.Skip("no recorded cassettes yet; record with " + xrrtest.EnvMode + "=record")
		}
	}
	client := xrrtest.NewClient(t, cassetteDir, recordingOptions(pnid, recipient)...)
	if mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode)); mode == xrrtest.ModeRecord {
		t.Cleanup(func() { assertNoResidue(t, cassetteDir, pnid, recipient, token) })
	}
	cr := newCreds()
	cr.values[whatsapp.PurposeGraph] = ports.Secret{Bytes: []byte(token)}
	inst, err := whatsapp.NewFactory(whatsapp.Options{}).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: model.TypeMetaWhatsapp, CredentialRef: "meta-app", Revision: "ch1",
		Configuration: whatsapp.Config{PhoneNumberID: pnid}.Extension(),
	}, adapter.Dependencies{HTTP: client, Credentials: cr, Clock: clock{testNow}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(t.Context()) })
	e := effect("00000000-0000-7000-8000-0000000000f1")
	e.Route.ID = recipient
	return inst.(*whatsapp.Instance), e
}

func TestRecordedTemplateSend(t *testing.T) {
	inst, e := recordedInstance(t)
	out, err := inst.Send(t.Context(), adapter.SendRequest{Effect: e, Content: templateContent(helloTemplate)})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != model.ProviderAccepted || len(out.References) != 1 || !strings.HasPrefix(out.References[0].ID, "wamid.") {
		t.Fatalf("outcome %+v, want provider_accepted with a wamid", out)
	}
}

func TestRecordedUnknownTemplateRejected(t *testing.T) {
	inst, e := recordedInstance(t)
	out, err := inst.Send(t.Context(), adapter.SendRequest{Effect: e, Content: templateContent(`{"name":"no_such_template_uma","language":{"code":"en_US"}}`)})
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Retry != model.NoRetry {
		t.Fatalf("Send = %+v, %v; want rejected without retry", out, err)
	}
}

// The adapter runs over the xrrtest transport: an exchange recorded from
// the documented-shape server replays with the server gone, and the
// cassette keeps no access token.
func TestXRRTransportRecordsAndReplays(t *testing.T) {
	dir := t.TempDir()
	f := fakeWithAccount(t)
	host := strings.TrimPrefix(f.srv.URL, "http://")
	send := func(mode xrrtest.Mode) adapter.Outcome {
		t.Helper()
		client := xrrtest.NewClient(t, dir, xrrtest.WithMode(mode), xrrtest.WithTransport(f.srv.Client().Transport),
			xrrtest.WithPlaceholder(host, "PROVIDER_HOST"), xrrtest.WithPlaceholder(testPNID, "PHONE_NUMBER_ID"))
		inst := openInstance(t, whatsapp.NewFactory(whatsapp.Options{BaseURL: f.srv.URL}), adapter.Dependencies{HTTP: client, Credentials: newCreds()})
		out, err := inst.Send(t.Context(), adapter.SendRequest{Effect: effect("00000000-0000-7000-8000-0000000000f2"), Content: textContent("hi")})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		return out
	}
	recorded := send(xrrtest.ModeRecord)
	f.srv.Close()
	replayed := send(xrrtest.ModeReplay)
	if replayed.State != model.ProviderAccepted || len(replayed.References) != 1 || replayed.References[0] != recorded.References[0] {
		t.Fatalf("replayed %+v, recorded %+v", replayed, recorded)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) == 0 {
		t.Fatal("nothing recorded")
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{"EAAG-access-token", testPNID, url.QueryEscape(host)} {
			if strings.Contains(string(data), leak) {
				t.Errorf("%s contains %q", filepath.Base(file), leak)
			}
		}
	}
}
