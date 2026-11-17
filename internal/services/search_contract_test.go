package services_test

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"hop.top/uma/internal/services"
	"hop.top/uma/model"
)

// searchFilters sets each message search field alone, keyed by its wire
// name.
func searchFilters() map[string]model.Filter {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	yes := true
	return map[string]model.Filter{
		"text":        {Text: "invoice"},
		"since":       {Since: &t0},
		"before":      {Before: &t0},
		"attachments": {Attachments: &model.AttachmentFilter{Has: &yes}},
	}
}

// Every list query but Messages.List rejects the message search fields
// with Validation, never ignoring them.
func TestOtherQueriesRejectSearchFields(t *testing.T) {
	s := newSvc(t)
	contact := model.ContactRef{Tenant: tenantA, ID: inboxOne}
	queries := map[string]func(model.Filter) error{
		"channels": func(f model.Filter) error {
			_, err := s.C.Configuration().Channels(s.Ctx, listReq(f, 1, ""))
			return err
		},
		"inboxes": func(f model.Filter) error {
			_, err := s.C.Configuration().Inboxes(s.Ctx, listReq(f, 1, ""))
			return err
		},
		"conversations": func(f model.Filter) error {
			_, err := s.C.Conversations().List(s.Ctx, listReq(f, 1, ""))
			return err
		},
		"work items": func(f model.Filter) error {
			_, err := s.C.WorkItems().List(s.Ctx, listReq(f, 1, ""))
			return err
		},
		"contacts": func(f model.Filter) error {
			_, err := s.C.Contacts().List(s.Ctx, listReq(f, 1, ""))
			return err
		},
		"identities": func(f model.Filter) error {
			_, err := s.C.Contacts().Identities(s.Ctx, listReq(f, 1, ""))
			return err
		},
		"history": func(f model.Filter) error {
			f.Contact = &contact
			_, err := s.C.Contacts().History(s.Ctx, listReq(f, 1, ""))
			return err
		},
	}
	for q, run := range queries {
		for field, f := range searchFilters() {
			t.Run(q+" "+field, func(t *testing.T) {
				requireCode(t, run(f), model.Validation, "filter."+field+": not supported by this query")
			})
		}
	}
}

// The query cursor binds the canonical filter. Search fields are appended
// after every existing field, each only when set, so a filter without
// them encodes byte for byte as before and its cursors stay valid.
func TestCanonicalFilterSearchFields(t *testing.T) {
	in := model.InboxRef{Tenant: tenantA, ID: "inbox-1"}
	conv := model.ConversationRef{Inbox: in, ID: "conv-1"}
	who := model.IdentityRef{Inbox: in, ID: "who"}
	base := model.Filter{Inboxes: []model.InboxRef{in}, Conversation: &conv}
	// Encoding of base before search fields existed.
	const baseHex = "013107696e626f782d3101310874656e616e742d610c636f6e766572736174696f6e07696e626f782d3106636f6e762d31013001300130"
	enc := func(f model.Filter) string { return hex.EncodeToString(services.CanonicalFilter(f)) }
	if got := enc(base); got != baseHex {
		t.Fatalf("base filter encoding changed: %s", got)
	}
	for field, f := range searchFilters() {
		f.Inboxes, f.Conversation = base.Inboxes, base.Conversation
		if got := enc(f); !strings.HasPrefix(got, baseHex) || got == baseHex {
			t.Fatalf("%s not appended last: %s", field, got)
		}
		f.Identity = &who
		withID := base
		withID.Identity = &who
		if got, prefix := enc(f), enc(withID); !strings.HasPrefix(got, prefix) || got == prefix {
			t.Fatalf("%s not appended after identity: %s", field, got)
		}
	}

	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Nanosecond)
	t0Paris := t0.In(time.FixedZone("CEST", 2*3600))
	yes, no := true, false
	same := map[string][2]model.Filter{
		"text token set": {{Text: "Invoice paid"}, {Text: "paid, INVOICE invoice"}},
		"instant":        {{Since: &t0}, {Since: &t0Paris}},
		"media order": {
			{Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/png", "text/*"}}},
			{Attachments: &model.AttachmentFilter{MediaTypes: []string{"text/*", "image/png"}}},
		},
	}
	for name, p := range same {
		if enc(p[0]) != enc(p[1]) {
			t.Fatalf("%s: equivalent filters encode differently", name)
		}
	}
	differ := map[string][2]model.Filter{
		"text":                {{Text: "invoice"}, {Text: "invoice paid"}},
		"since nanos":         {{Since: &t0}, {Since: &t1}},
		"since vs before":     {{Since: &t0}, {Before: &t0}},
		"before nanos":        {{Before: &t0}, {Before: &t1}},
		"since before split":  {{Since: &t0, Before: &t1}, {Before: &t1}},
		"has true vs false":   {{Attachments: &model.AttachmentFilter{Has: &yes}}, {Attachments: &model.AttachmentFilter{Has: &no}}},
		"has vs media":        {{Attachments: &model.AttachmentFilter{Has: &yes, MediaTypes: []string{"image/png"}}}, {Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/png"}}}},
		"media types":         {{Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/png"}}}, {Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/*"}}}},
		"text vs attachments": {{Text: "x"}, {Attachments: &model.AttachmentFilter{Has: &yes}}},
		"none vs text":        {{}, {Text: "x"}},
	}
	for name, p := range differ {
		if enc(p[0]) == enc(p[1]) {
			t.Fatalf("%s: different filters share an encoding", name)
		}
	}

	all := model.Filter{Text: "Invoice", Since: &t0, Before: &t1, Attachments: &model.AttachmentFilter{Has: &yes, MediaTypes: []string{"image/png"}}}
	const allHex = "01300130013001300130" + // empty base
		"0474657874" + "0131" + "07494e564f494345" + // text, 1 token, INVOICE
		"0573696e6365" + "0a31373930383132383030" + "0130" + // since, 1790812800 s, 0 ns
		"066265666f7265" + "0a31373930383132383030" + "0131" + // before, 1790812800 s, 1 ns
		"0b6174746163686d656e7473" + "0474727565" + "0131" + "09696d6167652f706e67" // attachments, has true, 1 type, image/png
	if got := enc(all); got != allHex {
		t.Fatalf("search field encoding changed: %s", got)
	}
}
