package conformance

import (
	"io"
	"slices"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

// MailHistoryProvider is implemented by the Provider of a mail adapter
// declaring history, for the cases about what a mailbox holds beyond the
// received mail the history reads. Each method returns ErrNotExpressible
// when the provider cannot hold what it describes; the case skips then.
// Without the interface the mail cases skip.
type MailHistoryProvider interface {
	HistoryProvider
	// HoldSent makes ev a message the account sent at at, in its sent
	// folder or label: addressed so that, were the sent mail read, it would
	// match the receive scope (the sender is ev's, the date in the window).
	HoldSent(channel model.ChannelRef, ev InboundEvent, at time.Time) error
	// HoldDraft makes ev a draft of the account, dated at at, held where
	// the received mail is.
	HoldDraft(channel model.ChannelRef, ev InboundEvent, at time.Time) error
	// HoldDated makes ev received mail that arrived at received and carries
	// the sender's own date dated, which differs: the sender's clock and
	// the provider's disagree.
	HoldDated(channel model.ChannelRef, ev InboundEvent, received, dated time.Time) error
	// HoldUnreadable makes ev received mail at at that the provider lists
	// and the adapter cannot turn into an envelope.
	HoldUnreadable(channel model.ChannelRef, ev InboundEvent, at time.Time) error
	// HoldAttachment makes ev received mail at at carrying one attachment,
	// held where the received mail is, whose bytes the provider serves as it
	// serves any attachment of inbound mail.
	HoldAttachment(channel model.ChannelRef, ev InboundEvent, at time.Time, a MailAttachment) error
	// ForgeHistoryCursors returns cursors derived from issued, a cursor
	// this provider's reader returned for a read of channel's received mail
	// in the window of the history cases, that widen the read it was issued
	// for: another mailbox, an earlier or later bound, other messages, a
	// position or link of the caller's choosing. The reader's scope is
	// known to the caller, so none of it guards a cursor.
	ForgeHistoryCursors(issued string) map[string]string
}

// MailAttachment is a file attached to held mail.
type MailAttachment struct {
	Name      string
	MediaType string
	Data      []byte
}

// mailHistory returns the mail provider, skipping the case without one.
func (e *adapterEnv) mailHistory(t *testing.T) MailHistoryProvider {
	t.Helper()
	p, ok := e.provider.(MailHistoryProvider)
	if !ok {
		t.Skip("provider does not implement MailHistoryProvider")
	}
	return p
}

// holdOK checks the result of a hold, skipping the case when the provider cannot express it.
func holdOK(t *testing.T, what string, err error) {
	t.Helper()
	if err == ErrNotExpressible {
		t.Skipf("provider cannot hold %s", what)
	}
	if err != nil {
		t.Fatalf("hold %s: %v", what, err)
	}
}

var adapterHistoryMailCases = []adapterCase{
	{name: "HistoryMailReadsReceivedMailOnly", needs: historyCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		mp := e.mailHistory(t)
		hr := e.historyReader(t)
		e.inbound(t)
		seed := e.message(adapterChanA, "seed", "peer-1", "support")
		wantAcked(t, e.receiveAll(t, adapterChanA, seed))
		m := findMessage(e.messages(t, adapterInboxA), adapterChanA, seed.MessageID)
		if m == nil {
			t.Skip("adapter stored no message for the event")
		}
		since := e.since()
		// The conversation's first message, as the provider holds it.
		seed = e.hold(t, "seed", "peer-1", since.Add(time.Minute))
		in1 := e.hold(t, "in-1", "peer-1", since.Add(time.Hour))
		sent := e.message(adapterChanA, "sent", "peer-1", "support")
		holdOK(t, "a sent message", mp.HoldSent(adapterChanA, sent, since.Add(2*time.Hour)))
		draft := e.message(adapterChanA, "draft", "peer-1", "support")
		holdOK(t, "a draft", mp.HoldDraft(adapterChanA, draft, since.Add(3*time.Hour)))
		in2 := e.hold(t, "in-2", "peer-1", since.Add(4*time.Hour))

		route := e.route(adapterChanA, "peer-1")
		for _, limit := range []int{1, 10} {
			e.wantWindow(t, e.readHistory(t, hr, nil, limit), seed, in1, in2)
			e.wantWindow(t, e.readHistory(t, hr, &route, limit), seed, in1, in2)
		}
		conv := m.Ref.Conversation
		rep, err := e.engine.Runtime().Import(e.ctx, runtimeCall(), uma.ImportRequest{Requester: "conformance-agent", Conversation: &conv, Since: since})
		if err != nil || rep.Imported != 2 || rep.Skipped != 1 || !rep.Complete {
			t.Fatalf("Import = %+v, %v; want the two received messages, the seed skipped", rep, err)
		}
		msgs := e.messages(t, adapterInboxA)
		for _, ev := range []InboundEvent{sent, draft} {
			if findMessage(msgs, adapterChanA, ev.MessageID) != nil {
				t.Errorf("provider message %q, outside the receive scope, was stored", ev.MessageID)
			}
		}
		e.wantNothingApplied(t)
	}},
	{name: "HistoryMailWindowIsJudgedOnReceivedTime", needs: historyCap, run: func(t *testing.T, e *adapterEnv) {
		mp := e.mailHistory(t)
		hr := e.historyReader(t)
		since, until := e.since(), e.since().Add(10*time.Hour)
		at := func(id string, received, dated time.Time) InboundEvent {
			ev := e.message(adapterChanA, id, "peer-1", "support")
			holdOK(t, "a message with its own date", mp.HoldDated(adapterChanA, ev, received, dated))
			return ev
		}
		// The sender's date says old, the provider received it inside.
		in1 := at("in-1", since.Add(time.Hour), since.Add(-48*time.Hour))
		// The sender's date says inside, the provider received it before
		// the window: out.
		at("early", since.Add(-time.Hour), since.Add(time.Hour))
		// At Until: out, whatever the sender's date says.
		at("late", until, since.Add(2*time.Hour))
		in2 := at("in-2", until.Add(-time.Hour), until.Add(48*time.Hour))
		for _, limit := range []int{1, 10} {
			// The envelopes' OccurredAt is the sender's date where the
			// provider reports one, so only the messages are compared.
			if got, want := envelopeIDs(e.readWindow(t, hr, nil, until, limit)), []string{in1.MessageID, in2.MessageID}; !slices.Equal(got, want) {
				t.Fatalf("history messages = %q, want %q: the window is the provider's received time, not the sender's date", got, want)
			}
		}
	}},
	{name: "HistoryMailForgedCursorIsValidation", needs: historyCap, run: func(t *testing.T, e *adapterEnv) {
		mp := e.mailHistory(t)
		hr := e.historyReader(t)
		since := e.since()
		for i, id := range []string{"h-1", "h-2", "h-3", "h-4"} {
			e.hold(t, id, "peer-1", since.Add(time.Duration(i+1)*time.Hour))
		}
		// A message the window excludes, that a widened read would take.
		e.hold(t, "h-old", "peer-1", since.Add(-time.Hour))
		route := e.route(adapterChanA, "peer-1")
		for name, rt := range map[string]*model.ExternalRef{"channel": nil, "conversation": &route} {
			req := adapter.HistoryRequest{Route: rt, Since: since, Limit: 1}
			p, err := hr.History(e.ctx, req)
			if err != nil || p.Next == "" {
				t.Fatalf("%s: History = %+v, %v; want a first page with more to read", name, p, err)
			}
			forged := mp.ForgeHistoryCursors(p.Next)
			if len(forged) == 0 {
				t.Skip("provider forges no cursors")
			}
			for kind, cursor := range forged {
				req.Cursor = cursor
				got, err := hr.History(e.ctx, req)
				if model.CodeOf(err) != model.Validation {
					t.Errorf("%s: %s cursor read %q, %v; want validation and no message", name, kind, envelopeIDs(got.Envelopes), err)
				}
			}
		}
		e.wantNothingApplied(t)
	}},
	{name: "HistoryMailUnreadableMessageIsAGap", needs: historyCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		mp := e.mailHistory(t)
		hr := e.historyReader(t)
		e.inbound(t)
		since := e.since()
		a := e.hold(t, "h-1", "peer-1", since.Add(time.Hour))
		bad := e.message(adapterChanA, "bad", "peer-1", "support")
		holdOK(t, "an unreadable message", mp.HoldUnreadable(adapterChanA, bad, since.Add(2*time.Hour)))
		b := e.hold(t, "h-2", "peer-1", since.Add(3*time.Hour))

		var got []model.Envelope
		var gaps int
		cursor := ""
		for range historyReadCap {
			p, err := hr.History(e.ctx, adapter.HistoryRequest{Since: since, Cursor: cursor, Limit: 1})
			if err != nil {
				t.Fatalf("History(cursor %q): %v", cursor, err)
			}
			got = append(got, p.Envelopes...)
			if p.Gap {
				gaps++
			}
			if cursor = p.Next; cursor == "" {
				break
			}
		}
		e.wantWindow(t, got, a, b)
		if gaps != 1 {
			t.Fatalf("%d pages reported a gap, want 1: the unreadable message is reported, not skipped silently", gaps)
		}
		rep, err := e.engine.Runtime().Import(e.ctx, runtimeCall(), uma.ImportRequest{Requester: "conformance-agent", Channel: &adapterChanA, Since: since})
		if err != nil || rep.Imported != 2 || rep.Gaps != 1 || !rep.Complete {
			t.Fatalf("Import = %+v, %v; want 2 imported and the gap counted", rep, err)
		}
		if slices.ContainsFunc(e.messages(t, adapterInboxA), func(m model.Message) bool {
			return slices.Contains(m.External, model.ProviderMessageRef(adapterChanA, bad.MessageID))
		}) {
			t.Errorf("unreadable message %q was stored", bad.MessageID)
		}
	}},
	{name: "HistoryMailStagesAttachmentBytes", needs: historyCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		// A host with a blob store: the import stages the attachment bytes
		// as the inbound path does, so the stored message holds them.
		blobs := &portstest.MemoryBlobs{}
		e = newAdapterEnvWith(t, e.h, envOptions{blobs: blobs})
		mp := e.mailHistory(t)
		e.inbound(t)
		since := e.since()
		plain := e.hold(t, "plain", "peer-1", since.Add(time.Hour))
		ev := e.message(adapterChanA, "files", "peer-1", "support")
		att := MailAttachment{Name: "invoice.txt", MediaType: "text/plain", Data: []byte("bytes of the invoice")}
		holdOK(t, "a message with an attachment", mp.HoldAttachment(adapterChanA, ev, since.Add(2*time.Hour), att))

		rep, err := e.engine.Runtime().Import(e.ctx, runtimeCall(), uma.ImportRequest{Requester: "conformance-agent", Channel: &adapterChanA, Since: since})
		if err != nil || rep.Imported != 2 || !rep.Complete {
			t.Fatalf("Import = %+v, %v; want both messages", rep, err)
		}
		msgs := e.messages(t, adapterInboxA)
		if m := findMessage(msgs, adapterChanA, plain.MessageID); m == nil || stagedParts(m.Content) != 0 {
			t.Fatalf("message without an attachment = %+v", m)
		}
		m := findMessage(msgs, adapterChanA, ev.MessageID)
		if m == nil {
			t.Fatalf("no message for provider message %q", ev.MessageID)
		}
		var refs []model.AttachmentRef
		for _, p := range m.Content.Parts {
			if p.Kind == model.PartAttachment && p.Attachment != nil {
				refs = append(refs, *p.Attachment)
			}
		}
		if len(refs) != 1 {
			t.Fatalf("imported message holds %d staged attachments in %+v, want 1: the bytes are staged, not only described", len(refs), m.Content.Parts)
		}
		a, rc, err := e.client.Attachments().Open(e.ctx, model.Lookup[model.AttachmentRef]{Call: runtimeCall().Call, Ref: refs[0]})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || string(got) != string(att.Data) || a.Metadata.Name != att.Name {
			t.Errorf("attachment %+v holds %q, %v; want %q named %q", a.Metadata, got, err, att.Data, att.Name)
		}
		held := len(blobs.Keys())
		if held != 1 {
			t.Fatalf("%d blobs held after the import, want 1", held)
		}
		// Reading the same history again stages the same bytes under the
		// same keys: nothing is imported and nothing is held twice.
		again, err := e.engine.Runtime().Import(e.ctx, runtimeCall(), uma.ImportRequest{Requester: "conformance-agent", Channel: &adapterChanA, Since: since})
		if err != nil || again.Imported != 0 || !again.Complete {
			t.Fatalf("second Import = %+v, %v; want nothing imported", again, err)
		}
		if n := len(blobs.Keys()); n != held {
			t.Errorf("%d blobs held after a second import, want %d", n, held)
		}
		if n := len(e.messages(t, adapterInboxA)); n != 2 {
			t.Errorf("%d messages stored, want 2", n)
		}
	}},
}

// stagedParts counts the staged attachment parts of c.
func stagedParts(c model.Content) int {
	n := 0
	for _, p := range c.Parts {
		if p.Kind == model.PartAttachment {
			n++
		}
	}
	return n
}
