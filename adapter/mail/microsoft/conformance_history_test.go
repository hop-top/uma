package microsoft

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

var _ conformance.MailHistoryProvider = (*graphProvider)(nil)

// holdIn holds ev in folder as mail received at at, with edit applied.
func (p *graphProvider) holdIn(ch model.ChannelRef, ev conformance.InboundEvent, folder string, at time.Time, edit func(*gMessage)) error {
	if ev.Visibility != "" && ev.Visibility != model.Public {
		return conformance.ErrNotExpressible
	}
	mb := p.box(ch)
	m := gMessage{
		ID: ev.MessageID, InternetMessageID: "<" + ev.MessageID + "@peer.test>", ConversationID: "conv-" + ev.Sender,
		Subject: "subject " + ev.MessageID, ReceivedDateTime: at,
		From:         &gRecipient{EmailAddress: gEmailAddress{Address: ev.Sender}},
		ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: ev.Destination}}},
		Body:         &gItemBody{ContentType: "text", Content: ev.Text},
	}
	if edit != nil {
		edit(&m)
	}
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	mb.order = append(mb.order, ev.MessageID)
	mb.messages[ev.MessageID] = &fakeMessage{folder: folder, modified: fakeStamp(), m: m}
	mb.events[ev.MessageID] = ev.EventID
	return nil
}

func (p *graphProvider) HoldSent(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	return p.holdIn(ch, ev, "sentitems", at, nil)
}

// HoldDraft puts the draft in the inbox, where the list sees it.
func (p *graphProvider) HoldDraft(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	return p.holdIn(ch, ev, "inbox", at, func(m *gMessage) { m.IsDraft = true })
}

// HoldDated carries the sender's date as the message's Date header; Graph
// dates the message by receivedDateTime.
func (p *graphProvider) HoldDated(ch model.ChannelRef, ev conformance.InboundEvent, received, dated time.Time) error {
	return p.holdIn(ch, ev, "inbox", received, func(m *gMessage) {
		m.InternetMessageHeaders = []gHeader{{Name: "Date", Value: dated.UTC().Format(time.RFC1123Z)}}
	})
}

// HoldUnreadable lists an item with no ID, which cannot be addressed.
func (p *graphProvider) HoldUnreadable(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	return p.holdIn(ch, ev, "inbox", at, func(m *gMessage) { m.ID = "" })
}

// HoldAttachment holds the message with a file attachment the fake serves
// the raw bytes of.
func (p *graphProvider) HoldAttachment(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time, a conformance.MailAttachment) error {
	if err := p.holdIn(ch, ev, "inbox", at, func(m *gMessage) { m.HasAttachments = true }); err != nil {
		return err
	}
	id := "att-" + ev.MessageID
	mb := p.box(ch)
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	mb.messages[ev.MessageID].atts = []gAttachment{{ODataType: fileType, ID: id, Name: a.Name, ContentType: a.MediaType, Size: int64(len(a.Data))}}
	p.g.files[id] = a.Data
	return nil
}

// ForgeHistoryCursors edits the paging parameter, token and page size: the
// cursor must hold nothing but the position in the list it was issued for.
func (p *graphProvider) ForgeHistoryCursors(issued string) map[string]string {
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
		"link": forge(func(m map[string]any) {
			m["link"] = "https://graph.example.test/v1.0/users/victim@contoso.test/mailFolders/sentitems/messages?$skiptoken=1"
		}),
		"other param": forge(func(m map[string]any) { m["p"] = "$filter" }),
		"no param":    forge(func(m map[string]any) { delete(m, "p") }),
		"big top":     forge(func(m map[string]any) { m["s"] = 100000 }),
		"no top":      forge(func(m map[string]any) { delete(m, "s") }),
		"huge token":  forge(func(m map[string]any) { m["t"] = strings.Repeat("x", 5000) }),
	}
}
