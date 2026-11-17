package sms

import (
	"context"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// History reads a window of the messages the channel's number received,
// oldest first (twilio.Client.ListInbound): the conversation's on Route,
// whose ID is the sender's number, or every sender's without one. Each is
// the envelope the webhook reports for the same message, occurring when
// Twilio received it (date_sent), within the window Since <= t < Until
// (Until zero is open; Twilio dates have one-second resolution), so the engine imports a message the
// webhook already delivered once. An MMS carries its media count only:
// the listing names no content types, and history stages no media.
//
// Only inbound messages are history. The channel's own sends are
// outbound messages, already stored from Send and Reconcile with their
// message SIDs and never reported by a webhook as received; an envelope
// has no form for a message the channel wrote, so outbound messages
// another client of the account sent are left out too.
//
// A channel sending through a Messaging Service only has no number of its
// own to list, and history is unsupported there. Next is the reader's own
// cursor; any other cursor, such as a Twilio page URI, is
// model.Validation, refused before any call.
func (i *Instance) History(ctx context.Context, req adapter.HistoryRequest) (adapter.HistoryPage, error) {
	if i.isClosed() {
		return adapter.HistoryPage{}, model.NewError(model.Unavailable, "instance closed")
	}
	if req.Limit < 1 {
		return adapter.HistoryPage{}, &model.Error{Code: model.Validation, SafeMessage: "history limit must be at least 1", Retry: model.NoRetry}
	}
	after, err := twilio.ParseCursor(req.Cursor)
	if err != nil {
		return adapter.HistoryPage{}, err
	}
	var from string
	if r := req.Route; r != nil {
		if r.Channel != i.channel.Ref || r.Namespace != Namespace || r.Kind != RouteKind || r.ID == "" {
			return adapter.HistoryPage{}, &model.Error{Code: model.Validation, SafeMessage: "route is not a twilio.sms phone route of this channel", Retry: model.NoRetry}
		}
		from = r.ID
	}
	if i.cfg.From == "" {
		return adapter.HistoryPage{}, &model.Error{Code: model.Unsupported, SafeMessage: "history needs the channel's own number; a Messaging Service alone names none", Retry: model.NoRetry}
	}
	if i.rest == nil {
		return adapter.HistoryPage{}, &model.Error{Code: model.Unavailable, SafeMessage: "no HTTP client for Twilio", Retry: model.NoRetry}
	}
	cred, err := i.resolve(ctx, PurposeREST, ports.KindSecret, ports.KindPassword)
	if err != nil {
		return adapter.HistoryPage{}, err
	}
	page, err := i.rest.ListInbound(ctx, i.cfg.AccountSID, cred, twilio.InboundQuery{
		To: i.cfg.From, From: from, Since: req.Since, Until: req.Until, After: after, Limit: req.Limit,
	})
	if err != nil {
		return adapter.HistoryPage{}, err
	}
	out := adapter.HistoryPage{Next: page.Next, Gap: page.Gap}
	for _, m := range page.Messages {
		at, _ := m.SentAt() // ListInbound returns dated messages only
		var media *Media
		if n, err := strconv.Atoi(m.NumMedia); err == nil && n > 0 {
			media = &Media{Count: n}
		}
		env, ok := i.messageEnvelope(m.SID, m.From, m.To, m.Body, media, at)
		if !ok {
			return adapter.HistoryPage{}, model.NewError(model.Permanent, "Twilio message not representable")
		}
		out.Envelopes = append(out.Envelopes, env)
	}
	return out, nil
}

var _ adapter.HistoryReader = (*Instance)(nil)
