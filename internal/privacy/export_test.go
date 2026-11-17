package privacy_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/retention"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

func exportReq(op model.OperationID, f model.Filter) model.ExportRequest {
	return model.ExportRequest{Command: cmd(op, ""), Filter: f, Format: model.ExportFormatJSONL}
}

// export runs req and opens the artifact, failing the test on error.
func export(t *testing.T, c enginetest.Client, ctx context.Context, req model.ExportRequest) (model.Acceptance, model.Export, []model.ExportLine) {
	t.Helper()
	a, err := c.Privacy().Export(ctx, req)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if a.Operation.State != model.Committed || a.Operation.Result == nil {
		t.Fatalf("acceptance = %+v, want a committed export", a)
	}
	if k := a.Operation.Key; k == nil || k.Kind != model.ActionPrivacyExport || k.ID != req.Operation {
		t.Fatalf("acceptance key = %+v, want privacy.export, %s", k, req.Operation)
	}
	ref, err := a.Operation.Result.AsExport()
	if err != nil {
		t.Fatalf("result %+v is not an export: %v", a.Operation.Result, err)
	}
	x, lines := open(t, c, ctx, ref)
	return a, x, lines
}

func open(t *testing.T, c enginetest.Client, ctx context.Context, ref model.ExportRef) (model.Export, []model.ExportLine) {
	t.Helper()
	x, rc, err := c.Privacy().Open(ctx, model.Lookup[model.ExportRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	sum := sha256.Sum256(data)
	if x.Ref != ref || x.Format != model.ExportFormatJSONL || x.Size != int64(len(data)) ||
		x.Digest != "sha256:"+hex.EncodeToString(sum[:]) || x.CreatedAt.IsZero() {
		t.Fatalf("export = %+v for %d bytes, want its ref, format, size and digest", x, len(data))
	}
	var lines []model.ExportLine
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var l model.ExportLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 || lines[0].Type != model.ExportLineExport {
		t.Fatalf("export lines = %v, want an export header first", lines)
	}
	return x, lines
}

func ofType[T any](t *testing.T, lines []model.ExportLine, typ string) []T {
	t.Helper()
	var out []T
	for _, l := range lines {
		if l.Type != typ {
			continue
		}
		var v T
		if err := json.Unmarshal(l.Data, &v); err != nil {
			t.Fatalf("%s line: %v", typ, err)
		}
		out = append(out, v)
	}
	return out
}

// A conversation export holds the conversation, every message the caller
// may read, notes included, and the metadata of their attachments, never
// a blob locator. Its bytes come only through Open, verified against the
// recorded size and digest, and the staging intent is bound on commit.
func TestExportConversation(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	r := enginetest.NewRig(t, withBlobs(blobs))
	n, att := note(t, r)
	reply := r.Reply("op-reply", "public answer")
	held := len(blobs.Keys())

	a, x, lines := export(t, r.C, r.Ctx, exportReq("op-export", model.Filter{Conversation: &r.Conv.Ref}))
	if a.Replayed || x.Filter.Conversation == nil || *x.Filter.Conversation != r.Conv.Ref {
		t.Fatalf("export = %+v, acceptance %+v", x, a)
	}
	convs := ofType[model.Conversation](t, lines, model.ExportLineConversation)
	if len(convs) != 1 || convs[0].Ref != r.Conv.Ref {
		t.Errorf("conversations = %+v, want the one", convs)
	}
	msgs := ofType[model.Message](t, lines, model.ExportLineMessage)
	if len(msgs) != 2 || msgs[0].Ref != n.Ref || msgs[1].Ref != *reply.Operation.ResultMessage {
		t.Errorf("messages = %+v, want the note then the reply", msgs)
	}
	atts := ofType[model.Attachment](t, lines, model.ExportLineAttachment)
	if len(atts) != 1 || atts[0].Ref != att || atts[0].Metadata.Name != "scan.pdf" {
		t.Errorf("attachments = %+v, want the note's", atts)
	}
	for _, l := range lines {
		if bytes.Contains(l.Data, []byte(`"blob"`)) || bytes.Contains(l.Data, []byte("locator")) {
			t.Errorf("line %s carries a blob locator: %s", l.Type, l.Data)
		}
	}
	if got := len(blobs.Keys()); got != held+1 {
		t.Errorf("blobs = %d, want the export's added to %d", got, held)
	}
	err := r.E.Store().View(r.Ctx, enginetest.TenantA, func(rd store.Reader) error {
		ref, _ := a.Operation.Result.AsExport()
		rec, err := rd.Export(r.Ctx, ref)
		if err != nil {
			return err
		}
		_, err = rd.Staging(r.Ctx, rec.Blob)
		enginetest.RequireCode(t, err, model.NotFound, "")
		if rec.Operation != a.Operation.Ref || rec.Creator != enginetest.Actor {
			t.Errorf("export record = %+v", rec)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// selectivePolicy denies the listed actions on resources whose audience is
// internal, and every listed action outright when deny is set.
type selectivePolicy struct {
	internal map[string]bool
	deny     map[string]bool
}

func (p *selectivePolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	if p.deny[a.Action] || (p.internal[a.Action] && a.Audience.Visibility == model.Internal) {
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true}, nil
}

// Notes and attachments the caller may not read are left out without
// trace; the public reply stays.
func TestExportSkipsWhatTheCallerMayNotRead(t *testing.T) {
	pol := &selectivePolicy{internal: map[string]bool{}, deny: map[string]bool{}}
	r := enginetest.NewRig(t, withBlobs(&portstest.MemoryBlobs{}), func(c *engine.Config, _ *enginetest.OutFactory) { c.Authorizer = pol })
	note(t, r)
	reply := r.Reply("op-reply", "public answer")
	pol.internal[model.ActionMessageRead] = true
	pol.internal[model.ActionAttachmentRead] = true
	_, _, lines := export(t, r.C, r.Ctx, exportReq("op-export", model.Filter{Conversation: &r.Conv.Ref}))
	msgs := ofType[model.Message](t, lines, model.ExportLineMessage)
	if len(msgs) != 1 || msgs[0].Ref != *reply.Operation.ResultMessage {
		t.Errorf("messages = %+v, want the reply only", msgs)
	}
	if atts := ofType[model.Attachment](t, lines, model.ExportLineAttachment); len(atts) != 0 {
		t.Errorf("attachments = %+v, want none", atts)
	}
	for _, l := range lines {
		if strings.Contains(string(l.Data), secret) {
			t.Errorf("export carries the note's content: %s", l.Data)
		}
	}
}

// Export authorizes privacy.export on its subject before staging a byte,
// and Open authorizes it again on every call.
func TestExportAndOpenAuthorizeTheSubject(t *testing.T) {
	pol := &selectivePolicy{internal: map[string]bool{}, deny: map[string]bool{model.ActionPrivacyExport: true}}
	blobs := &portstest.MemoryBlobs{}
	r := enginetest.NewRig(t, withBlobs(blobs), func(c *engine.Config, _ *enginetest.OutFactory) { c.Authorizer = pol })
	_, err := r.C.Privacy().Export(r.Ctx, exportReq("op-denied", model.Filter{Conversation: &r.Conv.Ref}))
	enginetest.RequireCode(t, err, model.Unauthorized, "")
	if len(blobs.Keys()) != 0 {
		t.Fatal("a denied export staged bytes")
	}
	pol.deny = map[string]bool{}
	a, _, _ := export(t, r.C, r.Ctx, exportReq("op-export", model.Filter{Conversation: &r.Conv.Ref}))
	ref, _ := a.Operation.Result.AsExport()
	pol.deny[model.ActionPrivacyExport] = true
	_, rc, err := r.C.Privacy().Open(r.Ctx, model.Lookup[model.ExportRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
	enginetest.RequireCode(t, err, model.Unauthorized, "")
	if rc != nil {
		t.Fatal("a denied Open returned a reader")
	}
}

// Malformed, foreign or unsupported requests fail before anything is
// staged.
func TestExportRefusals(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	r := enginetest.NewRig(t, withBlobs(blobs))
	foreignConv := model.ConversationRef{Inbox: model.InboxRef{Tenant: enginetest.TenantB, ID: enginetest.InboxOne}, ID: enginetest.ConvOne}
	searchAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		req  model.ExportRequest
		code model.ErrorCode
	}{
		"no subject":     {exportReq("op-1", model.Filter{}), model.Validation},
		"kinds":          {exportReq("op-2", model.Filter{Conversation: &r.Conv.Ref, Kinds: []string{"x"}}), model.Validation},
		"format":         {model.ExportRequest{Command: cmd("op-3", ""), Filter: model.Filter{Conversation: &r.Conv.Ref}, Format: "csv"}, model.Unsupported},
		"foreign":        {exportReq("op-4", model.Filter{Conversation: &foreignConv}), model.NotFound},
		"missing":        {exportReq("op-5", model.Filter{Inboxes: []model.InboxRef{{Tenant: enginetest.TenantA, ID: enginetest.InboxTwo}}}), model.NotFound},
		"inbox mismatch": {exportReq("op-6", model.Filter{Conversation: &r.Conv.Ref, Inboxes: []model.InboxRef{{Tenant: enginetest.TenantA, ID: enginetest.InboxTwo}}}), model.Validation},
		"identity":       {exportReq("op-8", model.Filter{Conversation: &r.Conv.Ref, Identity: &model.IdentityRef{Inbox: r.Conv.Ref.Inbox, ID: r.Conv.Ref.ID}}), model.Validation},
		"task":           {exportReq("op-9", model.Filter{Conversation: &r.Conv.Ref, Task: &model.TaskRef{Conversation: r.Conv.Ref, ID: r.Conv.Ref.ID}}), model.Validation},
		"text":           {exportReq("op-10", model.Filter{Conversation: &r.Conv.Ref, Text: "invoice"}), model.Validation},
		"since":          {exportReq("op-11", model.Filter{Conversation: &r.Conv.Ref, Since: &searchAt}), model.Validation},
		"before":         {exportReq("op-12", model.Filter{Conversation: &r.Conv.Ref, Before: &searchAt}), model.Validation},
		"attachments":    {exportReq("op-13", model.Filter{Conversation: &r.Conv.Ref, Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/png"}}}), model.Validation},
		"actor":          {exportReq("op-14", model.Filter{Conversation: &r.Conv.Ref, Actor: new(model.ActorID("staff"))}), model.Validation},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.C.Privacy().Export(r.Ctx, c.req)
			enginetest.RequireCode(t, err, c.code, "")
		})
	}
	if len(blobs.Keys()) != 0 {
		t.Fatal("a refused export staged bytes")
	}
	_, _, err := r.C.Privacy().Open(r.Ctx, model.Lookup[model.ExportRef]{
		Call: model.Call{Correlation: "c"}, Ref: model.ExportRef{Tenant: enginetest.TenantB, ID: enginetest.ConvOne},
	})
	enginetest.RequireCode(t, err, model.NotFound, "")

	bare := enginetest.NewRig(t)
	_, err = bare.C.Privacy().Export(bare.Ctx, exportReq("op-7", model.Filter{Conversation: &bare.Conv.Ref}))
	enginetest.RequireCode(t, err, model.Disabled, "")
}

// A retry with the same operation ID replays the stored export and keeps
// none of the bytes it staged again; a different filter on the same
// subject conflicts.
func TestExportReplays(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	r := enginetest.NewRig(t, withBlobs(blobs))
	req := exportReq("op-export", model.Filter{Conversation: &r.Conv.Ref})
	a, _, _ := export(t, r.C, r.Ctx, req)
	again, err := r.C.Privacy().Export(r.Ctx, req)
	if err != nil || !again.Replayed || *again.Operation.Result != *a.Operation.Result {
		t.Fatalf("retry = %+v, %v; want the stored export", again, err)
	}
	if n := len(blobs.Keys()); n != 1 {
		t.Errorf("blobs = %d, want the one export", n)
	}
	other := req
	other.Filter = model.Filter{Conversation: &r.Conv.Ref, Inboxes: []model.InboxRef{r.Conv.Ref.Inbox}}
	_, err = r.C.Privacy().Export(r.Ctx, other)
	enginetest.RequireCode(t, err, model.Conflict, "")
	if n := len(blobs.Keys()); n != 1 {
		t.Errorf("blobs after a conflicting retry = %d, want 1", n)
	}
}

// A contact export holds the contact, its identities and the messages
// they authored, and nothing another contact wrote.
func TestExportContact(t *testing.T) {
	h := enginetest.NewHarness(t, enginetest.ChatType, func(c *engine.Config) { c.Blobs = &portstest.MemoryBlobs{} })
	in := h.Configure(enginetest.PerContact(false), enginetest.Dest)
	h.MustAccept(h.Inbound("e-1", "pm-1", "alice", enginetest.Dest))
	h.MustAccept(h.Inbound("e-2", "pm-2", "bob", enginetest.Dest))
	h.MustAccept(h.Inbound("e-3", "pm-3", "alice", enginetest.Dest))
	alice := h.MessageByProviderID(in.Ref, "pm-1")
	var contact model.ContactRef
	h.View(func(r store.Reader) error {
		id, err := r.Identity(h.Ctx, model.IdentityKey{Inbox: in.Ref, Namespace: enginetest.IngressNS, SourceID: "alice"})
		contact = id.Contact
		return err
	})
	_, x, lines := export(t, h.Client, h.Ctx, exportReq("op-export", model.Filter{Contact: &contact}))
	if x.Filter.Contact == nil || *x.Filter.Contact != contact {
		t.Fatalf("export filter = %+v", x.Filter)
	}
	if cs := ofType[model.Contact](t, lines, model.ExportLineContact); len(cs) != 1 || cs[0].Ref != contact {
		t.Errorf("contacts = %+v, want alice's", cs)
	}
	ids := ofType[model.Identity](t, lines, model.ExportLineIdentity)
	if len(ids) != 1 || ids[0].Ref != alice.Author.Identity {
		t.Errorf("identities = %+v, want alice's", ids)
	}
	msgs := ofType[model.Message](t, lines, model.ExportLineMessage)
	if len(msgs) != 2 || msgs[0].Ref != alice.Ref || msgs[1].Ref != h.MessageByProviderID(in.Ref, "pm-3").Ref {
		t.Errorf("messages = %+v, want alice's two", msgs)
	}
	if convs := ofType[model.Conversation](t, lines, model.ExportLineConversation); len(convs) != 1 || convs[0].Ref != alice.Ref.Conversation {
		t.Errorf("conversations = %+v, want alice's", convs)
	}
}

// Exports are retained for the Exports horizon: Purge deletes the record
// and its bytes, and clears the staging intent it recorded for them.
func TestExportsExpire(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	horizon := time.Hour
	r := enginetest.NewRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Blobs = blobs
		c.Retention = engine.RetentionPolicy{
			Events: horizon, Operations: horizon, Ingress: horizon, Attempts: horizon, Staging: horizon, Exports: horizon,
			Contacts: horizon,
		}
	})
	a, _, _ := export(t, r.C, r.Ctx, exportReq("op-export", model.Filter{Conversation: &r.Conv.Ref}))
	ref, _ := a.Operation.Result.AsExport()
	r.Clock.Advance(2 * horizon)
	rep, err := retention.Purge(r.Ctx, r.E, enginetest.PrincipalA())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if rep.Exports != 1 {
		t.Errorf("report = %+v, want one export purged", rep)
	}
	if keys := blobs.Keys(); len(keys) != 0 {
		t.Errorf("blobs = %v, want the export's bytes deleted", keys)
	}
	_, _, err = r.C.Privacy().Open(r.Ctx, model.Lookup[model.ExportRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
	enginetest.RequireCode(t, err, model.NotFound, "")
	err = r.E.Store().Transact(r.Ctx, enginetest.TenantA, func(tx store.Tx) error {
		b, err := tx.AbandonStaging(r.Ctx, store.StagingSweep{Before: r.Clock.Now().Add(time.Hour), Limit: 10})
		if err == nil && len(b.Intents) != 0 {
			t.Errorf("staging intents left = %+v", b.Intents)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
