package smtp

import (
	"bytes"
	"context"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/recipients"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// relayAccept are the credential kinds relay submission can use.
var relayAccept = []ports.CredentialKind{ports.KindPassword, ports.KindOAuth2, ports.KindClientCert}

// Send submits the content to the route address through the relay. A send
// carrying recipients (model.ReplyRecipients) also addresses the message's Cc
// and, by envelope only, Bcc recipients, and a reply to all addresses those
// of the message it answers (see Recipients); a recipient the relay refuses
// ends the submission as a whole.
func (i *Instance) Send(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error) {
	if i.closed.Load() {
		return adapter.Outcome{}, i.errClosed()
	}
	text, err := plainText(req.Content)
	if err != nil {
		return adapter.Outcome{}, err
	}
	to, err := i.routeAddress(req.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	rcpts, err := Recipients(req, to, []string{i.cfg.From}, MessageSchema)
	if err != nil {
		return adapter.Outcome{}, err
	}
	msgID := i.messageID(req.Operation)
	th := ThreadOf(i.ch.Ref, req, MessageSchema, i.cfg.Subject)
	out := mime.Outgoing{
		From:      mime.Address{Name: i.cfg.FromName, Address: i.cfg.From},
		To:        recipients.Addresses(rcpts.To),
		Cc:        recipients.Addresses(rcpts.Cc),
		Subject:   th.Subject,
		Date:      i.deps.Clock.Now(),
		MessageID: msgID,
		Text:      text,
		InReplyTo: th.InReplyTo, References: th.References,
	}
	var body bytes.Buffer
	if err := mime.Compose(&body, out); err != nil {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "smtp.mail: message cannot be composed", Retry: model.NoRetry}
	}
	res, err := Submission{
		Credentials: i.deps.Credentials,
		Request: ports.CredentialRequest{
			Channel: i.ch.Ref, Reference: i.ch.CredentialRef, Purpose: PurposeRelay, Accept: relayAccept,
		},
		Server:   i.cfg.Relay,
		Hostname: i.cfg.Hostname,
		RootCAs:  i.opts.RootCAs,
	}.Submit(ctx, i.cfg.From, rcpts.All(), body.Bytes())
	if err != nil || res.State != model.ProviderAccepted {
		return res, err
	}
	res.References = []model.ExternalRef{model.ProviderMessageRef(i.ch.Ref, msgID)}
	return res, nil
}

// plainText joins the text parts. Attachment and structured parts cannot be
// sent: the adapter holds no attachment bytes and renders no structured
// schema.
func plainText(c model.Content) (string, error) {
	if len(c.Parts) == 0 {
		return "", &model.Error{Code: model.Validation, SafeMessage: "smtp.mail: content has no parts", Retry: model.NoRetry}
	}
	texts := make([]string, 0, len(c.Parts))
	for _, p := range c.Parts {
		if p.Kind != model.PartText {
			return "", &model.Error{Code: model.Unsupported, SafeMessage: "smtp.mail: only text parts can be sent", Retry: model.NoRetry}
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n\n"), nil
}

// routeAddress returns the address of a route on this channel.
func (i *Instance) routeAddress(r model.ExternalRef) (string, error) {
	bad := &model.Error{Code: model.Validation, SafeMessage: "smtp.mail: route is not a mailbox address on this channel", Retry: model.NoRetry}
	if r.Channel != i.ch.Ref || r.Namespace != Namespace || r.Kind != RouteKind {
		return "", bad
	}
	norm, err := model.NormalizeEmail(r.ID)
	if err != nil || norm != r.ID {
		return "", bad
	}
	return r.ID, nil
}

// messageID derives the Message-ID of an operation (see MessageID).
func (i *Instance) messageID(op model.OperationRef) string {
	return MessageID(op, i.cfg.messageIDDomain())
}
