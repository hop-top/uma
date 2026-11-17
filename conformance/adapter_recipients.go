package conformance

import (
	"slices"
	"testing"

	"hop.top/uma/model"
)

// adapterRecipientsCases cover reply recipients (reply to all, Cc, Bcc):
// an adapter declaring reply_recipients reports readable limits with the
// capability, and discovery carries them to a host.
var adapterRecipientsCases = []adapterCase{
	{name: "RecipientLimitsReported", needs: []model.Capability{model.CapabilityReplyRecipients}, run: func(t *testing.T, e *adapterEnv) {
		e.seed(t, adapterChanA, adapterInboxA, e.selection(), "support")
		st := adapterStatus(t, e, e.open(t, adapterChanA), model.CapabilityReplyRecipients)
		if !st.Available {
			t.Skipf("reply_recipients is %+v", st)
		}
		l, err := model.ParseRecipientLimits(st.Limits)
		if err != nil {
			t.Fatalf("limits %+v: %v, want readable recipient limits", st.Limits, err)
		}
		if !l.ReplyAll && !l.Cc && !l.Bcc {
			t.Fatalf("limits %+v support nothing: a declared capability supports reply all, cc or bcc", l)
		}
		discovered, err := e.client.Configuration().Capabilities(e.ctx, model.Lookup[model.ChannelRef]{
			Call: model.Call{Correlation: "c-discovery"}, Ref: adapterChanA,
		})
		if err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
		i := slices.IndexFunc(discovered, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityReplyRecipients })
		if i < 0 {
			t.Fatalf("discovery %+v lacks reply_recipients", discovered)
		}
		if got, err := model.ParseRecipientLimits(discovered[i].Limits); err != nil || got != l {
			t.Fatalf("discovered limits %+v (%v), want the adapter's %+v", discovered[i].Limits, err, l)
		}
	}},
}

// adapterRecipientsUnsupportedCases: without reply_recipients a reply naming
// recipients is refused before any provider call, and a reply naming none is
// unaffected.
var adapterRecipientsUnsupportedCases = []adapterCase{
	{name: "ReplyRecipientsWithoutCapability", absent: []model.Capability{model.CapabilityReplyRecipients}, needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		_, err := e.client.Messages().Reply(e.ctx, model.Reply{
			Command:      model.Command{Call: model.Call{Correlation: "c-recipients"}, Operation: "op-recipients"},
			Conversation: conv.Ref,
			Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "reply"}}},
			Recipients:   &model.ReplyRecipients{Cc: []string{"cc@example.test"}},
		})
		if model.CodeOf(err) != model.Unsupported {
			t.Fatalf("Reply with recipients on a channel without reply_recipients = %v, want unsupported", err)
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none", calls)
		}
	}},
}
