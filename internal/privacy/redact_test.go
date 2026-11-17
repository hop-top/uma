package privacy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/retention"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

const secret = "secret body"

func withBlobs(b *portstest.MemoryBlobs) enginetest.RigOption {
	return func(c *engine.Config, _ *enginetest.OutFactory) { c.Blobs = b }
}

func cmd(op model.OperationID, rev model.Revision) model.Command {
	return model.Command{Call: model.Call{Correlation: "corr-" + model.CorrelationID(op)}, Operation: op, ExpectedRevision: rev}
}

func redactMessage(m model.Message, op model.OperationID) model.Redact {
	ref := m.Ref
	return model.Redact{Command: cmd(op, m.Revision), Target: ref.Ref(), Message: &ref, Reason: "erasure"}
}

// note records an internal note holding secret text and an uploaded
// attachment, and returns it with the attachment.
func note(t *testing.T, r *enginetest.Rig) (model.Message, model.AttachmentRef) {
	t.Helper()
	return noteAs(t, r, "")
}

// noteAs is note under operation IDs ending in suffix.
func noteAs(t *testing.T, r *enginetest.Rig, suffix model.OperationID) (model.Message, model.AttachmentRef) {
	t.Helper()
	inbox := r.Conv.Ref.Inbox
	prep, err := r.C.Attachments().Prepare(r.Ctx, model.PrepareAttachment{
		Command: cmd("op-prepare"+suffix, ""), Inbox: inbox,
		Metadata: model.AttachmentMetadata{Name: "scan.pdf", MediaType: "application/pdf", Audience: model.Audience{Visibility: model.Internal}},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if k := prep.Operation.Key; k == nil || k.Kind != model.ActionAttachmentPrepare || k.Target != inbox.Ref() {
		t.Fatalf("Prepare acceptance key = %+v, want attachment.prepare on the inbox", k)
	}
	att, err := prep.Operation.Result.AsAttachment()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.C.Attachments().Upload(r.Ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "c"}, Ref: att},
		cmd("op-upload"+suffix, ""), strings.NewReader(secret)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	a, err := r.C.Messages().AddInternalNote(r.Ctx, model.InternalNote{
		Command: cmd("op-note"+suffix, ""), Conversation: r.Conv.Ref, PolicyRef: "desk",
		Content: model.Content{Parts: []model.Part{
			{Kind: model.PartText, Text: secret},
			{Kind: model.PartAttachment, Attachment: &att},
		}},
	})
	if err != nil {
		t.Fatalf("AddInternalNote: %v", err)
	}
	return get(t, r, *a.Operation.ResultMessage), att
}

func get(t *testing.T, r *enginetest.Rig, ref model.MessageRef) model.Message {
	t.Helper()
	m, err := r.C.Messages().Get(r.Ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return m
}

// requireNoSecret fails when any committed event carries the secret text.
func requireNoSecret(t *testing.T, evs []model.Event) {
	t.Helper()
	for _, ev := range evs {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("event %s (%s) carries message content: %s", ev.ID, ev.Kind, b)
		}
	}
}

// Redacting a note tombstones it, its attachment record and blob, and
// reports the change in a message.updated event that, like every event,
// carries no content. The tombstone keeps its references and audit fields,
// and a retry replays the stored operation.
func TestRedactTombstonesANoteAndItsAttachment(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	r := enginetest.NewRig(t, withBlobs(blobs))
	m, att := note(t, r)
	requireNoSecret(t, r.Events())
	before := len(r.Events())

	req := redactMessage(m, "op-redact")
	a, err := r.C.Privacy().Redact(r.Ctx, req)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if a.Replayed || a.Operation.State != model.Committed || a.Operation.Result == nil || *a.Operation.Result != m.Ref.Ref() ||
		a.Operation.ResultMessage == nil || *a.Operation.ResultMessage != m.Ref {
		t.Fatalf("acceptance = %+v, want a committed operation on the message", a)
	}

	got := get(t, r, m.Ref)
	if !got.Redacted || len(got.Content.Parts) != 0 || got.Revision == m.Revision || got.Revision == "" {
		t.Fatalf("message after redaction = %+v, want an empty tombstone under a new revision", got)
	}
	if got.Ref != m.Ref || got.Audience != m.Audience || got.Origin == nil || *got.Origin != *m.Origin ||
		!got.RecordedAt.Equal(m.RecordedAt) || !got.OccurredAt.Equal(m.OccurredAt) || got.Author != m.Author {
		t.Errorf("tombstone lost references or audit fields: %+v, was %+v", got, m)
	}

	if keys := blobs.Keys(); len(keys) != 0 {
		t.Errorf("blobs after redaction = %v, want the attachment's bytes deleted", keys)
	}
	_, _, err = r.C.Attachments().Open(r.Ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "c"}, Ref: att})
	enginetest.RequireCode(t, err, model.Conflict, "")
	err = r.E.Store().View(r.Ctx, enginetest.TenantA, func(rd store.Reader) error {
		rec, err := rd.Attachment(r.Ctx, att)
		if err == nil && (rec.State != store.AttachmentRedacted || rec.Blob != nil || rec.Attachment.Metadata.Name != "") {
			t.Errorf("attachment record = %+v, want a tombstone", rec)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	evs := r.Events()
	requireNoSecret(t, evs)
	if len(evs) != before+1 || evs[len(evs)-1].Kind != model.EventMessageUpdated {
		t.Fatalf("events = %v, want one message.updated more", enginetest.EventKinds(evs))
	}
	ev := evs[len(evs)-1]
	var p model.MessageUpdatedPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if ev.Resource != m.Ref.Ref() || ev.Revision != got.Revision || ev.Audience != m.Audience || ev.Correlation != req.Correlation ||
		p.Message != m.Ref || p.Change != model.MessageChangeRedacted || p.Trigger != model.TriggerCommand ||
		p.PreviousRevision != m.Revision || p.Operation == nil || *p.Operation != a.Operation.Ref {
		t.Errorf("event = %+v payload %+v", ev, p)
	}

	again, err := r.C.Privacy().Redact(r.Ctx, req)
	if err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref {
		t.Fatalf("retry = %+v, %v; want the stored operation replayed", again, err)
	}
	other := req
	other.Reason = "other"
	_, err = r.C.Privacy().Redact(r.Ctx, other)
	enginetest.RequireCode(t, err, model.Conflict, "")
}

// Policy decides before anything changes: a denied redaction leaves the
// message as it was, and the check names the privacy action on the message
// under its own audience and the expected revision.
func TestRedactIsAuthorizedFirst(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	r := enginetest.NewRig(t, withBlobs(blobs))
	m, _ := note(t, r)
	r.Policy.Set(true)
	_, err := r.C.Privacy().Redact(r.Ctx, redactMessage(m, "op-redact"))
	enginetest.RequireCode(t, err, model.Unauthorized, "")
	r.Policy.Set(false)
	if got := get(t, r, m.Ref); got.Redacted || len(got.Content.Parts) != 2 || got.Revision != m.Revision {
		t.Fatalf("message after a denied redaction = %+v, want it unchanged", got)
	}
	if len(blobs.Keys()) != 1 {
		t.Fatal("a denied redaction deleted the attachment's bytes")
	}
	if !slices.Contains(r.Policy.Actions(), model.ActionPrivacyRedact) {
		t.Fatalf("policy actions = %v, want %s", r.Policy.Actions(), model.ActionPrivacyRedact)
	}
	var seen []policy.Authorization
	auth := &recordingPolicy{}
	r2 := enginetest.NewRig(t, withBlobs(&portstest.MemoryBlobs{}), func(c *engine.Config, _ *enginetest.OutFactory) { c.Authorizer = auth })
	m2, _ := note(t, r2)
	if _, err := r2.C.Privacy().Redact(r2.Ctx, redactMessage(m2, "op-redact")); err != nil {
		t.Fatal(err)
	}
	for _, a := range auth.checks {
		if a.Action == model.ActionPrivacyRedact {
			seen = append(seen, a)
		}
	}
	if len(seen) != 1 || seen[0].Resource != m2.Ref.Ref() || seen[0].Audience != m2.Audience || seen[0].Revision != m2.Revision ||
		seen[0].Principal != enginetest.PrincipalA() {
		t.Fatalf("redact authorizations = %+v", seen)
	}
}

type recordingPolicy struct{ checks []policy.Authorization }

func (p *recordingPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.checks = append(p.checks, a)
	return policy.Decision{Allow: true}, nil
}

// Requests outside the principal's tenant, under a stale revision, on a
// kind this version does not redact, naming no record or asking for
// provider propagation fail before anything changes.
func TestRedactRefusals(t *testing.T) {
	r := enginetest.NewRig(t, withBlobs(&portstest.MemoryBlobs{}))
	m, _ := note(t, r)
	foreign, err := enginetest.For(r.E, model.Principal{Tenant: enginetest.TenantB, Actor: enginetest.Actor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = foreign.Privacy().Redact(r.Ctx, redactMessage(m, "op-foreign"))
	enginetest.RequireCode(t, err, model.NotFound, "")

	stale := redactMessage(m, "op-stale")
	stale.ExpectedRevision = "stale"
	_, err = r.C.Privacy().Redact(r.Ctx, stale)
	enginetest.RequireCode(t, err, model.Conflict, "")

	moved := redactMessage(m, "op-moved")
	other := m.Ref
	other.Conversation.ID = enginetest.InboxTwo
	moved.Message = &other
	_, err = r.C.Privacy().Redact(r.Ctx, moved)
	enginetest.RequireCode(t, err, model.NotFound, "")

	propagate := redactMessage(m, "op-propagate")
	propagate.PropagateToProvider = true
	_, err = r.C.Privacy().Redact(r.Ctx, propagate)
	enginetest.RequireCode(t, err, model.Unsupported, "")

	contact := model.Redact{Command: cmd("op-contact", "rev"), Target: model.ContactRef{Tenant: enginetest.TenantA, ID: m.Ref.ID}.Ref()}
	_, err = r.C.Privacy().Redact(r.Ctx, contact)
	enginetest.RequireCode(t, err, model.Validation, "cascade")
	cascade := redactMessage(m, "op-cascade")
	cascade.Cascade = true
	_, err = r.C.Privacy().Redact(r.Ctx, cascade)
	enginetest.RequireCode(t, err, model.Validation, "cascade")

	_, err = r.C.Privacy().Redact(r.Ctx, model.Redact{Command: cmd("op-bad", "rev"), Target: m.Ref.Ref()})
	enginetest.RequireCode(t, err, model.Validation, "message")

	irf := model.InteractionRef{Conversation: m.Ref.Conversation, ID: m.Ref.ID}
	_, err = r.C.Privacy().Redact(r.Ctx, model.Redact{Command: cmd("op-interaction", "rev"), Target: irf.Ref(), Interaction: &irf})
	enginetest.RequireCode(t, err, model.NotFound, "")

	if got := get(t, r, m.Ref); got.Redacted || got.Revision != m.Revision {
		t.Fatalf("message after refused redactions = %+v, want it unchanged", got)
	}
}

// A reply still queued for delivery cannot be redacted, since the worker
// would read its content from the operation. Once it is delivered the
// redaction clears that copy too.
func TestRedactWaitsForAReplyToSettle(t *testing.T) {
	r := enginetest.NewRig(t)
	a := r.Reply("op-reply", secret)
	m := get(t, r, *a.Operation.ResultMessage)
	_, err := r.C.Privacy().Redact(r.Ctx, redactMessage(m, "op-early"))
	enginetest.RequireCode(t, err, model.Conflict, "")
	r.Step("w")
	enginetest.RequireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	m = get(t, r, m.Ref)
	if _, err := r.C.Privacy().Redact(r.Ctx, redactMessage(m, "op-redact")); err != nil {
		t.Fatalf("Redact after delivery: %v", err)
	}
	err = r.E.Store().View(r.Ctx, enginetest.TenantA, func(rd store.Reader) error {
		op, err := rd.Operation(r.Ctx, a.Operation.Ref)
		if err == nil && (len(op.Command.Data) != 0 || op.Operation.State != model.ProviderAccepted) {
			t.Errorf("reply operation = %+v, want its intent data cleared and its state kept", op)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(r.F.ProviderSends()); n != 1 {
		t.Errorf("provider sends = %d, want the one delivery only", n)
	}
	requireNoSecret(t, r.Events())
}

// ambiguousStore reports the first committed unit as a serialization
// failure, as a commit whose outcome is lost would look.
type ambiguousStore struct {
	store.Store
	lost bool
}

func (s *ambiguousStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	err := s.Store.Transact(ctx, t, fn)
	if err == nil && !s.lost {
		s.lost = true
		return model.NewError(model.Transient, "commit outcome lost")
	}
	return err
}

// Redacting an attachment directly tombstones its record and deletes its
// bytes, also when the committing unit's outcome was lost and the retry
// replays the operation; the message referring to it is untouched and no
// event is emitted.
func TestRedactAttachmentDeletesItsBytesAfterALostCommit(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	r := enginetest.NewRig(t, withBlobs(blobs))
	m, att := note(t, r)
	before := len(r.Events())
	var rev model.Revision
	err := r.E.Store().View(r.Ctx, enginetest.TenantA, func(rd store.Reader) error {
		rec, err := rd.Attachment(r.Ctx, att)
		rev = rec.Attachment.Revision
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Store.Set(&ambiguousStore{Store: r.Store.Inner()})
	a, err := r.C.Privacy().Redact(r.Ctx, model.Redact{Command: cmd("op-redact", rev), Target: att.Ref()})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if !a.Replayed || a.Operation.ResultMessage != nil || *a.Operation.Result != att.Ref() {
		t.Fatalf("acceptance = %+v, want the committed attachment redaction replayed", a)
	}
	if keys := blobs.Keys(); len(keys) != 0 {
		t.Errorf("blobs = %v, want the attachment's bytes deleted", keys)
	}
	if got := get(t, r, m.Ref); got.Redacted || len(got.Content.Parts) != 2 {
		t.Errorf("message = %+v, want it untouched", got)
	}
	if n := len(r.Events()); n != before {
		t.Errorf("events = %d, want %d: attachments have no event", n, before)
	}
}

// failingDeletes fails every Delete while fail is set.
type failingDeletes struct {
	*portstest.MemoryBlobs
	fail bool
}

func (b *failingDeletes) Delete(ctx context.Context, k ports.BlobKey) error {
	if b.fail {
		return model.NewError(model.Transient, "blob store unavailable")
	}
	return b.MemoryBlobs.Delete(ctx, k)
}

// Bytes a redaction unbound but could not delete stay recorded as a
// staging intent, and the retention sweep deletes them; a delete that
// succeeds clears the intent at once.
func TestRedactLeavesUndeletedBytesToTheSweep(t *testing.T) {
	blobs := &failingDeletes{MemoryBlobs: &portstest.MemoryBlobs{}, fail: true}
	horizon := time.Hour
	r := enginetest.NewRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Blobs = blobs
		c.Retention = engine.RetentionPolicy{Events: horizon, Operations: horizon, Ingress: horizon, Attempts: horizon, Staging: horizon, Exports: horizon, Contacts: horizon}
	})
	m, want := note(t, r)
	key := blobs.Keys()[0]
	if _, err := r.C.Privacy().Redact(r.Ctx, redactMessage(m, "op-redact")); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	staging := func() error {
		return r.E.Store().View(r.Ctx, enginetest.TenantA, func(rd store.Reader) error {
			in, err := rd.Staging(r.Ctx, key)
			if err == nil && in.Attachment != want {
				t.Errorf("staging intent = %+v, want one for %+v", in, want)
			}
			return err
		})
	}
	if err := staging(); err != nil {
		t.Fatalf("staging intent after a failed delete: %v", err)
	}
	if len(blobs.Keys()) != 1 {
		t.Fatal("bytes gone although every delete failed")
	}
	blobs.fail = false
	r.Clock.Advance(2 * horizon)
	if _, err := retention.Purge(r.Ctx, r.E, enginetest.PrincipalA()); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if keys := blobs.Keys(); len(keys) != 0 {
		t.Errorf("blobs after the sweep = %v, want none", keys)
	}
	enginetest.RequireCode(t, staging(), model.NotFound, "")

	// A delete that succeeds clears its intent in the same call.
	m2, att2 := noteAs(t, r, "-2")
	want = att2
	key2 := blobs.Keys()[0]
	if _, err := r.C.Privacy().Redact(r.Ctx, redactMessage(m2, "op-redact-2")); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	key = key2
	enginetest.RequireCode(t, staging(), model.NotFound, "")
}
