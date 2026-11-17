package operations_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
)

// The version 1 encoding is the documented layout: domain, version, field
// count, then length-prefixed names and values in name order.
func TestFingerprintV1Layout(t *testing.T) {
	f := operations.NewFingerprint()
	f.Set("b", operations.Field().Str("2"))
	f.Set("a", operations.Field().Str("1"))
	var want []byte
	lp := func(b []byte) {
		want = binary.AppendUvarint(want, uint64(len(b)))
		want = append(want, b...)
	}
	lp([]byte("uma.operation.fingerprint"))
	want = binary.BigEndian.AppendUint32(want, 1)
	want = binary.BigEndian.AppendUint32(want, 2)
	for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}} {
		lp([]byte(kv[0]))
		var v []byte
		v = binary.AppendUvarint(v, uint64(len(kv[1])))
		v = append(v, kv[1]...)
		lp(v)
	}
	sum := sha256.Sum256(want)
	if got := f.Sum(); hex.EncodeToString(got) != hex.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint = %x, want %x", got, sum)
	}
}

// A stored fingerprint must never change meaning: this digest pins version
// 1 of the reply field set and encoding.
func TestReplyFingerprintV1IsStable(t *testing.T) {
	r := newRig(t)
	got := hex.EncodeToString(operations.ReplyFingerprint(r.ReplyReq("op-1", "hello"), operations.ReplyAudience()))
	const want = "076bad3e84ab74dd3f6f798b948226d09e9fc616545c473d2c5ff6c2e7d7410a"
	if got != want {
		t.Fatalf("reply fingerprint v1 = %s, want %s; an encoding change needs a new version", got, want)
	}
}

func TestReplyFingerprintCoversEffectsNotDiagnostics(t *testing.T) {
	r := newRig(t)
	base := r.ReplyReq("op-1", "hello")
	sum := func(q model.Reply) string {
		return hex.EncodeToString(operations.ReplyFingerprint(q, operations.ReplyAudience()))
	}
	same := base
	same.Correlation = "another-correlation"
	if sum(same) != sum(base) {
		t.Error("correlation changed the fingerprint")
	}
	for name, mutate := range map[string]func(*model.Reply){
		"text":              func(q *model.Reply) { q.Content.Parts[0].Text = "hello!" },
		"expected revision": func(q *model.Reply) { q.ExpectedRevision = "cv1" },
		"in reply to": func(q *model.Reply) {
			q.InReplyTo = &model.MessageRef{Conversation: q.Conversation, ID: inboxTwo}
		},
		"target": func(q *model.Reply) { q.Conversation.ID = inboxTwo },
	} {
		q := r.ReplyReq("op-1", "hello")
		mutate(&q)
		if sum(q) == sum(base) {
			t.Errorf("%s did not change the fingerprint", name)
		}
	}
	if a, b := hex.EncodeToString(operations.ReplyFingerprint(base, model.Audience{Visibility: model.Public})),
		hex.EncodeToString(operations.ReplyFingerprint(base, model.Audience{Visibility: model.Internal})); a == b {
		t.Error("audience did not change the fingerprint")
	}
}

func TestReplyQueuesDurableIntent(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "hello")
	if a.Replayed || a.Durable || a.Operation.State != model.Queued {
		t.Fatalf("acceptance = %+v, want new, non-durable, queued", a)
	}
	if got := r.Op(a.Operation.Ref); got.State != model.Queued || got.Revision != a.Operation.Revision {
		t.Fatalf("Operations.Get = %+v", got)
	}
	if n := len(r.Claim("w")); n != 1 {
		t.Fatalf("claimable work = %d, want 1", n)
	}
}

// Replay: the same key and payload return the stored operation, also with
// a new correlation, and enqueue no second execution.
func TestReplyReplaysSamePayload(t *testing.T) {
	r := newRig(t)
	first := r.Reply("op-1", "hello")
	req := r.ReplyReq("op-1", "hello")
	req.Correlation = "corr-redelivery"
	again, err := r.C.Messages().Reply(r.Ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("replay = %+v, want replay of %v", again, first.Operation.Ref)
	}
	if n := len(r.Claim("w")); n != 1 {
		t.Fatalf("claimable work = %d, want 1", n)
	}
}

// Conflict: the same key with a different payload is rejected.
func TestReplyConflictsOnChangedPayload(t *testing.T) {
	r := newRig(t)
	r.Reply("op-1", "hello")
	_, err := r.C.Messages().Reply(r.Ctx, r.ReplyReq("op-1", "goodbye"))
	requireCode(t, err, model.Conflict, "different payload")
}

// A replay reveals the stored result, after execution too, but only once
// current policy allows the caller.
func TestReplayIsAuthorizedAndReturnsStoredResult(t *testing.T) {
	r := newRig(t)
	first := r.Reply("op-1", "hello")
	r.Step("w")
	again, err := r.C.Messages().Reply(r.Ctx, r.ReplyReq("op-1", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Operation.State != model.ProviderAccepted || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("replay = %+v, want the stored provider_accepted result", again)
	}
	r.Policy.Set(true)
	_, err = r.C.Messages().Reply(r.Ctx, r.ReplyReq("op-1", "hello"))
	requireCode(t, err, model.Unauthorized, "")
}

// The key includes the caller: another actor's equal operation ID is a new
// operation and never reveals the first one.
func TestOperationKeyIsScopedToCaller(t *testing.T) {
	r := newRig(t)
	first := r.Reply("op-1", "hello")
	other, err := enginetest.For(r.E, model.Principal{Tenant: tenantA, Actor: "actor-2"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := other.Messages().Reply(r.Ctx, r.ReplyReq("op-1", "different text"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Replayed || a.Operation.Ref == first.Operation.Ref {
		t.Fatalf("other caller acceptance = %+v, want a new operation", a)
	}
}

// A completed command does not fail on replay because its expected revision
// has since advanced; a new command with that revision conflicts.
func TestReplayPrecedesRevisionCheck(t *testing.T) {
	r := newRig(t)
	req := r.ReplyReq("op-1", "hello")
	req.ExpectedRevision = r.Conv.Revision
	if _, err := r.C.Messages().Reply(r.Ctx, req); err != nil {
		t.Fatal(err)
	}
	r.SetRoute(r.Conv.ReplyRoute) // bumps the conversation revision
	again, err := r.C.Messages().Reply(r.Ctx, req)
	if err != nil || !again.Replayed {
		t.Fatalf("replay = %+v, %v; want replay", again, err)
	}
	req.Operation = "op-2"
	_, err = r.C.Messages().Reply(r.Ctx, req)
	requireCode(t, err, model.Conflict, "revision")
}

func TestOperationsGetIsScopedAndAuthorized(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "hello")
	foreign := a.Operation.Ref
	foreign.Tenant = tenantB
	_, err := r.C.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "g"}, Ref: foreign})
	requireCode(t, err, model.NotFound, "")
	r.Policy.Set(true)
	_, err = r.C.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "g"}, Ref: a.Operation.Ref})
	requireCode(t, err, model.Unauthorized, "")
}

func TestReplyToUnknownConversationIsNotFound(t *testing.T) {
	r := newRig(t)
	req := r.ReplyReq("op-1", "hello")
	req.Conversation.ID = inboxTwo
	_, err := r.C.Messages().Reply(r.Ctx, req)
	requireCode(t, err, model.NotFound, "")
}

// Recipients extend version 1 without changing it: a reply naming none keeps
// its digest, one naming some has another, an empty set is none, and the
// normalized set is what counts.
func TestReplyFingerprintCoversRecipients(t *testing.T) {
	r := newRig(t)
	sum := func(q model.Reply) string {
		return hex.EncodeToString(operations.ReplyFingerprint(q, operations.ReplyAudience()))
	}
	base := r.ReplyReq("op-1", "hello")
	with := func(rec *model.ReplyRecipients) model.Reply { q := base; q.Recipients = rec; return q }
	if sum(base) != sum(with(&model.ReplyRecipients{})) {
		t.Error("an empty recipients value changed the fingerprint")
	}
	cc := with(&model.ReplyRecipients{Cc: []string{"a@example.com"}})
	for name, other := range map[string]model.Reply{
		"cc":        with(&model.ReplyRecipients{Cc: []string{"b@example.com"}}),
		"as bcc":    with(&model.ReplyRecipients{Bcc: []string{"a@example.com"}}),
		"reply all": with(&model.ReplyRecipients{Cc: []string{"a@example.com"}, ReplyAll: true}),
		"none":      base,
	} {
		if sum(cc) == sum(other) {
			t.Errorf("recipients %s did not change the fingerprint", name)
		}
	}
	if sum(cc) != sum(with(&model.ReplyRecipients{Cc: []string{"a@EXAMPLE.com", "a@example.com"}})) {
		t.Error("an equivalent recipients set changed the fingerprint")
	}
}
