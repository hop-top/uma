package smtp_test

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	gosmtp "github.com/emersion/go-smtp"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

var (
	opA = model.OperationRef{Tenant: tenant, ID: "00000c00-0000-7000-8000-0000000000e1"}
	opB = model.OperationRef{Tenant: tenant, ID: "00000c00-0000-7000-8000-0000000000e2"}
)

type sendRig struct {
	relay *relay
	creds *credHost
	inst  *smtp.Instance
}

func newSendRig(t *testing.T) *sendRig {
	t.Helper()
	r := newRelay(t)
	creds := passwordHost()
	f := smtp.NewFactory(smtp.Options{RootCAs: certs(t).roots, Resolver: newZone()})
	return &sendRig{relay: r, creds: creds, inst: open(t, f, channel(channelA, config(r.port)), creds)}
}

func route(ch model.ChannelRef, addr string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: smtp.Namespace, Kind: smtp.RouteKind, ID: addr}
}

func textRequest(op model.OperationRef, text string) adapter.SendRequest {
	return adapter.SendRequest{
		Effect: adapter.Effect{
			Operation: op, Attempt: "00000c00-0000-7000-8000-0000000000a1",
			Route: route(channelA, "jane@example.org"), Audience: model.Audience{Visibility: model.Public},
		},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	}
}

func TestSendAccepted(t *testing.T) {
	rig := newSendRig(t)
	req := textRequest(opA, "Thanks, on it.")
	parent := model.ProviderMessageRef(channelA, "<m2@example.org>")
	req.Target = &parent
	out, err := rig.inst.Send(t.Context(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	txs := rig.relay.transactions()
	if len(txs) != 1 || !txs[0].applied || txs[0].from != "support@example.com" || len(txs[0].rcpts) != 1 || txs[0].rcpts[0] != "jane@example.org" {
		t.Fatalf("relay transactions = %+v", txs)
	}
	m, err := mime.Parse(bytes.NewReader(txs[0].data), mime.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	h := m.Header
	if out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != model.ProviderMessageRef(channelA, h.MessageID) {
		t.Fatalf("outcome = %+v, want provider_accepted referencing Message-ID %q", out, h.MessageID)
	}
	if !strings.HasSuffix(h.MessageID, "@example.com>") {
		t.Errorf("Message-ID %q not in the From domain", h.MessageID)
	}
	if h.InReplyTo != "<m2@example.org>" || h.References != "<m2@example.org>" {
		t.Errorf("In-Reply-To %q References %q, want the target message", h.InReplyTo, h.References)
	}
	if h.Subject != "Your request" || h.From[0] != (mime.Address{Name: "Support", Address: "support@example.com"}) || m.Text != "Thanks, on it." {
		t.Errorf("message = %+v text %q", h, m.Text)
	}
	if !h.Date.Equal(now) {
		t.Errorf("Date = %v, want the clock's %v", h.Date, now)
	}
	reqs := rig.creds.requests()
	if len(reqs) != 1 || reqs[0].Purpose != smtp.PurposeRelay || reqs[0].Reference != "relay-cred" || reqs[0].Channel != channelA ||
		!reqs[0].Accepts(ports.KindPassword) || !reqs[0].Accepts(ports.KindOAuth2) || reqs[0].Accepts(ports.KindSecret) {
		t.Errorf("credential requests = %+v", reqs)
	}
}

func TestMessageIDOwnedPerOperation(t *testing.T) {
	rig := newSendRig(t)
	ids := map[model.OperationRef][]string{}
	for _, op := range []model.OperationRef{opA, opA, opB} {
		out, err := rig.inst.Send(t.Context(), textRequest(op, "hi"))
		if err != nil {
			t.Fatal(err)
		}
		ids[op] = append(ids[op], out.References[0].ID)
	}
	if ids[opA][0] != ids[opA][1] {
		t.Errorf("one operation sent Message-IDs %v; retries must reuse it", ids[opA])
	}
	if ids[opA][0] == ids[opB][0] {
		t.Errorf("two operations share Message-ID %s", ids[opA][0])
	}
	if strings.Contains(ids[opA][0], string(opA.ID)) {
		t.Errorf("Message-ID %s reveals the operation ID", ids[opA][0])
	}
	if req := textRequest(opA, "hi"); true {
		other := model.ProviderMessageRef(channelB, "<m2@example.org>")
		req.Target = &other
		if _, err := rig.inst.Send(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		txs := rig.relay.transactions()
		m, _ := mime.Parse(bytes.NewReader(txs[len(txs)-1].data), mime.Limits{})
		if m.Header.InReplyTo != "" {
			t.Errorf("In-Reply-To %q from another channel's target", m.Header.InReplyTo)
		}
	}
}

func smtpErr(code int, a, b, c int) *gosmtp.SMTPError {
	return &gosmtp.SMTPError{Code: code, EnhancedCode: gosmtp.EnhancedCode{a, b, c}, Message: "scripted " + relayPass}
}

// TestSendReplyMapping pins the relay reply to outcome mapping.
func TestSendReplyMapping(t *testing.T) {
	cases := map[string]struct {
		reply   reply
		state   model.DeliveryState
		code    model.ErrorCode
		applied bool
	}{
		"4xx at MAIL is transient":        {reply{at: "MAIL", err: smtpErr(451, 4, 3, 0)}, "", model.Transient, false},
		"4xx at RCPT is transient":        {reply{at: "RCPT", err: smtpErr(452, 4, 2, 2)}, "", model.Transient, false},
		"4xx at end of DATA is transient": {reply{at: "END", err: smtpErr(451, 4, 7, 1)}, "", model.Transient, false},
		"5xx at MAIL is rejected":         {reply{at: "MAIL", err: smtpErr(550, 5, 7, 1)}, model.Rejected, model.Permanent, false},
		"5xx at RCPT is rejected":         {reply{at: "RCPT", err: smtpErr(550, 5, 1, 1)}, model.Rejected, model.Permanent, false},
		"5xx at DATA is rejected":         {reply{at: "DATA", err: smtpErr(554, 5, 0, 0)}, model.Rejected, model.Permanent, false},
		"5xx at end of DATA is rejected":  {reply{at: "END", err: smtpErr(554, 5, 6, 0)}, model.Rejected, model.Permanent, false},
		"timeout after DATA is unknown":   {reply{hang: true}, "", model.OutcomeUnknown, true},
		"dropped after DATA is unknown":   {reply{drop: true}, "", model.OutcomeUnknown, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newSendRig(t)
			rig.relay.respond(c.reply)
			out, err := rig.inst.Send(t.Context(), textRequest(opA, "hi"))
			if c.state == model.Rejected {
				if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != c.code || out.Failure.Retry != model.NoRetry {
					t.Fatalf("Send = %+v, %v; want rejected (%s, no retry)", out, err, c.code)
				}
				if strings.Contains(out.Failure.Error(), "scripted") {
					t.Errorf("failure %q carries relay reply text", out.Failure)
				}
			} else {
				if !model.IsCode(err, c.code) {
					t.Fatalf("Send = %+v, %v; want error %s", out, err, c.code)
				}
				if strings.Contains(err.Error(), "scripted") {
					t.Errorf("error %q carries relay reply text", err)
				}
			}
			txs := rig.relay.transactions()
			if len(txs) != 1 || txs[0].applied != c.applied {
				t.Fatalf("relay transactions = %+v, want one (applied %v)", txs, c.applied)
			}
		})
	}
}

func TestSendAuthentication(t *testing.T) {
	t.Run("refused credential is unauthorized", func(t *testing.T) {
		rig := newSendRig(t)
		rig.relay.setAuth(false)
		_, err := rig.inst.Send(t.Context(), textRequest(opA, "hi"))
		if !model.IsCode(err, model.Unauthorized) {
			t.Fatalf("Send = %v, want unauthorized", err)
		}
		if strings.Contains(err.Error(), relayPass) {
			t.Error("error discloses the password")
		}
		if txs := rig.relay.transactions(); len(txs) != 0 {
			t.Fatalf("relay transactions = %+v, want none", txs)
		}
	})
	t.Run("host refusal makes no connection", func(t *testing.T) {
		rig := newSendRig(t)
		rig.creds.err = model.NewError(model.Unauthorized, "consent revoked")
		if _, err := rig.inst.Send(t.Context(), textRequest(opA, "hi")); !model.IsCode(err, model.Unauthorized) {
			t.Fatalf("Send = %v, want unauthorized", err)
		}
		if rig.relay.authCount() != 0 || len(rig.relay.transactions()) != 0 {
			t.Fatal("relay contacted without a credential")
		}
	})
	t.Run("credential per send", func(t *testing.T) {
		rig := newSendRig(t)
		for range 2 {
			if _, err := rig.inst.Send(t.Context(), textRequest(opA, "hi")); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(rig.creds.requests()); n != 2 {
			t.Fatalf("credential resolutions = %d, want one per send", n)
		}
	})
	t.Run("relay unreachable is retryable", func(t *testing.T) {
		ln := listen(t)
		p := port(ln)
		_ = ln.Close()
		f := smtp.NewFactory(smtp.Options{RootCAs: certs(t).roots})
		inst := open(t, f, channel(channelA, config(p)), passwordHost())
		_, err := inst.Send(t.Context(), textRequest(opA, "hi"))
		var me *model.Error
		if !asError(err, &me) || me.Retry != model.RetrySameOperation {
			t.Fatalf("Send = %v, want a retryable error", err)
		}
	})
}

func TestSendRefusesBeforeConnecting(t *testing.T) {
	att := model.Content{Parts: []model.Part{{Kind: model.PartAttachment, Attachment: &model.AttachmentRef{Inbox: model.InboxRef{Tenant: tenant, ID: "00000c00-0000-7000-8000-0000000000b1"}, ID: "00000c00-0000-7000-8000-0000000000b2"}}}}
	for name, c := range map[string]struct {
		mut  func(*adapter.SendRequest)
		code model.ErrorCode
	}{
		"attachment part":        {func(r *adapter.SendRequest) { r.Content = att }, model.Unsupported},
		"route on other channel": {func(r *adapter.SendRequest) { r.Route = route(channelB, "jane@example.org") }, model.Validation},
		"route not an address":   {func(r *adapter.SendRequest) { r.Route = route(channelA, "Jane <jane@example.org>") }, model.Validation},
		"route other namespace":  {func(r *adapter.SendRequest) { r.Route.Namespace = "org.example.other" }, model.Validation},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newSendRig(t)
			req := textRequest(opA, "hi")
			c.mut(&req)
			_, err := rig.inst.Send(t.Context(), req)
			var me *model.Error
			if !asError(err, &me) || me.Code != c.code || me.Retry != model.NoRetry {
				t.Fatalf("Send = %v, want %s without retry", err, c.code)
			}
			if len(rig.creds.requests()) != 0 || rig.relay.authCount() != 0 {
				t.Fatal("credential resolved or relay contacted for an unsendable request")
			}
		})
	}
}

func TestSendRequiresTLS(t *testing.T) {
	r := newRelayTLS(t, false)
	f := smtp.NewFactory(smtp.Options{RootCAs: certs(t).roots})
	inst := open(t, f, channel(channelA, config(r.port)), passwordHost())
	if _, err := inst.Send(t.Context(), textRequest(opA, "hi")); err == nil {
		t.Fatal("Send without TLS succeeded")
	}
	if r.authCount() != 0 {
		t.Fatal("credential sent over plaintext")
	}
}

// A reply carrying its thread threads the whole chain and answers the
// parent's subject.
func TestSendThreadsTheReply(t *testing.T) {
	rig := newSendRig(t)
	req := textRequest(opA, "Here you go.")
	parent := model.ProviderMessageRef(channelA, "<m3@example.org>")
	hdr := func(kind, id string) model.ExternalRef {
		return model.ExternalRef{Channel: channelA, Namespace: model.EmailHeaderNamespace, Kind: kind, ID: id}
	}
	req.Target = &parent
	req.Thread = &adapter.Thread{
		References: []model.ExternalRef{parent, hdr(model.RefKindEmailMessageID, "<m3@example.org>"), hdr(model.RefKindEmailReferences, "<m1@example.org>")},
		Metadata:   []model.Extension{{Name: smtp.MessageSchema, Version: 1, Data: []byte(`{"headers":{"subject":"Invoice 12"}}`)}},
	}
	if _, err := rig.inst.Send(t.Context(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	m, err := mime.Parse(bytes.NewReader(rig.relay.transactions()[0].data), mime.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if h := m.Header; h.InReplyTo != "<m3@example.org>" || h.References != "<m1@example.org> <m3@example.org>" || h.Subject != "Re: Invoice 12" {
		t.Fatalf("In-Reply-To %q References %q Subject %q", h.InReplyTo, h.References, h.Subject)
	}
}

func parentRecord(t *testing.T, headers string) []model.Extension {
	t.Helper()
	return []model.Extension{{Name: smtp.MessageSchema, Version: smtp.MessageVersion, Data: []byte(`{"headers":` + headers + `}`)}}
}

func sentMessage(t *testing.T, rig *sendRig) (relayTx, *mime.Message) {
	t.Helper()
	txs := rig.relay.transactions()
	if len(txs) != 1 || !txs[0].applied {
		t.Fatalf("relay transactions = %+v, want one applied", txs)
	}
	m, err := mime.Parse(bytes.NewReader(txs[0].data), mime.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return txs[0], m
}

func addrs(list []mime.Address) []string {
	var out []string
	for _, a := range list {
		out = append(out, a.Address)
	}
	return out
}

// Cc goes in the header and the envelope, Bcc in the envelope only.
func TestSendCcAndBcc(t *testing.T) {
	rig := newSendRig(t)
	req := textRequest(opA, "hi")
	req.Recipients = &model.ReplyRecipients{Cc: []string{"carol@example.org"}, Bcc: []string{"audit@example.com"}}
	out, err := rig.inst.Send(t.Context(), req)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	tx, m := sentMessage(t, rig)
	if want := []string{"jane@example.org", "carol@example.org", "audit@example.com"}; !slices.Equal(tx.rcpts, want) {
		t.Errorf("RCPT = %v, want %v", tx.rcpts, want)
	}
	if got := addrs(m.Header.To); !slices.Equal(got, []string{"jane@example.org"}) {
		t.Errorf("To = %v", got)
	}
	if got := addrs(m.Header.Cc); !slices.Equal(got, []string{"carol@example.org"}) {
		t.Errorf("Cc = %v", got)
	}
	if strings.Contains(strings.ToLower(string(tx.data)), "audit@example.com") {
		t.Errorf("a Bcc recipient appears in the message:\n%s", tx.data)
	}
}

// Reply to all addresses the answered message's To and Cc, minus the
// mailbox itself and repeats.
func TestSendReplyAll(t *testing.T) {
	rig := newSendRig(t)
	req := textRequest(opA, "hi")
	req.Recipients = &model.ReplyRecipients{ReplyAll: true, Cc: []string{"extra@example.org"}}
	req.Thread = &adapter.Thread{Metadata: parentRecord(t,
		`{"from":[{"address":"jane@example.org"}],"to":[{"name":"Support","address":"Support@example.com"},{"address":"bob@example.org"}],"cc":[{"address":"carol@example.org"},{"address":"bob@example.org"}]}`)}
	if out, err := rig.inst.Send(t.Context(), req); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	tx, m := sentMessage(t, rig)
	if want := []string{"jane@example.org", "bob@example.org", "carol@example.org", "extra@example.org"}; !slices.Equal(tx.rcpts, want) {
		t.Errorf("RCPT = %v, want %v", tx.rcpts, want)
	}
	if got := addrs(m.Header.To); !slices.Equal(got, []string{"jane@example.org", "bob@example.org"}) {
		t.Errorf("To = %v", got)
	}
	if got := addrs(m.Header.Cc); !slices.Equal(got, []string{"carol@example.org", "extra@example.org"}) {
		t.Errorf("Cc = %v", got)
	}
}

// Reply to all of a message whose payload is unknown adds nothing.
func TestSendReplyAllWithoutParentRecipients(t *testing.T) {
	rig := newSendRig(t)
	req := textRequest(opA, "hi")
	req.Recipients = &model.ReplyRecipients{ReplyAll: true}
	if _, err := rig.inst.Send(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if tx, _ := sentMessage(t, rig); !slices.Equal(tx.rcpts, []string{"jane@example.org"}) {
		t.Errorf("RCPT = %v, want the route alone", tx.rcpts)
	}
}

func TestSendTooManyRecipientsIsRefusedBeforeSubmission(t *testing.T) {
	rig := newSendRig(t)
	var cc []string
	for i := 0; i < 50; i++ {
		cc = append(cc, fmt.Sprintf("u%d@example.org", i))
	}
	req := textRequest(opA, "hi")
	req.Recipients = &model.ReplyRecipients{Cc: cc}
	_, err := rig.inst.Send(t.Context(), req)
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("Send = %v, want validation", err)
	}
	if n := len(rig.relay.transactions()); n != 0 {
		t.Errorf("relay saw %d transactions, want none", n)
	}
}

func TestReplyRecipientsCapabilityReportsLimits(t *testing.T) {
	rig := newSendRig(t)
	statuses, err := rig.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(statuses, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityReplyRecipients })
	if i < 0 || !statuses[i].Available {
		t.Fatalf("statuses = %+v, want reply_recipients available", statuses)
	}
	l, err := model.ParseRecipientLimits(statuses[i].Limits)
	if err != nil || !l.ReplyAll || !l.Cc || !l.Bcc || l.MaxRecipients != 50 {
		t.Fatalf("limits = %+v, %v", l, err)
	}
}
