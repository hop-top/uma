package imap

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/recipients"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Send composes a text/plain message to the route address with a Message-ID
// derived from the operation (stable across attempts), replying to the
// effect's target message when it names one, and submits it through the
// submission server with the channel's credential (purpose
// PurposeSubmission). Submission replies are definitive: 2xx is
// provider_accepted with the Message-ID as provider reference, 4xx
// transient, 5xx rejected, and no reply after the end of DATA
// outcome_unknown. A send carrying recipients (model.ReplyRecipients) also
// addresses Cc, Bcc (by envelope only) and, replying to all, the To and Cc of
// the message it answers (see smtp.Recipients). Once accepted, a copy is appended to the sent-copies
// mailbox unless the provider saves one itself; a failed append leaves the
// outcome accepted.
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
	rcpts, err := smtp.Recipients(req, to, []string{i.cfg.Address}, MessageSchema)
	if err != nil {
		return adapter.Outcome{}, err
	}
	msgID := i.messageID(req.Operation)
	th := smtp.ThreadOf(i.ch.Ref, req, MessageSchema, i.cfg.Subject)
	out := mime.Outgoing{
		From:      mime.Address{Name: i.cfg.FromName, Address: i.cfg.Address},
		To:        recipients.Addresses(rcpts.To),
		Cc:        recipients.Addresses(rcpts.Cc),
		Subject:   th.Subject,
		Date:      i.now(),
		MessageID: msgID,
		Text:      text,
		InReplyTo: th.InReplyTo, References: th.References,
	}
	var body bytes.Buffer
	if err := mime.Compose(&body, out); err != nil {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "imap.mail: message cannot be composed", Retry: model.NoRetry}
	}
	if i.deps.Credentials == nil {
		return adapter.Outcome{}, i.noCredentials(PurposeSubmission)
	}
	res, err := smtp.Submission{
		Credentials: i.deps.Credentials,
		Request: ports.CredentialRequest{
			Channel: i.ch.Ref, Reference: i.ch.CredentialRef, Purpose: PurposeSubmission, Accept: accept,
		},
		Server:   i.cfg.Submission.relay(),
		Hostname: i.cfg.messageIDDomain(),
		RootCAs:  i.f.opts.RootCAs,
	}.Submit(ctx, i.cfg.Address, rcpts.All(), body.Bytes())
	if err != nil || res.State != model.ProviderAccepted {
		return res, err
	}
	if i.cfg.Sent.Mailbox != "" && !i.cfg.Sent.SavedByProvider {
		_ = i.appendSent(ctx, body.Bytes(), out)
	}
	res.References = []model.ExternalRef{model.ProviderMessageRef(i.ch.Ref, msgID)}
	return res, nil
}

// appendSent files the sent copy, marked \Seen, in the sent-copies mailbox.
func (i *Instance) appendSent(ctx context.Context, raw []byte, out mime.Outgoing) error {
	s, err := i.connect(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	return s.within(func() error {
		cmd := s.c.Append(i.cfg.Sent.Mailbox, int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}, Time: out.Date})
		if _, err := cmd.Write(raw); err != nil {
			_ = cmd.Close()
			return err
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		_, err := cmd.Wait()
		return err
	})
}

// Reconcile looks the effect's Message-ID up in the sent-copies mailbox.
// Found, the submission was accepted. Not found, the attempt was never
// accepted when the provider saves every submitted message itself (a new
// attempt is safe); otherwise absence proves nothing and the outcome stays
// unknown, so the message is never resent. A provider state effect is
// reconciled by reading the mailbox (see reconcileState).
func (i *Instance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	e := req.Effect
	if i.closed.Load() {
		return adapter.Outcome{}, i.errClosed()
	}
	if isStateCapability(req.Capability) {
		return i.reconcileState(ctx, req)
	}
	if i.cfg.Sent.Mailbox == "" {
		return adapter.Outcome{}, &model.Error{Code: model.Unsupported, SafeMessage: "imap.mail: no sent-copies mailbox configured", Retry: model.NoRetry}
	}
	msgID := i.messageID(e.Operation)
	s, err := i.connect(ctx)
	if err != nil {
		return adapter.Outcome{}, err
	}
	defer s.close()
	found, err := s.findSent(i.cfg.Sent.Mailbox, msgID)
	if err != nil {
		return adapter.Outcome{}, lost(ctx, err)
	}
	switch {
	case found:
		return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMessageRef(i.ch.Ref, msgID)}}, nil
	case i.cfg.Sent.SavedByProvider:
		return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.Transient, PurposeSubmission+": no sent copy of the attempt")}, nil
	}
	return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, PurposeSubmission+": sent copies cannot prove a lost submission")}, nil
}

// findSent reports whether mailbox holds a message whose Message-ID is
// exactly msgID. SEARCH HEADER matches substrings, so each hit's header is
// checked.
func (s *session) findSent(mailbox, msgID string) (bool, error) {
	var hit bool
	err := s.within(func() error {
		if _, err := s.c.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return err
		}
		data, err := s.c.UIDSearch(&imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: msgID}},
		}, nil).Wait()
		if err != nil {
			return err
		}
		uids := data.AllUIDs()
		if len(uids) == 0 {
			return nil
		}
		section := &imap.FetchItemBodySection{Peek: true, Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Message-ID"}}
		msgs, err := s.c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if hdr, err := mime.Parse(bytes.NewReader(m.FindBodySection(section)), mime.Limits{}); err == nil {
				if ids := model.ParseMsgIDs(hdr.Header.MessageID); len(ids) > 0 && ids[0] == msgID {
					hit = true
				}
			}
		}
		return nil
	})
	var ie *imap.Error
	if errors.As(err, &ie) {
		// NO or BAD, typically a missing mailbox: inconclusive, never
		// evidence of absence.
		return false, model.NewError(model.Unavailable, PurposeIMAP+": sent-copies mailbox cannot be searched")
	}
	return hit, err
}

// plainText joins the text parts. Attachment and structured parts cannot be
// sent: the adapter holds no attachment bytes and renders no structured
// schema.
func plainText(c model.Content) (string, error) {
	if len(c.Parts) == 0 {
		return "", &model.Error{Code: model.Validation, SafeMessage: "imap.mail: content has no parts", Retry: model.NoRetry}
	}
	texts := make([]string, 0, len(c.Parts))
	for _, p := range c.Parts {
		if p.Kind != model.PartText {
			return "", &model.Error{Code: model.Unsupported, SafeMessage: "imap.mail: only text parts can be sent", Retry: model.NoRetry}
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n\n"), nil
}

// routeAddress returns the address of a route on this channel.
func (i *Instance) routeAddress(r model.ExternalRef) (string, error) {
	bad := &model.Error{Code: model.Validation, SafeMessage: "imap.mail: route is not a mailbox address on this channel", Retry: model.NoRetry}
	if r.Channel != i.ch.Ref || r.Namespace != Namespace || r.Kind != RouteKind {
		return "", bad
	}
	norm, err := model.NormalizeEmail(r.ID)
	if err != nil || norm != r.ID {
		return "", bad
	}
	return r.ID, nil
}

func (i *Instance) messageID(op model.OperationRef) string {
	return smtp.MessageID(op, i.cfg.messageIDDomain())
}
