package smtp

import (
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func TestThreadOf(t *testing.T) {
	ch := model.ChannelRef{Tenant: "t", ID: "00000000-0000-7000-8000-0000000000c1"}
	other := model.ChannelRef{Tenant: "t", ID: "00000000-0000-7000-8000-0000000000c2"}
	hdr := func(kind, id string) model.ExternalRef {
		return model.ExternalRef{Channel: ch, Namespace: model.EmailHeaderNamespace, Kind: kind, ID: id}
	}
	payload := func(subject string) model.Extension {
		data, _ := json.Marshal(map[string]any{"headers": map[string]string{"subject": subject}})
		return model.Extension{Name: MessageSchema, Version: 1, Data: data}
	}
	parent := model.ProviderMessageRef(ch, "<p1@peer.example>")
	for name, c := range map[string]struct {
		req  adapter.SendRequest
		want Thread
	}{
		"no parent": {adapter.SendRequest{}, Thread{Subject: "Support"}},
		"target only": {adapter.SendRequest{Effect: adapter.Effect{Target: &parent}}, Thread{
			InReplyTo: "<p1@peer.example>", References: []string{"<p1@peer.example>"}, Subject: "Support",
		}},
		"full thread": {adapter.SendRequest{Effect: adapter.Effect{Target: &parent}, Thread: &adapter.Thread{
			References: []model.ExternalRef{
				parent, hdr(model.RefKindEmailMessageID, "<p1@peer.example>"),
				hdr(model.RefKindEmailReferences, "<r1@a.example>"), hdr(model.RefKindEmailReferences, "<r2@a.example>"),
				{Channel: other, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailReferences, ID: "<x@other.example>"},
			},
			Metadata: []model.Extension{payload("Order 7")},
		}}, Thread{
			InReplyTo: "<p1@peer.example>", References: []string{"<r1@a.example>", "<r2@a.example>", "<p1@peer.example>"}, Subject: "Re: Order 7",
		}},
		"already a reply subject": {adapter.SendRequest{Thread: &adapter.Thread{
			References: []model.ExternalRef{hdr(model.RefKindEmailMessageID, "<p2@peer.example>")},
			Metadata:   []model.Extension{payload("RE: Order 7")},
		}}, Thread{InReplyTo: "<p2@peer.example>", References: []string{"<p2@peer.example>"}, Subject: "RE: Order 7"}},
		"multiline subject refused": {adapter.SendRequest{Thread: &adapter.Thread{Metadata: []model.Extension{payload("a\r\nBcc: x@y")}}}, Thread{Subject: "Support"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := ThreadOf(ch, c.req, MessageSchema, "Support")
			if got.InReplyTo != c.want.InReplyTo || !slices.Equal(got.References, c.want.References) || got.Subject != c.want.Subject {
				t.Fatalf("ThreadOf = %+v, want %+v", got, c.want)
			}
		})
	}
}
