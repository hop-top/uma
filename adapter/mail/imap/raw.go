package imap

import (
	"context"
	"errors"

	"github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// ReadRaw reads the message req.Message names, by its Message-ID or by the
// location a move reported, as the account holds it: the whole RFC 5322
// text with its headers and every part (adapter.RawReader). The message is
// found where a state change finds it (see locate), in a read-only
// selection and fetched with BODY.PEEK[], so nothing changes in the
// mailbox, \Seen included. A message over Config.MaxMessageBytes, the bound
// the receiver keeps, is model.Permanent with model.TooLarge before its
// body is fetched, never a truncated copy; one the account does not hold is
// model.NotFound. Nothing is kept.
func (i *Instance) ReadRaw(ctx context.Context, req adapter.RawRequest) (adapter.RawMessage, error) {
	if i.closed.Load() {
		return adapter.RawMessage{}, i.errClosed()
	}
	if !i.isMessageRef(req.Message) {
		return adapter.RawMessage{}, model.NewError(model.Validation, PurposeIMAP+": message: not a provider message of this channel")
	}
	if i.deps.Credentials == nil {
		return adapter.RawMessage{}, i.noCredentials(PurposeIMAP)
	}
	s, err := i.connect(ctx)
	if err != nil {
		return adapter.RawMessage{}, err
	}
	defer s.close()
	info, err := i.serverInfo(ctx, s)
	if err != nil {
		return adapter.RawMessage{}, err
	}
	found, err := i.locate(ctx, s, info, []model.ExternalRef{req.Message})
	if err != nil {
		return adapter.RawMessage{}, err
	}
	if len(found) == 0 {
		return adapter.RawMessage{}, errNoMessage()
	}
	at := found[0].loc
	err = s.within(func() error {
		_, err := s.c.Select(at.mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
		return err
	})
	var ie *imap.Error
	switch {
	case errors.As(err, &ie) && ctx.Err() == nil:
		return adapter.RawMessage{}, errNoMessage()
	case err != nil:
		return adapter.RawMessage{}, lost(ctx, err)
	}
	var metas []*fetchMeta
	if err := s.within(func() error {
		var err error
		metas, err = fetchMetadata(s, imap.UIDSetNum(imap.UID(at.uid)))
		return err
	}); err != nil {
		return adapter.RawMessage{}, lost(ctx, err)
	}
	if len(metas) == 0 || metas[0].uid != at.uid {
		return adapter.RawMessage{}, errNoMessage()
	}
	bound := i.cfg.maxMessageBytes()
	if metas[0].size > bound {
		return adapter.RawMessage{}, errRawTooLarge()
	}
	f, err := i.fetchMessage(ctx, s, metas[0], at.uidValidity)
	if err != nil {
		return adapter.RawMessage{}, err
	}
	switch {
	case len(f.raw) == 0:
		return adapter.RawMessage{}, errNoMessage()
	case int64(len(f.raw)) > bound:
		// The server's size and its text disagree: the bound is on the text.
		return adapter.RawMessage{}, errRawTooLarge()
	}
	return adapter.RawMessage{MediaType: model.MediaTypeRawMessage, Bytes: f.raw}, nil
}

func errNoMessage() error {
	return model.NewError(model.NotFound, PurposeIMAP+": no such message in the mailbox")
}

func errRawTooLarge() error {
	return model.NewReasonError(model.Permanent, model.TooLarge, PurposeIMAP+": message is over the size limit of a read")
}
