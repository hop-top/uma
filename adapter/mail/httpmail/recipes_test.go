package httpmail_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"

	"hop.top/uma/adapter/mail/httpmail"
	"hop.top/uma/adapter/web/webhookauth"
)

// The configurations the page documents for hosted inbound services, run
// against payloads shaped as their documentation shows. They are not
// evidence about the live services.

// Postmark posts one JSON document, signs nothing, and takes the URL's
// user and password as HTTP Basic credentials. The token scheme compares the
// whole Authorization value with the stored secret.
func TestRecipePostmarkInbound(t *testing.T) {
	cfg := baseConfig()
	cfg.Auth = httpmail.Auth{Scheme: "token", Header: "Authorization"}
	cfg.JSONFields = httpmail.JSONFields{
		From: "From", To: "OriginalRecipient", Subject: "Subject", Body: "TextBody", Date: "Date", Attachments: "Attachments",
		AttachmentFields: httpmail.AttachmentFields{Name: "Name", MimeType: "ContentType", SizeBytes: "ContentLength"},
	}
	r := newRig(t, cfg)
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("postmark:a-long-webhook-password"))
	r.creds.cred = secretOf(basic)
	body := `{"FromName":"Postmarkapp Support","From":"support@postmarkapp.com","To":"support@example.com","OriginalRecipient":"support@example.com",
	  "Subject":"Test subject","MessageID":"73e6d360-66eb-11e1-8e72-a8904824019b","Date":"Fri, 1 Aug 2014 16:45:32 -04:00",
	  "TextBody":"This is a test text body.","HtmlBody":"<html><body><p>This is a test html body.</p></body></html>",
	  "Headers":[{"Name":"X-Spam-Status","Value":"No"}],
	  "Attachments":[{"Name":"test.txt","Content":"VGhpcyBpcyBhdHRhY2htZW50IGNvbnRlbnRzLCBiYXNlLTY0IGVuY29kZWQu","ContentType":"text/plain","ContentLength":45}]}`

	wrong := request{contentType: "application/json", body: body, noAuth: true, header: http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte("postmark:another-password-entirely"))}}}
	wantStatus(t, r.do(wrong), 401)
	ok := request{contentType: "application/json", body: body, noAuth: true, header: http.Header{"Authorization": {basic}}}
	wantStatus(t, r.do(ok), 200)
	in := only(t, r)
	if in.Envelope.Sender.ID != "support@postmarkapp.com" || textOf(in) != "This is a test text body." || in.Envelope.Destination != "support" {
		t.Errorf("envelope %+v", in.Envelope)
	}
	atts := partsOf(t, in)["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["size"] != float64(45) {
		t.Errorf("attachments %v", atts)
	}
}

// Mailgun's route to a URL ending in json posts one JSON document, the
// signature and timestamp in headers. Its receiving page signs the
// timestamp followed by the entire body, HMAC-SHA-256 as hex.
func TestRecipeMailgunJSONRoute(t *testing.T) {
	cfg := baseConfig()
	cfg.Auth = httpmail.Auth{
		Scheme: "hmac", Header: "X-Mailgun-Signature", Payload: "{timestamp}{body}",
		Timestamp: &httpmail.AuthTimestamp{Header: "X-Mailgun-Timestamp"},
		Replay:    &httpmail.AuthReplay{Source: "signature"},
	}
	cfg.JSONFields = httpmail.JSONFields{
		From: "from", To: "recipient", Subject: "subject", Body: "body-plain", Attachments: "attachments",
		AttachmentFields: httpmail.AttachmentFields{Name: "filename", MimeType: "content-type"},
	}
	r := newRig(t, cfg, func(o *httpmail.Options) { o.ReplayStore = webhookauth.NewMemoryReplayStore(8, fixedClock{now}) })
	body := `{"sender":"bounce@peers.test","from":"Bob <bob@peers.test>","recipient":"support@example.com","subject":"Hello",
	  "message-headers":[["Received","by mx"]],"body-plain":"Hi there","stripped-text":"Hi there",
	  "attachments":[{"filename":"a.txt","content-type":"text/plain","content":"aGk="}]}`
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(relayTokenForSigning))
	mac.Write([]byte(ts + body))
	hdr := http.Header{"X-Mailgun-Timestamp": {ts}, "X-Mailgun-Signature": {hex.EncodeToString(mac.Sum(nil))}}
	rq := request{contentType: "application/json", body: body, noAuth: true, header: hdr}
	wantStatus(t, r.do(rq), 200)
	in := only(t, r)
	if in.Envelope.Sender.ID != "bob@peers.test" || textOf(in) != "Hi there" || in.Envelope.Destination != "support" {
		t.Errorf("envelope %+v", in.Envelope)
	}
	atts := partsOf(t, in)["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["filename"] != "a.txt" || atts[0].(map[string]any)["media_type"] != "text/plain" {
		t.Errorf("attachments %v", atts)
	}
	wantStatus(t, r.do(rq), 409) // the same signed delivery again
	tampered := rq
	tampered.body = body + " "
	wantStatus(t, r.do(tampered), 401)
}
