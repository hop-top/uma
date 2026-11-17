package google

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

var _ conformance.MailHistoryProvider = (*gmailProvider)(nil)

// holdRaw puts raw in the account as mail of ev, received at at, with labels.
func (p *gmailProvider) holdRaw(ch model.ChannelRef, ev conformance.InboundEvent, raw []byte, at time.Time, labels ...string) {
	p.mu.Lock()
	p.init(ch)
	p.events[ch][ev.MessageID] = ev.EventID
	p.mu.Unlock()
	p.box(ch)
	p.g.hold(mailboxOf(ch), ev.MessageID, "thread-"+ev.Room, raw, at, labels...)
}

func (p *gmailProvider) heldRaw(ch model.ChannelRef, ev conformance.InboundEvent) []byte {
	return rawMessage(ev.Sender, ev.Destination, "<"+ev.MessageID+"@peers.test>", "hello", ev.Text)
}

func (p *gmailProvider) HoldSent(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	p.holdRaw(ch, ev, p.heldRaw(ch, ev), at, labelSent)
	return nil
}

func (p *gmailProvider) HoldDraft(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	p.holdRaw(ch, ev, p.heldRaw(ch, ev), at, labelInbox, labelDraft)
	return nil
}

// HoldDated gives the mail the sender's Date header dated; Gmail dates it
// by its own internalDate, received.
func (p *gmailProvider) HoldDated(ch model.ChannelRef, ev conformance.InboundEvent, received, dated time.Time) error {
	raw := bytes.Replace(p.heldRaw(ch, ev), []byte("Date: Sun, 11 Oct 2026 12:00:00 +0000"), []byte("Date: "+dated.UTC().Format(time.RFC1123Z)), 1)
	p.holdRaw(ch, ev, raw, received)
	return nil
}

// HoldUnreadable lists the mail and answers its raw fetch with a body that
// can never be decoded.
func (p *gmailProvider) HoldUnreadable(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	p.holdRaw(ch, ev, p.heldRaw(ch, ev), at)
	answerFor(p.g, mailboxOf(ch), ev.MessageID, "raw", 200, undecodableRaw(ev.MessageID), always)
	return nil
}

// HoldAttachment holds the mail as a multipart message with a base64 part.
func (p *gmailProvider) HoldAttachment(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time, a conformance.MailAttachment) error {
	raw := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: hello\r\nDate: Sun, 11 Oct 2026 12:00:00 +0000\r\nMessage-ID: <%s@peers.test>\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\n%s\r\n"+
		"--b\r\nContent-Type: %s; name=%q\r\nContent-Disposition: attachment; filename=%q\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n--b--\r\n",
		ev.Sender, ev.Destination, ev.MessageID, ev.Text, a.MediaType, a.Name, a.Name, base64.StdEncoding.EncodeToString(a.Data))
	p.holdRaw(ch, ev, []byte(raw), at)
	return nil
}

// ForgeHistoryCursors rewrites the cursor's bounds: the scope hash is
// computed from what the caller knows, so only the bounds are checked.
func (p *gmailProvider) ForgeHistoryCursors(issued string) map[string]string {
	b, err := base64.RawURLEncoding.DecodeString(issued)
	if err != nil {
		return nil
	}
	var base historyCursor
	if json.Unmarshal(b, &base) != nil {
		return nil
	}
	forge := func(edit func(*historyCursor)) string {
		c := base
		edit(&c)
		return c.encode()
	}
	return map[string]string{
		"lo before since": forge(func(c *historyCursor) { c.Lo = 1 }),
		"lo negative":     forge(func(c *historyCursor) { c.Lo = -5 }),
		"hi after now":    forge(func(c *historyCursor) { c.Hi += 86400 * 365 }),
	}
}
