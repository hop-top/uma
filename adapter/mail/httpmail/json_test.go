package httpmail_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter/mail/httpmail"
	"hop.top/uma/model"
)

// payloadOf decodes the message metadata of an envelope.
func payloadOf(t *testing.T, in model.Ingress) map[string]any {
	t.Helper()
	p := in.Envelope.Payload
	if p.Name != httpmail.MessageSchema || p.Version != httpmail.MessageVersion {
		t.Fatalf("payload %s v%d", p.Name, p.Version)
	}
	var m map[string]any
	if err := json.Unmarshal(p.Data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func sub(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v: %T is not an object", path, cur)
		}
		cur = mm[k]
	}
	return cur
}

// partsOf decodes the structured attachment part of an envelope.
func partsOf(t *testing.T, in model.Ingress) map[string]any {
	t.Helper()
	for _, p := range in.Envelope.Content.Parts {
		if p.Kind == model.PartStructured {
			if p.Structured.Name != httpmail.PartsSchema || p.Structured.Version != httpmail.PartsVersion {
				t.Fatalf("part %s v%d", p.Structured.Name, p.Structured.Version)
			}
			var m map[string]any
			if err := json.Unmarshal(p.Structured.Data, &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	return nil
}

func textOf(in model.Ingress) string {
	for _, p := range in.Envelope.Content.Parts {
		if p.Kind == model.PartText {
			return p.Text
		}
	}
	return ""
}

func only(t *testing.T, r *rig) model.Ingress {
	t.Helper()
	got := r.sink.ingresses()
	if len(got) != 1 {
		t.Fatalf("sink saw %d ingresses, want 1", len(got))
	}
	return got[0]
}

func TestJSONDefaultFields(t *testing.T) {
	r := newRig(t, baseConfig())
	rec := r.postJSON(`{"from":"alice@peers.test","to":"support@example.com","subject":"Quote","body":"Please quote 40 units."}`)
	wantStatus(t, rec, 200)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"accepted":1}` {
		t.Errorf("answer %q", got)
	}
	in := only(t, r)
	env := in.Envelope
	if env.Destination != "support" || env.Kind != model.EnvelopeMessage {
		t.Errorf("destination %q kind %q", env.Destination, env.Kind)
	}
	want := model.ExternalRef{Channel: chRef, Namespace: httpmail.Namespace, Kind: httpmail.RouteKind, ID: "alice@peers.test"}
	if env.Sender != want {
		t.Errorf("sender %+v, want %+v", env.Sender, want)
	}
	if env.Assurance != "unverified" {
		t.Errorf("assurance %q: nothing in a JSON payload is verified", env.Assurance)
	}
	if got := textOf(in); got != "Please quote 40 units." {
		t.Errorf("text %q", got)
	}
	if env.Audience.Visibility != model.Public {
		t.Errorf("audience %+v", env.Audience)
	}
	m := payloadOf(t, in)
	if sub(t, m, "format") != "json" || sub(t, m, "headers", "subject") != "Quote" {
		t.Errorf("payload %v", m)
	}
	if err := env.Content.Validate(); err != nil {
		t.Errorf("content: %v", err)
	}
	if err := env.Payload.Validate(); err != nil {
		t.Errorf("payload: %v", err)
	}
}

func TestJSONContentTypeParametersAreIgnored(t *testing.T) {
	r := newRig(t, baseConfig())
	rec := r.do(request{contentType: "Application/JSON; charset=utf-8", body: minimalJSON})
	wantStatus(t, rec, 200)
}

func TestJSONSenderWithDisplayNameIsNormalized(t *testing.T) {
	r := newRig(t, baseConfig())
	wantStatus(t, r.postJSON(`{"from":"Alice Martin <Alice@Peers.TEST>","to":"Support <support@EXAMPLE.com>","body":"x"}`), 200)
	in := only(t, r)
	if in.Envelope.Sender.ID != "Alice@peers.test" {
		t.Errorf("sender %q: domain lowercased, local part exact", in.Envelope.Sender.ID)
	}
	if in.Envelope.Destination != "support" {
		t.Errorf("destination %q", in.Envelope.Destination)
	}
	from, _ := sub(t, payloadOf(t, in), "headers", "from").([]any)
	if len(from) != 1 {
		t.Fatalf("from %v", from)
	}
	if got := from[0].(map[string]any)["name"]; got != "Alice Martin" {
		t.Errorf("from name %v", got)
	}
}

func TestJSONRecipientForms(t *testing.T) {
	for name, to := range map[string]string{
		"string":      `"support@example.com"`,
		"list string": `"nobody@example.com, support@example.com"`,
		"array":       `["nobody@example.com", "Support <support@example.com>"]`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, baseConfig())
			wantStatus(t, r.postJSON(fmt.Sprintf(`{"from":"a@peers.test","to":%s,"body":"x"}`, to)), 200)
			if in := only(t, r); in.Envelope.Destination != "support" {
				t.Errorf("destination %q", in.Envelope.Destination)
			}
		})
	}
}

func TestJSONAddressesOfOneDestinationShareAnEnvelope(t *testing.T) {
	cfg := baseConfig()
	cfg.Recipients = append(cfg.Recipients, httpmail.Recipient{Address: "help@example.com", Destination: "support"})
	r := newRig(t, cfg)
	wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":["help@example.com","support@example.com","help@example.com"],"body":"x"}`), 200)
	in := only(t, r)
	rcpts, _ := sub(t, payloadOf(t, in), "recipients").([]any)
	if len(rcpts) != 2 || rcpts[0] != "help@example.com" || rcpts[1] != "support@example.com" {
		t.Errorf("recipients %v", rcpts)
	}
}

func TestJSONOneIngressPerDestination(t *testing.T) {
	r := newRig(t, baseConfig())
	wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":["support@example.com","sales@example.com","SUPPORT@example.com"],"body":"x"}`), 200)
	got := r.sink.ingresses()
	if len(got) != 2 || got[0].Envelope.Destination != "support" || got[1].Envelope.Destination != "sales" {
		t.Fatalf("ingresses %d: %+v", len(got), got)
	}
	if got[0].Envelope.EventID == got[1].Envelope.EventID {
		t.Errorf("one event ID %q for two destinations", got[0].Envelope.EventID)
	}
}

func TestJSONUnknownRecipientIsRefused(t *testing.T) {
	r := newRig(t, baseConfig())
	wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":"nobody@example.com","body":"x"}`), 422)
	if n := len(r.sink.ingresses()); n != 0 {
		t.Fatalf("sink saw %d ingresses", n)
	}
}

func TestJSONMalformedRequestsAreRefused(t *testing.T) {
	for name, body := range map[string]string{
		"not json":       `{"from":`,
		"array":          `[]`,
		"no from":        `{"to":"support@example.com","body":"x"}`,
		"no to":          `{"from":"a@peers.test","body":"x"}`,
		"from not text":  `{"from":{"email":"a@peers.test"},"to":"support@example.com"}`,
		"bad from":       `{"from":"not an address","to":"support@example.com"}`,
		"to not text":    `{"from":"a@peers.test","to":7}`,
		"bad to":         `{"from":"a@peers.test","to":"@@"}`,
		"body not text":  `{"from":"a@peers.test","to":"support@example.com","body":["x"]}`,
		"trailing value": minimalJSON + ` {}`,
		"empty":          ``,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, baseConfig())
			wantStatus(t, r.postJSON(body), 400)
			if n := len(r.sink.ingresses()); n != 0 {
				t.Fatalf("sink saw %d ingresses", n)
			}
		})
	}
}

// Another sender's member names are configuration: a Postmark inbound
// document, whose top level carries more members than are read.
func TestJSONFieldNamesFromConfiguration(t *testing.T) {
	cfg := baseConfig()
	cfg.JSONFields = httpmail.JSONFields{
		From: "From", To: "To", Subject: "Subject", Body: "TextBody", Date: "Date", Attachments: "Attachments",
		AttachmentFields: httpmail.AttachmentFields{Name: "Name", MimeType: "ContentType", SizeBytes: "ContentLength"},
	}
	r := newRig(t, cfg)
	wantStatus(t, r.postJSON(`{
	  "FromName": "Alice", "From": "alice@peers.test", "FromFull": {"Email": "alice@peers.test", "Name": "Alice"},
	  "To": "support@example.com", "ToFull": [{"Email": "support@example.com"}], "OriginalRecipient": "support@example.com",
	  "Subject": "Invoice", "MessageID": "73e6d360-66eb-11e1-8e72-a8904824019b", "Date": "Fri, 1 Aug 2014 16:45:32 -04:00",
	  "TextBody": "See attached.", "HtmlBody": "<p>See attached.</p>",
	  "Headers": [{"Name": "X-Spam-Status", "Value": "No"}],
	  "Attachments": [{"Name": "invoice.pdf", "Content": "JVBERi0xLjQK", "ContentType": "application/pdf", "ContentLength": 4096}]
	}`), 200)
	in := only(t, r)
	if textOf(in) != "See attached." || in.Envelope.Destination != "support" || in.Envelope.Sender.ID != "alice@peers.test" {
		t.Errorf("envelope %+v", in.Envelope)
	}
	if want := time.Date(2014, 8, 1, 20, 45, 32, 0, time.UTC); !in.Envelope.OccurredAt.Equal(want) {
		t.Errorf("occurred %v, want %v", in.Envelope.OccurredAt, want)
	}
	atts := partsOf(t, in)["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("attachments %v", atts)
	}
	a := atts[0].(map[string]any)
	if a["filename"] != "invoice.pdf" || a["media_type"] != "application/pdf" || a["size"] != float64(4096) {
		t.Errorf("attachment %v", a)
	}
	if strings.Contains(string(in.Envelope.Content.Parts[len(in.Envelope.Content.Parts)-1].Structured.Data), "JVBERi") {
		t.Error("attachment bytes were kept")
	}
	// A Postmark MessageID is no RFC 5322 msg-id: it threads nothing.
	if in.Envelope.MessageID != "" {
		t.Errorf("message id %q", in.Envelope.MessageID)
	}
}

func TestJSONThreadingHeaders(t *testing.T) {
	r := newRig(t, baseConfig())
	wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":"support@example.com","body":"x",
	  "message_id":"<m2@peers.test>","in_reply_to":"<m1@peers.test>","references":"<m0@peers.test> <m1@peers.test>"}`), 200)
	env := only(t, r).Envelope
	if env.MessageID != "<m2@peers.test>" {
		t.Errorf("message id %q", env.MessageID)
	}
	want := model.EmailHeaders{MessageID: "<m2@peers.test>", InReplyTo: "<m1@peers.test>", References: "<m0@peers.test> <m1@peers.test>"}.Normalize(chRef)
	if fmt.Sprint(env.References) != fmt.Sprint(want.References) {
		t.Errorf("references %v, want %v", env.References, want.References)
	}
	if !strings.HasPrefix(env.EventID, "rcpt:support;sha256:") {
		t.Errorf("event id %q", env.EventID)
	}
}

// A Message-ID is the sender's to choose and anyone can repeat it, so it
// never decides that a message was already seen: the bytes do. Sender B
// reusing the id sender A used must not suppress B's message, and A's
// message must not be suppressed by one that claimed its id first.
func TestSameMessageIDDifferentMessageIsNotADuplicate(t *testing.T) {
	r := newRig(t, baseConfig())
	post := func(from, body string) {
		wantStatus(t, r.postJSON(fmt.Sprintf(`{"from":%q,"to":"support@example.com","body":%q,"message_id":"<claimed@peers.test>"}`, from, body)), 200)
	}
	post("attacker@peers.test", "forged")
	post("alice@peers.test", "the real message")
	post("alice@peers.test", "another message under the same id")
	post("alice@peers.test", "the real message") // a genuine redelivery
	got := r.sink.ingresses()
	id := func(i int) [2]string { return [2]string{got[i].Envelope.EventID, got[i].Envelope.MessageID} }
	if id(0) == id(1) || id(1) == id(2) || id(0) == id(2) {
		t.Errorf("messages with different content share an identity: %v %v %v", id(0), id(1), id(2))
	}
	if id(1) != id(3) {
		t.Errorf("a redelivery has two identities: %v %v", id(1), id(3))
	}
}

func TestJSONRedeliveryHasOneIdentity(t *testing.T) {
	r := newRig(t, baseConfig())
	for range 2 {
		wantStatus(t, r.postJSON(minimalJSON), 200)
	}
	wantStatus(t, r.postJSON(`{"from":"alice@peers.test","to":"support@example.com","subject":"hi","body":"another"}`), 200)
	got := r.sink.ingresses()
	if got[0].Envelope.EventID != got[1].Envelope.EventID {
		t.Errorf("a redelivery has two identities: %q, %q", got[0].Envelope.EventID, got[1].Envelope.EventID)
	}
	if got[0].Envelope.EventID == got[2].Envelope.EventID {
		t.Errorf("two messages share the identity %q", got[0].Envelope.EventID)
	}
	if !strings.HasPrefix(got[0].Envelope.EventID, "rcpt:support;sha256:") {
		t.Errorf("event id %q", got[0].Envelope.EventID)
	}
}

func TestJSONAttachmentsAreDescribedNotFetched(t *testing.T) {
	r := newRig(t, baseConfig())
	wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":"support@example.com","body":"x","attachments":[
	  {"type":"image","url":"https://files.example.net/a.png","mime_type":"image/png","size_bytes":123,"name":"a.png"},
	  {"type":"document","url":"javascript:alert(1)","mime_type":"text/html"},
	  {"url":"file:///etc/passwd"},
	  {"url":"//files.example.net/b.png"},
	  {"url":"https:///no-host"},
	  {"type":"audio"},
	  "not an object",
	  {}
	]}`), 200)
	in := only(t, r)
	atts := partsOf(t, in)["attachments"].([]any)
	if len(atts) != 6 {
		t.Fatalf("%d attachments %v, want 6: entries that are no object or carry nothing are dropped", len(atts), atts)
	}
	first := atts[0].(map[string]any)
	if first["kind"] != "image" || first["url"] != "https://files.example.net/a.png" || first["media_type"] != "image/png" || first["size"] != float64(123) || first["filename"] != "a.png" {
		t.Errorf("first %v", first)
	}
	for i, a := range atts[1:5] {
		if _, ok := a.(map[string]any)["url"]; ok {
			t.Errorf("attachment %d kept url %v: only absolute http and https are recorded", i+1, a)
		}
	}
	if sub(t, payloadOf(t, in), "attachments_claimed") != true {
		t.Errorf("payload does not say the descriptions are the sender's claims: %v", payloadOf(t, in))
	}
}

func TestJSONAttachmentSizeIsAPositiveWholeNumber(t *testing.T) {
	r := newRig(t, baseConfig())
	wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":"support@example.com","body":"x","attachments":[
	  {"name":"neg","size_bytes":-5},{"name":"zero","size_bytes":0},{"name":"frac","size_bytes":1.5},{"name":"text","size_bytes":"12"},{"name":"junk","size_bytes":"big"}]}`), 200)
	atts := partsOf(t, only(t, r))["attachments"].([]any)
	for _, a := range atts {
		a := a.(map[string]any)
		want := float64(0)
		if a["filename"] == "text" {
			want = 12
		}
		if got, _ := a["size"].(float64); got != want {
			t.Errorf("%v: size %v, want %v", a["filename"], a["size"], want)
		}
	}
}

func TestJSONAttachmentOnlyMessage(t *testing.T) {
	r := newRig(t, baseConfig())
	wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":"support@example.com","attachments":[{"name":"a.txt"}]}`), 200)
	in := only(t, r)
	if textOf(in) != "" || partsOf(t, in) == nil {
		t.Errorf("parts %+v", in.Envelope.Content.Parts)
	}
	if err := in.Envelope.Content.Validate(); err != nil {
		t.Error(err)
	}
	// No text and nothing described still yields a valid, empty description.
	r = newRig(t, baseConfig())
	wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":"support@example.com"}`), 200)
	if err := only(t, r).Envelope.Content.Validate(); err != nil {
		t.Error(err)
	}
}

func TestJSONAttachmentCountIsBounded(t *testing.T) {
	r := newRig(t, baseConfig())
	var b strings.Builder
	for i := range 80 {
		fmt.Fprintf(&b, `{"name":"f%d"},`, i)
	}
	wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":"support@example.com","body":"x","attachments":[`+strings.TrimSuffix(b.String(), ",")+`]}`), 200)
	parts := partsOf(t, only(t, r))
	if n := len(parts["attachments"].([]any)); n != 50 || parts["omitted"] != float64(30) {
		t.Errorf("%d attachments, omitted %v", n, parts["omitted"])
	}
}

func TestJSONOversizeTextIsTruncated(t *testing.T) {
	body := strings.Repeat("é", model.DefaultContentLimits().MaxTextBytes) // 2 MiB
	cfg := baseConfig()
	cfg.MaxBodyBytes = 4 << 20
	r := newRig(t, cfg)
	wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":"support@example.com","body":"`+body+`"}`), 200)
	in := only(t, r)
	if got := len(textOf(in)); got > model.DefaultContentLimits().MaxTextBytes || got < model.DefaultContentLimits().MaxTextBytes-4 {
		t.Errorf("text %d bytes", got)
	}
	if sub(t, payloadOf(t, in), "text", "truncated") != true {
		t.Errorf("payload %v", payloadOf(t, in))
	}
	if err := in.Envelope.Content.Validate(); err != nil {
		t.Error(err)
	}
}

func TestJSONDate(t *testing.T) {
	for name, tc := range map[string]struct {
		date string
		want time.Time
	}{
		"rfc3339": {`"2026-10-12T08:30:00+02:00"`, time.Date(2026, 10, 12, 6, 30, 0, 0, time.UTC)},
		"rfc5322": {`"Mon, 12 Oct 2026 08:30:00 +0200"`, time.Date(2026, 10, 12, 6, 30, 0, 0, time.UTC)},
		"absent":  {`null`, now},
		"garbage": {`"yesterday"`, now},
		"number":  {`1760000000`, now},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, baseConfig())
			wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":"support@example.com","body":"x","date":`+tc.date+`}`), 200)
			if got := only(t, r).Envelope.OccurredAt; !got.Equal(tc.want) {
				t.Errorf("occurred %v, want %v", got, tc.want)
			}
		})
	}
}

func TestJSONNeverTakesAssuranceFromThePayload(t *testing.T) {
	r := newRig(t, baseConfig())
	wantStatus(t, r.postJSON(`{"from":"ceo@example.com","to":"support@example.com","body":"wire the money",
	  "assurance":"dmarc_pass","verified":true,"spf":"pass","dkim":"pass","dmarc":"pass","sender_kind":"bot",
	  "authentication_results":"mx.example.com; spf=pass; dkim=pass; dmarc=pass header.from=example.com"}`), 200)
	in := only(t, r)
	if in.Envelope.Assurance != "unverified" {
		t.Errorf("assurance %q", in.Envelope.Assurance)
	}
	if in.Envelope.SenderKind != "" {
		t.Errorf("sender kind %q", in.Envelope.SenderKind)
	}
	m := payloadOf(t, in)
	if sub(t, m, "evidence", "trusted") != false {
		t.Errorf("evidence %v", sub(t, m, "evidence"))
	}
	for _, k := range []string{"spf", "dkim", "dmarc"} {
		if sub(t, m, "evidence", k) != nil {
			t.Errorf("evidence carries %s from a payload member", k)
		}
	}
}

func TestSinkFailuresMapToRetryAdvice(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"unavailable":  {&model.Error{Code: model.Unavailable, SafeMessage: "down"}, 503},
		"untyped":      {fmt.Errorf("boom"), 503},
		"unauthorized": {&model.Error{Code: model.Unauthorized, SafeMessage: "denied", Retry: model.NoRetry}, 403},
		"not found":    {&model.Error{Code: model.NotFound, SafeMessage: "no inbox", Retry: model.NoRetry}, 422},
		"disabled":     {&model.Error{Code: model.Disabled, SafeMessage: "off", Retry: model.NoRetry}, 422},
		"validation":   {&model.Error{Code: model.Validation, SafeMessage: "bad", Retry: model.NoRetry}, 422},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, baseConfig())
			r.sink.fail(tc.err)
			wantStatus(t, r.postJSON(minimalJSON), tc.want)
		})
	}
}

func TestFormatsAreConfiguration(t *testing.T) {
	cfg := baseConfig()
	cfg.Formats = []string{httpmail.FormatRFC822}
	r := newRig(t, cfg)
	wantStatus(t, r.postJSON(minimalJSON), 415)
}
