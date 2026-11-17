package operations_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
)

func recipientsArgs(r *enginetest.Rig, text string, rec *model.ReplyRecipients) model.Extension {
	data, _ := json.Marshal(operations.ReplyIntent{
		Conversation: r.Conv.Ref, Content: textContent(text), Audience: operations.ReplyAudience(), Recipients: rec,
	})
	return model.Extension{Name: operations.IntentReply, Version: 1, Data: data}
}

// A held reply keeps the recipients it was proposed with: an approval sends
// them, and an edit may change the text but not who receives it.
func TestHeldReplyKeepsItsRecipients(t *testing.T) {
	r := newRig(t, enginetest.WithReplyRecipients)
	rec := &model.ReplyRecipients{ReplyAll: true, Cc: []string{"a@example.com"}}
	s := proposal(r, "p-1", "fp-p-1")
	s.Intent = recipientsArgs(r, "hello", rec)
	held := propose(t, r, pluginClient(t, r, plugin), s).Operation

	_, err := r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: recipientsArgs(r, "hello", &model.ReplyRecipients{Cc: []string{"b@example.com"}})})
	requireCode(t, err, model.Validation, "recipients cannot change")
	_, err = r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: recipientsArgs(r, "hello", nil)})
	requireCode(t, err, model.Validation, "recipients cannot change")
	requireHeld(t, r, held.Ref)

	if _, err := r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: recipientsArgs(r, "hello, edited", rec)}); err != nil {
		t.Fatalf("Edit text only: %v", err)
	}
	r.Step("w")
	sends := r.F.ProviderSends()
	if len(sends) != 1 || !reflect.DeepEqual(sends[0].Recipients, rec) {
		t.Fatalf("sends = %+v, want one carrying %+v", sends, rec)
	}
}
