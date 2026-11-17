package whatsapp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/whatsapp"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

const (
	imageSID = "MM10000000000000000000000000000004"
	mediaOne = "ME10000000000000000000000000000001"
	mediaTwo = "ME10000000000000000000000000000002"
	mediaDir = "/2010-04-01/Accounts/" + hookAccount + "/Messages/" + imageSID + "/Media/"
)

// stagedItem is one StageAttachment call.
type stagedItem struct {
	Req   adapter.StageRequest
	Bytes []byte
}

// stagingSink is a sink that also stages attachments, as the engine's does:
// it records each request with the bytes read, and answers stageErr.
type stagingSink struct {
	recordingSink
	smu      sync.Mutex
	staged   []stagedItem
	stageErr func(key string) error
}

func (s *stagingSink) StageAttachment(_ context.Context, req adapter.StageRequest, body io.Reader) (model.AttachmentRef, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return model.AttachmentRef{}, err
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	s.staged = append(s.staged, stagedItem{req, b})
	if s.stageErr != nil {
		if err := s.stageErr(req.Key); err != nil {
			return model.AttachmentRef{}, err
		}
	}
	return model.AttachmentRef{Inbox: model.InboxRef{Tenant: "t", ID: "i"}, ID: model.ID("att-" + req.Key)}, nil
}

var _ adapter.AttachmentSink = (*stagingSink)(nil)

// imageForm is the golden incoming image with its media URL on origin.
func imageForm(t *testing.T, origin string) url.Values {
	t.Helper()
	form, err := url.ParseQuery(string(fixture(t, "incoming_image.form")))
	if err != nil {
		t.Fatal(err)
	}
	form.Set("MediaUrl0", strings.Replace(form.Get("MediaUrl0"), "https://api.twilio.com", origin, 1))
	return form
}

func attachments(env model.Envelope) []model.AttachmentRef {
	var out []model.AttachmentRef
	for _, p := range env.Content.Parts {
		if p.Kind == model.PartAttachment && p.Attachment != nil {
			out = append(out, *p.Attachment)
		}
	}
	return out
}

// mediaHook is the handler on a fake Twilio API holding the given media.
func mediaHook(t *testing.T, creds ports.Credentials, p adapter.ProtocolState, sink adapter.Sink, media map[string][]byte) (http.Handler, *fakeTwilio) {
	t.Helper()
	fake := newFakeTwilio(t)
	fake.mu.Lock()
	for k, v := range media {
		fake.account(hookAccount).media[k] = v
	}
	fake.mu.Unlock()
	deps := adapter.Dependencies{Credentials: creds}
	if p != nil {
		deps.Protocol = p
	}
	return open(t, fake, hookConfig(), deps).(adapter.HTTPReceiver).Handler(sink), fake
}

// An incoming image is downloaded from the account's Media resource with
// the REST credential, staged under its media SID, and named by an
// attachment part next to the media summary.
func TestIncomingMediaStaged(t *testing.T) {
	sink := &stagingSink{}
	creds := &tokenHost{}
	jpeg := []byte("\xff\xd8\xff\xe0 jpeg")
	hd, fake := mediaHook(t, creds, newProtocol(), sink, map[string][]byte{mediaOne: jpeg})
	body := []byte(imageForm(t, fake.srv.URL).Encode())
	for range 2 {
		if rec := post(hd, "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
	if len(sink.staged) != 2 {
		t.Fatalf("staged %d, want 1 per delivery", len(sink.staged))
	}
	st := sink.staged[0]
	if st.Req.Key != mediaOne || st.Req.Destination != senderNumber || string(st.Bytes) != string(jpeg) ||
		st.Req.Metadata.Name != "media-0" || st.Req.Metadata.MediaType != "image/jpeg" || st.Req.Metadata.Audience.Visibility != model.Public {
		t.Fatalf("stage request %+v with %q", st.Req, st.Bytes)
	}
	env := sink.got[0].Envelope
	if atts := attachments(env); len(atts) != 1 || atts[0].ID != "att-"+mediaOne {
		t.Fatalf("parts %+v, want the staged attachment", env.Content.Parts)
	}
	var summary bool
	for _, p := range env.Content.Parts {
		summary = summary || (p.Structured != nil && p.Structured.Name == whatsapp.PayloadMedia)
	}
	if !summary || env.Content.Parts[0].Text != "A caption" {
		t.Fatalf("parts %+v, want the caption and the media summary kept", env.Content.Parts)
	}
	var rest bool
	for _, r := range creds.requests() {
		rest = rest || r.Purpose == whatsapp.PurposeREST
	}
	if !rest {
		t.Fatalf("credential requests %+v, want the REST purpose for the download", creds.requests())
	}
	for _, c := range fake.calls(hookAccount) {
		if strings.HasPrefix(c.Path, mediaDir) && c.User != hookAccount {
			t.Fatalf("media request %s authorized as %q", c.Path, c.User)
		}
	}
}

// A JSON-bodied webhook stages its media alike.
func TestJSONIncomingMediaStaged(t *testing.T) {
	sink := &stagingSink{}
	hd, fake := mediaHook(t, &tokenHost{}, nil, sink, map[string][]byte{mediaOne: []byte("jpeg")})
	body, q := jsonOf(t, []byte(imageForm(t, fake.srv.URL).Encode()))
	if rec := post(hd, q, body, jsonType, signJSON(hookToken, q)); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(sink.staged) != 1 || len(attachments(sink.got[0].Envelope)) != 1 {
		t.Fatalf("staged %d, parts %+v", len(sink.staged), sink.got[0].Envelope.Content.Parts)
	}
}

// The REST credential never reaches a media URL off the API origin or of
// another message: the message keeps its media summary only.
func TestMediaDownloadStaysOnTheAccount(t *testing.T) {
	for name, mediaURL := range map[string]string{
		"other host":    "https://media.attacker.example" + mediaDir + mediaOne,
		"other message": "ORIGIN/2010-04-01/Accounts/" + hookAccount + "/Messages/MM10000000000000000000000000000099/Media/" + mediaOne,
		"other account": "ORIGIN/2010-04-01/Accounts/AC00000000000000000000000000000002/Messages/" + imageSID + "/Media/" + mediaOne,
		"with a query":  "ORIGIN" + mediaDir + mediaOne + "?x=1",
		"not a media":   "ORIGIN" + mediaDir + "x",
	} {
		t.Run(name, func(t *testing.T) {
			sink := &stagingSink{}
			hd, fake := mediaHook(t, &tokenHost{}, nil, sink, map[string][]byte{mediaOne: []byte("x")})
			form := imageForm(t, fake.srv.URL)
			form.Set("MediaUrl0", strings.Replace(mediaURL, "ORIGIN", fake.srv.URL, 1))
			body := []byte(form.Encode())
			if rec := post(hd, "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			if len(sink.got) != 1 || len(attachments(sink.got[0].Envelope)) != 0 || len(sink.staged) != 0 {
				t.Fatalf("parts %+v, want the media summary only", sink.got[0].Envelope.Content.Parts)
			}
			if n := len(fake.calls(hookAccount)); n != 0 {
				t.Fatalf("API calls = %d, want none", n)
			}
		})
	}
}

// At most 10 media are read from one message, in order; each stages under
// its own media SID.
func TestMediaStagedAtMostTen(t *testing.T) {
	sink := &stagingSink{}
	media := map[string][]byte{}
	for k := range 12 {
		media[fmt.Sprintf("ME%032x", k+1)] = []byte{byte(k)}
	}
	hd, fake := mediaHook(t, &tokenHost{}, nil, sink, media)
	form := imageForm(t, fake.srv.URL)
	form.Set("NumMedia", "12")
	for k := range 12 {
		n := fmt.Sprint(k)
		form.Set("MediaUrl"+n, fake.srv.URL+mediaDir+fmt.Sprintf("ME%032x", k+1))
		form.Set("MediaContentType"+n, "image/png")
	}
	body := []byte(form.Encode())
	if rec := post(hd, "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if len(sink.staged) != 10 || sink.staged[9].Req.Key != fmt.Sprintf("ME%032x", 10) || sink.staged[9].Req.Metadata.Name != "media-9" {
		t.Fatalf("staged %d, want the first 10", len(sink.staged))
	}
}

// Media that cannot be staged for good leave the message with its summary;
// a failure worth a redelivery answers 503 before the sink or the window
// see the message.
func TestMediaFailures(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		stageErr func(string) error
		want     int
		attached int
		gap      string
	}{
		{"gone at Twilio", http.StatusNotFound, nil, http.StatusOK, 0, "gone"},
		{"gone for good at Twilio", http.StatusGone, nil, http.StatusOK, 0, "gone"},
		{"refused credential", http.StatusUnauthorized, nil, http.StatusOK, 0, "unauthorized"},
		{"forbidden credential", http.StatusForbidden, nil, http.StatusOK, 0, "unauthorized"},
		{"unexpected answer", http.StatusBadRequest, nil, http.StatusOK, 0, "failed"},
		{"redirect nowhere", http.StatusTemporaryRedirect, nil, http.StatusOK, 0, "redirect"},
		{"request timeout", http.StatusRequestTimeout, nil, http.StatusServiceUnavailable, 0, ""},
		{"server fault", http.StatusInternalServerError, nil, http.StatusServiceUnavailable, 0, ""},
		{"rate limited", http.StatusTooManyRequests, nil, http.StatusServiceUnavailable, 0, ""},
		{"refused by the engine", 0, func(string) error { return model.NewError(model.Validation, "type refused") }, http.StatusOK, 0, ""},
		{"no blob store", 0, func(string) error { return model.NewError(model.Disabled, "no blob store") }, http.StatusOK, 0, ""},
		{"staging unavailable", 0, func(string) error { return model.NewError(model.Transient, "store down") }, http.StatusServiceUnavailable, 0, ""},
		{"staging canceled", 0, func(string) error { return context.Canceled }, http.StatusServiceUnavailable, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := &stagingSink{stageErr: c.stageErr}
			p := newProtocol()
			hd, fake := mediaHook(t, &tokenHost{}, p, sink, map[string][]byte{mediaOne: []byte("x")})
			if c.status != 0 {
				fake.mu.Lock()
				fake.account(hookAccount).mediaStatus[mediaOne] = c.status
				fake.mu.Unlock()
			}
			body := []byte(imageForm(t, fake.srv.URL).Encode())
			rec := post(hd, "", body, formType, signTwilio(hookToken, hookURL, body))
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if c.want == http.StatusServiceUnavailable {
				if len(sink.got) != 0 || !p.lastInbound(userNumber).IsZero() {
					t.Fatalf("a retryable media failure reached the sink (%d) or opened the window", len(sink.got))
				}
				return
			}
			if len(sink.got) != 1 || len(attachments(sink.got[0].Envelope)) != c.attached {
				t.Fatalf("sink got %d, want the message with %d attachments", len(sink.got), c.attached)
			}
			if got := mediaGap(t, sink.got[0].Envelope); got != c.gap {
				t.Fatalf("media 0 unavailable as %q, want %q", got, c.gap)
			}
		})
	}
}

// Without a staging sink, or an HTTP client for the download, a message
// with media still arrives with its summary, and a status callback never
// downloads anything.
func TestMediaNotStagedWithoutMeans(t *testing.T) {
	t.Run("sink without staging", func(t *testing.T) {
		sink := &recordingSink{}
		hd, fake := mediaHook(t, &tokenHost{}, nil, sink, map[string][]byte{mediaOne: []byte("x")})
		body := []byte(imageForm(t, fake.srv.URL).Encode())
		if rec := post(hd, "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		if len(sink.got) != 1 || len(attachments(sink.got[0].Envelope)) != 0 || len(fake.calls(hookAccount)) != 0 {
			t.Fatalf("parts %+v", sink.got[0].Envelope.Content.Parts)
		}
	})
	t.Run("no HTTP client", func(t *testing.T) {
		sink := &stagingSink{}
		body := fixture(t, "incoming_image.form")
		if rec := post(hook(t, &tokenHost{}, nil, sink), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		if len(sink.got) != 1 || len(sink.staged) != 0 {
			t.Fatalf("got %d staged %d", len(sink.got), len(sink.staged))
		}
	})
	t.Run("status callback", func(t *testing.T) {
		sink := &stagingSink{}
		hd, fake := mediaHook(t, &tokenHost{}, nil, sink, nil)
		form := imageForm(t, fake.srv.URL)
		form.Set("MessageStatus", "read")
		body := []byte(form.Encode())
		if rec := post(hd, "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		if len(sink.staged) != 0 || len(fake.calls(hookAccount)) != 0 {
			t.Fatalf("a status callback downloaded media")
		}
	})
}

// restFail resolves the webhook token and fails every other purpose.
type restFail struct {
	tokenHost
	err error
}

func (h *restFail) Resolve(ctx context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	if r.Purpose == whatsapp.PurposeWebhook {
		return h.tokenHost.Resolve(ctx, r)
	}
	return nil, h.err
}

// A REST credential that cannot be resolved for now is worth a redelivery;
// one that never will be leaves the message with its summary.
func TestMediaCredentialFailures(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"transient":  {model.NewError(model.Transient, "vault down"), http.StatusServiceUnavailable},
		"permanent":  {model.NewError(model.Unauthorized, "no credential"), http.StatusOK},
		"no purpose": {errors.New("unknown purpose"), http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			sink := &stagingSink{}
			hd, fake := mediaHook(t, &restFail{err: c.err}, nil, sink, map[string][]byte{mediaOne: []byte("x")})
			body := []byte(imageForm(t, fake.srv.URL).Encode())
			if rec := post(hd, "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != c.want {
				t.Fatalf("status %d, want %d", rec.Code, c.want)
			}
			if len(sink.staged) != 0 || len(fake.calls(hookAccount)) != 0 {
				t.Fatal("media downloaded without a credential")
			}
			if c.want == http.StatusOK && (len(sink.got) != 1 || mediaGap(t, sink.got[0].Envelope) != "unauthorized") {
				t.Fatal("a media left without a credential is not reported unauthorized")
			}
		})
	}
}

// mediaGap is why the media summary of env reports media 0 unavailable, ""
// when it does not.
func mediaGap(t *testing.T, env model.Envelope) string {
	t.Helper()
	for _, p := range env.Content.Parts {
		if p.Structured == nil || p.Structured.Name != whatsapp.PayloadMedia {
			continue
		}
		var m whatsapp.Media
		if err := json.Unmarshal(p.Structured.Data, &m); err != nil {
			t.Fatal(err)
		}
		for _, g := range m.Unavailable {
			if g.Index == 0 {
				return g.Reason
			}
		}
	}
	return ""
}
