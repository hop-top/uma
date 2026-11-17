package tombstone_test

import (
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/internal/tombstone"
)

// A redaction names exactly one target: a message, an interaction or an
// attachment.
func TestCheckTargets(t *testing.T) {
	conv := model.ConversationRef{
		Inbox: model.InboxRef{Tenant: "tenant-a", ID: "00000000-0000-7000-8000-000000000001"},
		ID:    "00000000-0000-7000-8000-000000000002",
	}
	msg := model.MessageRef{Conversation: conv, ID: "00000000-0000-7000-8000-000000000003"}
	in := model.InteractionRef{Conversation: conv, ID: "00000000-0000-7000-8000-000000000004"}
	att := model.AttachmentRef{Inbox: conv.Inbox, ID: "00000000-0000-7000-8000-000000000005"}
	for name, tc := range map[string]struct {
		req store.Redaction
		ok  bool
	}{
		"message":     {store.Redaction{Message: &msg}, true},
		"interaction": {store.Redaction{Interaction: &in}, true},
		"attachment":  {store.Redaction{Attachment: &att}, true},
		"none":        {store.Redaction{}, false},
		"two":         {store.Redaction{Message: &msg, Interaction: &in}, false},
		"three":       {store.Redaction{Message: &msg, Interaction: &in, Attachment: &att}, false},
	} {
		t.Run(name, func(t *testing.T) {
			tc.req.Expected, tc.req.Revision = "r1", "r2"
			err := tombstone.Check(tc.req)
			if tc.ok != (err == nil) {
				t.Fatalf("Check = %v, want ok %v", err, tc.ok)
			}
			if err != nil && model.CodeOf(err) != model.Validation {
				t.Fatalf("code = %s, want validation", model.CodeOf(err))
			}
		})
	}
}

// An interaction tombstone clears what the provider supplied and the token,
// and keeps the rest, the response state and the token expiry included.
func TestInteractionTombstone(t *testing.T) {
	op := model.OperationRef{Tenant: "tenant-a", ID: "00000000-0000-7000-8000-000000000009"}
	expires := time.Unix(1_700_000_900, 7).UTC()
	rec := store.InteractionRecord{
		Interaction: model.Interaction{
			Kind: model.InteractionSubmission, Action: "approve", Command: "triage", Arguments: "secret",
			Fields:   model.Extension{Name: "org.example.form", Version: 2, Data: []byte(`{"f":1}`)},
			Response: model.InteractionResponse{Initial: &op, FollowUps: 2},
			Revision: "i1",
		},
		ResponseToken: "tok", TokenExpiresAt: expires,
	}
	got := tombstone.Interaction(rec, "i2")
	want := rec.Clone()
	want.Interaction.Fields.Data, want.Interaction.Arguments = nil, ""
	want.Interaction.Redacted, want.Interaction.Revision, want.ResponseToken = true, "i2", ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tombstone = %+v, want %+v", got, want)
	}
	if rec.ResponseToken != "tok" || rec.Interaction.Arguments != "secret" || rec.Interaction.Redacted {
		t.Fatal("tombstone changed its input")
	}
}
