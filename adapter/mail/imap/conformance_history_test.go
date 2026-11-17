package imap_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

var _ conformance.MailHistoryProvider = (*provider)(nil)

// holdRaw appends the mail of ev to mailbox, arrived at at, with the
// sender's Date header dated and without the headers in drop.
func (p *provider) holdRaw(ch model.ChannelRef, ev conformance.InboundEvent, mailbox string, at, dated time.Time, from bool) {
	a := p.account(ch)
	if !slices.ContainsFunc(a.srv.boxList(), func(b box) bool { return b.name == mailbox }) {
		a.srv.create(mailbox, goimap.MailboxAttrDrafts)
	}
	raw := ""
	if from {
		raw += "From: " + ev.Sender + "\r\n"
	}
	raw += "To: desk@" + domain(ch) + "\r\nSubject: hello\r\nMessage-ID: " + ev.MessageID +
		"\r\nDate: " + dated.UTC().Format(time.RFC1123Z) + "\r\n\r\n" + ev.Text + "\r\n"
	a.srv.deliver(mailbox, []byte(raw), at)
}

func (p *provider) HoldSent(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	p.holdRaw(ch, ev, "Sent", at, at, true)
	return nil
}

// HoldDraft puts the draft in Drafts: IMAP holds drafts in a mailbox of
// their own, which the receive scope never names.
func (p *provider) HoldDraft(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	p.holdRaw(ch, ev, "Drafts", at, at, true)
	return nil
}

func (p *provider) HoldDated(ch model.ChannelRef, ev conformance.InboundEvent, received, dated time.Time) error {
	p.holdRaw(ch, ev, "INBOX", received, dated, true)
	return nil
}

// HoldUnreadable is mail without a sender, which cannot be delivered.
func (p *provider) HoldUnreadable(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	p.holdRaw(ch, ev, "INBOX", at, at, false)
	return nil
}

// HoldAttachment appends the mail to INBOX as a multipart message with a
// base64 part.
func (p *provider) HoldAttachment(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time, a conformance.MailAttachment) error {
	raw := fmt.Sprintf("From: %s\r\nTo: desk@%s\r\nSubject: hello\r\nMessage-ID: %s\r\nDate: %s\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\n%s\r\n"+
		"--b\r\nContent-Type: %s; name=%q\r\nContent-Disposition: attachment; filename=%q\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n--b--\r\n",
		ev.Sender, domain(ch), ev.MessageID, at.UTC().Format(time.RFC1123Z), ev.Text, a.MediaType, a.Name, a.Name, base64.StdEncoding.EncodeToString(a.Data))
	p.account(ch).srv.deliver("INBOX", []byte(raw), at)
	return nil
}

// ForgeHistoryCursors names another mailbox, or a date outside the window.
func (p *provider) ForgeHistoryCursors(issued string) map[string]string {
	raw, err := base64.RawURLEncoding.DecodeString(issued)
	if err != nil {
		return nil
	}
	var base map[string]any
	if json.Unmarshal(raw, &base) != nil {
		return nil
	}
	forge := func(edit func(map[string]any)) string {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		edit(m)
		b, _ := json.Marshal(m)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return map[string]string{
		"sent mailbox":      forge(func(m map[string]any) { m["mailbox"] = "Sent" }),
		"no mailbox":        forge(func(m map[string]any) { m["mailbox"] = "" }),
		"date before since": forge(func(m map[string]any) { m["last_date"] = 1 }),
	}
}
