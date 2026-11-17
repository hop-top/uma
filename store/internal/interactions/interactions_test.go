package interactions_test

import (
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/internal/interactions"
)

const (
	tenant   model.TenantID = "tenant-a"
	inboxID  model.ID       = "00000000-0000-7000-8000-000000000001"
	convID   model.ID       = "00000000-0000-7000-8000-000000000002"
	interact model.ID       = "00000000-0000-7000-8000-000000000003"
	opID     model.ID       = "00000000-0000-7000-8000-000000000004"
)

func record() store.InteractionRecord {
	conv := model.ConversationRef{Inbox: model.InboxRef{Tenant: tenant, ID: inboxID}, ID: convID}
	return store.InteractionRecord{
		Interaction: model.Interaction{
			Ref: model.InteractionRef{Conversation: conv, ID: interact}, Kind: model.InteractionAction, Revision: "i1",
		},
		Ingress: opID, ResponseToken: "s3cr3t-value", TokenExpiresAt: time.Unix(1_700_000_000, 0),
	}
}

func TestCheckPut(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*store.InteractionRecord)
		code model.ErrorCode
	}{
		"valid":       {func(*store.InteractionRecord) {}, ""},
		"no token":    {func(r *store.InteractionRecord) { r.ResponseToken, r.TokenExpiresAt = "", time.Time{} }, ""},
		"foreign":     {func(r *store.InteractionRecord) { r.Interaction.Ref.Conversation.Inbox.Tenant = "other" }, model.NotFound},
		"no id":       {func(r *store.InteractionRecord) { r.Interaction.Ref.ID = "" }, model.Validation},
		"bad kind":    {func(r *store.InteractionRecord) { r.Interaction.Kind = "click" }, model.Validation},
		"no revision": {func(r *store.InteractionRecord) { r.Interaction.Revision = "" }, model.Validation},
		"no ingress":  {func(r *store.InteractionRecord) { r.Ingress = "" }, model.Validation},
		"no expiry":   {func(r *store.InteractionRecord) { r.TokenExpiresAt = time.Time{} }, model.Validation},
		"foreign actor": {func(r *store.InteractionRecord) {
			r.Interaction.Actor.Identity = model.IdentityRef{Inbox: model.InboxRef{Tenant: "other", ID: inboxID}, ID: opID}
		}, model.NotFound},
	} {
		rec := record()
		c.edit(&rec)
		err := interactions.CheckPut(tenant, rec)
		if c.code == "" && err != nil || c.code != "" && !model.IsCode(err, c.code) {
			t.Errorf("%s: CheckPut = %v, want %q", name, err, c.code)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: error %q echoes the token", name, err)
		}
	}
}

func TestClaim(t *testing.T) {
	op1 := model.OperationRef{Tenant: tenant, ID: opID}
	cur := record().Interaction
	first, err := interactions.Claim(cur, store.ResponseClaim{Operation: op1, Revision: "i2"})
	if err != nil || first.Response.Initial == nil || *first.Response.Initial != op1 || first.Revision != "i2" {
		t.Fatalf("initial claim = %+v, %v", first, err)
	}
	if cur.Response.Initial != nil || cur.Revision != "i1" {
		t.Fatal("Claim changed its input")
	}
	if _, err := interactions.Claim(first, store.ResponseClaim{Operation: op1, Revision: "i3"}); !model.IsCode(err, model.Conflict) {
		t.Errorf("second initial claim = %v, want conflict", err)
	}
	follow, err := interactions.Claim(first, store.ResponseClaim{Operation: op1, FollowUp: true, Revision: "i3"})
	if err != nil || follow.Response.FollowUps != 1 || follow.Response.Initial == nil {
		t.Errorf("follow-up = %+v, %v", follow.Response, err)
	}
	if _, err := interactions.Claim(cur, store.ResponseClaim{Operation: op1, Revision: "i1"}); !model.IsCode(err, model.Validation) {
		t.Errorf("same revision = %v, want validation", err)
	}
	cur.Redacted = true
	if _, err := interactions.Claim(cur, store.ResponseClaim{Operation: op1, FollowUp: true, Revision: "i2"}); !model.IsCode(err, model.Conflict) {
		t.Errorf("redacted = %v, want conflict", err)
	}
}

func TestCheckLink(t *testing.T) {
	ref := record().Interaction.Ref
	msg := model.MessageRef{Conversation: ref.Conversation, ID: opID}
	for name, c := range map[string]struct {
		edit func(*store.InteractionLink)
		code model.ErrorCode
	}{
		"valid":         {func(*store.InteractionLink) {}, ""},
		"other conv":    {func(l *store.InteractionLink) { l.Message.Conversation.ID = interact }, ""},
		"foreign":       {func(l *store.InteractionLink) { l.Interaction.Conversation.Inbox.Tenant = "other" }, model.NotFound},
		"no message id": {func(l *store.InteractionLink) { l.Message.ID = "" }, model.Validation},
		"other inbox":   {func(l *store.InteractionLink) { l.Message.Conversation.Inbox.ID = interact }, model.Validation},
		"no revision":   {func(l *store.InteractionLink) { l.Revision = "" }, model.Validation},
	} {
		l := store.InteractionLink{Interaction: ref, Message: msg, Revision: "i2"}
		c.edit(&l)
		err := interactions.CheckLink(tenant, l)
		if c.code == "" && err != nil || c.code != "" && !model.IsCode(err, c.code) {
			t.Errorf("%s: CheckLink = %v, want %q", name, err, c.code)
		}
	}
}

func TestLink(t *testing.T) {
	cur := record().Interaction
	src := model.ExternalRef{Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: "pm-1"}
	cur.MessageExternal = &src
	msg := model.MessageRef{Conversation: cur.Ref.Conversation, ID: opID}
	l := store.InteractionLink{Interaction: cur.Ref, Message: msg, Revision: "i2"}
	if !interactions.Unlinked(cur, src) {
		t.Fatal("an interaction naming no message is not unlinked")
	}
	next, changed, err := interactions.Link(cur, l)
	if err != nil || !changed || next.Message == nil || *next.Message != msg || next.Revision != "i2" {
		t.Fatalf("Link = %+v, %v, %v", next, changed, err)
	}
	if cur.Message != nil || cur.Revision != "i1" {
		t.Fatal("Link changed its input")
	}
	if interactions.Unlinked(next, src) {
		t.Error("a linked interaction is still unlinked")
	}
	again, changed, err := interactions.Link(next, store.InteractionLink{Interaction: cur.Ref, Message: msg, Revision: "i3"})
	if err != nil || changed || again.Revision != "i2" {
		t.Errorf("relink = %+v, %v, %v; want it unchanged", again, changed, err)
	}
	other := msg
	other.ID = interact
	if _, _, err := interactions.Link(next, store.InteractionLink{Interaction: cur.Ref, Message: other, Revision: "i3"}); !model.IsCode(err, model.Conflict) {
		t.Errorf("link to another message = %v, want conflict", err)
	}
	if _, _, err := interactions.Link(cur, store.InteractionLink{Interaction: cur.Ref, Message: msg, Revision: "i1"}); !model.IsCode(err, model.Validation) {
		t.Errorf("same revision = %v, want validation", err)
	}
	bare := cur
	bare.MessageExternal = nil
	if _, _, err := interactions.Link(bare, l); !model.IsCode(err, model.Conflict) {
		t.Errorf("no provider message = %v, want conflict", err)
	}
	if interactions.Unlinked(bare, src) {
		t.Error("an interaction naming no provider message is unlinked")
	}
	redacted := cur
	redacted.Redacted = true
	if _, changed, err := interactions.Link(redacted, l); err != nil || !changed {
		t.Errorf("redacted = %v, %v; want it linked", changed, err)
	}
}
