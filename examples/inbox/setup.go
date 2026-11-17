package main

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"

	"hop.top/uma"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

// Credential references the channels carry; the host's credential port
// resolves them.
const (
	widgetKeyRef = "widget-session-key"
	relayRef     = "mail-relay"
)

// setup configures the widget and mail channels and one inbox on each,
// through the Configuration service. It is idempotent across restarts and
// across hosts sharing the database: an existing channel or inbox is
// reused, replaced only when its configuration changed, and creation uses
// a fixed operation ID so concurrent hosts converge on one record.
func (h *Host) setup(ctx context.Context) error {
	admin, err := h.engine.For(model.Principal{Tenant: h.cfg.Tenant, Actor: setupActor})
	if err != nil {
		return err
	}
	cfg := admin.Configuration()
	mailCfg := smtp.Config{
		Hostname:   h.cfg.MailHostname,
		Recipients: []smtp.Recipient{{Address: h.cfg.MailAddress, Destination: h.cfg.Site}},
		From:       h.cfg.MailAddress,
		FromName:   "Support",
		Subject:    "Re: your message",
		Relay:      h.cfg.Relay,
	}
	if h.widgetChannel, err = h.ensureChannel(ctx, cfg, model.Channel{
		Type: widget.Type, Configuration: widget.Configuration(h.cfg.Site), CredentialRef: widgetKeyRef,
	}); err != nil {
		return err
	}
	if h.mailChannel, err = h.ensureChannel(ctx, cfg, model.Channel{
		Type: model.TypeSmtpMail, Configuration: mailCfg.Extension(), CredentialRef: relayRef,
	}); err != nil {
		return err
	}
	// Chat keeps one open conversation per visitor; mail threads by
	// In-Reply-To and References, so a new topic is a new conversation.
	if h.widgetInbox, err = h.ensureInbox(ctx, cfg, model.Inbox{
		Channel: h.widgetChannel, Name: "Website chat", Destinations: []string{h.cfg.Site}, Selection: perContact(true),
	}); err != nil {
		return err
	}
	h.mailInbox, err = h.ensureInbox(ctx, cfg, model.Inbox{
		Channel: h.mailChannel, Name: "Support mail", Destinations: []string{h.cfg.Site}, Selection: perContact(false),
	})
	return err
}

func perContact(lock bool) model.Selection {
	return model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
}

func (h *Host) ensureChannel(ctx context.Context, cfg uma.Configuration, want model.Channel) (model.ChannelRef, error) {
	want.Ref.Tenant = h.cfg.Tenant
	var found *model.Channel
	err := pages(ctx, cfg.Channels, func(ch model.Channel) bool {
		if ch.Type == want.Type {
			found = &ch
			return false
		}
		return true
	})
	if err != nil {
		return model.ChannelRef{}, err
	}
	cmd := model.Command{Call: model.Call{Correlation: "setup"}, Operation: model.OperationID("setup/channel/" + string(want.Type))}
	if found != nil {
		if sameJSON(found.Configuration, want.Configuration) && found.CredentialRef == want.CredentialRef {
			return found.Ref, nil
		}
		want.Ref = found.Ref
		cmd.ExpectedRevision = found.Revision
		if cmd.Operation, err = h.newOperation(ctx); err != nil {
			return model.ChannelRef{}, err
		}
	}
	acc, err := cfg.ConfigureChannel(ctx, model.ConfigureChannel{Command: cmd, Channel: want})
	if err != nil {
		return model.ChannelRef{}, err
	}
	return model.ChannelRef{Tenant: h.cfg.Tenant, ID: acc.Operation.Result.ID}, nil
}

func (h *Host) ensureInbox(ctx context.Context, cfg uma.Configuration, want model.Inbox) (model.InboxRef, error) {
	want.Ref.Tenant = h.cfg.Tenant
	var found *model.Inbox
	err := pages(ctx, cfg.Inboxes, func(in model.Inbox) bool {
		if in.Channel == want.Channel {
			found = &in
			return false
		}
		return true
	})
	if err != nil {
		return model.InboxRef{}, err
	}
	cmd := model.Command{Call: model.Call{Correlation: "setup"}, Operation: model.OperationID("setup/inbox/" + string(want.Channel.ID))}
	if found != nil {
		if found.Name == want.Name && slices.Equal(found.Destinations, want.Destinations) &&
			reflect.DeepEqual(found.Selection, want.Selection) && !found.Disabled {
			return found.Ref, nil
		}
		want.Ref = found.Ref
		cmd.ExpectedRevision = found.Revision
		if cmd.Operation, err = h.newOperation(ctx); err != nil {
			return model.InboxRef{}, err
		}
	}
	acc, err := cfg.ConfigureInbox(ctx, model.ConfigureInbox{Command: cmd, Inbox: want})
	if err != nil {
		return model.InboxRef{}, err
	}
	return model.InboxRef{Tenant: h.cfg.Tenant, ID: acc.Operation.Result.ID}, nil
}

// pages walks an authorized list until visit returns false or the list is
// complete. A page may be short or empty and still carry a cursor.
func pages[T any](ctx context.Context, list func(context.Context, model.ListRequest) (model.Page[T], error), visit func(T) bool) error {
	var after model.Cursor
	for {
		p, err := list(ctx, model.ListRequest{Call: model.Call{Correlation: "setup"}, Page: model.PageRequest{Limit: uma.MaxPageLimit, After: after}})
		if err != nil {
			return err
		}
		for _, item := range p.Items {
			if !visit(item) {
				return nil
			}
		}
		if p.Complete || p.Next == "" {
			return nil
		}
		after = p.Next
	}
}

// sameJSON compares two configurations by value: the store may keep JSON
// in another key order or spacing than it was written.
func sameJSON(a, b model.Extension) bool {
	if a.Name != b.Name || a.Version != b.Version {
		return false
	}
	var x, y any
	if json.Unmarshal(a.Data, &x) != nil || json.Unmarshal(b.Data, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// newOperation returns a fresh operation ID for a command that must not
// replay an earlier one.
func (h *Host) newOperation(ctx context.Context) (model.OperationID, error) {
	id, err := h.ids.New(ctx)
	return model.OperationID(id), err
}
