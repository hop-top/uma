package runtime_test

import (
	"reflect"
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

func withRecipients(t *testing.T) *rig {
	t.Helper()
	return newRig(t, enginetest.WithReplyRecipients)
}

// A reply naming recipients sends them to the adapter, normalized, next to
// the route and the thread; a reply naming none sends none.
func TestReplyCarriesRecipients(t *testing.T) {
	r := withRecipients(t)
	putInbound(t, r, "00000009-0000-7000-8000-0000000000f1", "pm-parent", model.Audience{Visibility: model.Public})
	req := r.ReplyReq("op-1", "answer")
	req.Recipients = &model.ReplyRecipients{ReplyAll: true, Cc: []string{"A@Example.COM", "a@example.com"}, Bcc: []string{"b@example.com"}}
	if _, err := r.C.Messages().Reply(r.Ctx, req); err != nil {
		t.Fatal(err)
	}
	r.Reply("op-2", "plain")
	r.Step("w")
	sends := r.F.ProviderSends()
	if len(sends) != 2 {
		t.Fatalf("sends = %d, want 2", len(sends))
	}
	want := &model.ReplyRecipients{ReplyAll: true, Cc: []string{"A@example.com"}, Bcc: []string{"b@example.com"}}
	if !reflect.DeepEqual(sends[0].Recipients, want) {
		t.Errorf("recipients = %+v, want %+v", sends[0].Recipients, want)
	}
	if sends[1].Recipients != nil {
		t.Errorf("a reply naming none sent %+v", sends[1].Recipients)
	}
}

// A reply naming recipients on a channel that does not declare the
// capability is refused before anything is stored.
func TestReplyRecipientsNeedTheCapability(t *testing.T) {
	r := newRig(t)
	req := r.ReplyReq("op-1", "answer")
	req.Recipients = &model.ReplyRecipients{Cc: []string{"a@example.com"}}
	_, err := r.C.Messages().Reply(r.Ctx, req)
	requireCode(t, err, model.Unsupported, "reply_recipients")
	// An empty recipients value asks for nothing: no capability is needed.
	req = r.ReplyReq("op-2", "answer")
	req.Recipients = &model.ReplyRecipients{}
	if _, err := r.C.Messages().Reply(r.Ctx, req); err != nil {
		t.Fatalf("empty recipients: %v", err)
	}
}

func TestReplyRecipientsAreValidated(t *testing.T) {
	r := withRecipients(t)
	req := r.ReplyReq("op-1", "answer")
	req.Recipients = &model.ReplyRecipients{Cc: []string{"Not <an@address>"}}
	_, err := r.C.Messages().Reply(r.Ctx, req)
	requireCode(t, err, model.Validation, "recipients.cc")
}

// Recipients are part of the request's fingerprint: the same request
// replays, another set conflicts.
func TestReplyRecipientsAreFingerprinted(t *testing.T) {
	r := withRecipients(t)
	req := r.ReplyReq("op-1", "answer")
	req.Recipients = &model.ReplyRecipients{Cc: []string{"a@example.com"}}
	first, err := r.C.Messages().Reply(r.Ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.C.Messages().Reply(r.Ctx, req)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	req.Recipients = &model.ReplyRecipients{Cc: []string{"b@example.com"}}
	_, err = r.C.Messages().Reply(r.Ctx, req)
	requireCode(t, err, model.Conflict, "")
	req.Recipients = nil
	_, err = r.C.Messages().Reply(r.Ctx, req)
	requireCode(t, err, model.Conflict, "")
}
