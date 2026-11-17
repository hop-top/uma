package privacy_test

import (
	"context"
	"strings"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// noteReaders denies reading internal messages to the actor "reader", and
// to everyone once revoked; attachments alone deny reading internal
// attachments to everyone.
type noteReaders struct{ revoked, attachments bool }

func (p *noteReaders) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	internal := a.Audience.Visibility == model.Internal
	switch {
	case internal && a.Action == model.ActionMessageRead && (a.Principal.Actor == "reader" || p.revoked),
		internal && a.Action == model.ActionAttachmentRead && (a.Principal.Actor == "reader" || p.revoked || p.attachments):
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true}, nil
}

// Open reauthorizes every record an export holds against the opener's
// current permissions: another actor allowed to export the conversation
// but not to read its note, the creator after losing that permission, and
// the creator after losing only the note's attachment, are all refused
// before any byte, as a direct read would be.
func TestOpenReauthorizesExportedRecords(t *testing.T) {
	p := &noteReaders{}
	r := enginetest.NewRig(t, withBlobs(&portstest.MemoryBlobs{}), func(c *engine.Config, _ *enginetest.OutFactory) { c.Authorizer = p })
	n, att := note(t, r)
	a, _, _ := export(t, r.C, r.Ctx, exportReq("op-export", model.Filter{Conversation: &r.Conv.Ref}))
	ref, _ := a.Operation.Result.AsExport()
	reader, err := enginetest.For(r.E, model.Principal{Tenant: enginetest.TenantA, Actor: "reader"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                 string
		c                    enginetest.Client
		revoked, attachments bool
	}{
		{"other actor", reader, false, false},
		{"revoked creator", r.C, true, false},
		{"creator without the attachment", r.C, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.revoked, p.attachments = tc.revoked, tc.attachments
			call := model.Call{Correlation: "c"}
			_, err := tc.c.Messages().Get(r.Ctx, model.Lookup[model.MessageRef]{Call: call, Ref: n.Ref})
			_, _, aerr := tc.c.Attachments().Open(r.Ctx, model.Lookup[model.AttachmentRef]{Call: call, Ref: att})
			if !model.IsCode(err, model.Unauthorized) && !model.IsCode(aerr, model.Unauthorized) {
				t.Fatalf("direct reads not denied: message %v, attachment %v", err, aerr)
			}
			_, rc, err := tc.c.Privacy().Open(r.Ctx, model.Lookup[model.ExportRef]{Call: call, Ref: ref})
			enginetest.RequireCode(t, err, model.Unauthorized, "")
			if rc != nil {
				_ = rc.Close()
				t.Fatal("a refused Open returned a reader")
			}
		})
	}
	p.revoked, p.attachments = false, false
	open(t, r.C, r.Ctx, ref)
}

// An export recorded without the list of the records it holds cannot be
// reauthorized and is refused.
func TestOpenRefusesExportWithoutManifest(t *testing.T) {
	r := enginetest.NewRig(t, withBlobs(&portstest.MemoryBlobs{}))
	a, _, _ := export(t, r.C, r.Ctx, exportReq("op-export", model.Filter{Conversation: &r.Conv.Ref}))
	ref, _ := a.Operation.Result.AsExport()
	err := r.E.Store().Transact(r.Ctx, enginetest.TenantA, func(tx store.Tx) error {
		rec, err := tx.Export(r.Ctx, ref)
		if err != nil {
			return err
		}
		prev := rec.Export.Revision
		rec.Manifest, rec.Export.Revision = nil, "rev-without-manifest"
		return tx.PutExport(r.Ctx, rec, prev)
	})
	if err != nil {
		t.Fatal(err)
	}
	_, rc, err := r.C.Privacy().Open(r.Ctx, model.Lookup[model.ExportRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
	enginetest.RequireCode(t, err, model.Unauthorized, "")
	if rc != nil {
		t.Fatal("a refused Open returned a reader")
	}
}

// An export built before one of its records was redacted would serve the
// erased content: Open refuses it with model.Conflict, before any byte, so
// the caller exports again. A record already a tombstone when exported
// does not stale the export.
func TestOpenRefusesExportSupersededByRedaction(t *testing.T) {
	call := model.Call{Correlation: "c"}
	requireSuperseded := func(t *testing.T, r *enginetest.Rig, ref model.ExportRef) {
		t.Helper()
		_, rc, err := r.C.Privacy().Open(r.Ctx, model.Lookup[model.ExportRef]{Call: call, Ref: ref})
		enginetest.RequireCode(t, err, model.Conflict, "")
		if rc != nil {
			_ = rc.Close()
			t.Fatal("a refused Open returned a reader")
		}
	}
	exportConv := func(r *enginetest.Rig, op model.OperationID) model.ExportRef {
		a, _, _ := export(t, r.C, r.Ctx, exportReq(op, model.Filter{Conversation: &r.Conv.Ref}))
		ref, _ := a.Operation.Result.AsExport()
		return ref
	}
	t.Run("message", func(t *testing.T) {
		r := enginetest.NewRig(t, withBlobs(&portstest.MemoryBlobs{}))
		n, _ := note(t, r)
		before := exportConv(r, "op-export")
		if _, err := r.C.Privacy().Redact(r.Ctx, redactMessage(n, "op-redact")); err != nil {
			t.Fatalf("Redact: %v", err)
		}
		requireSuperseded(t, r, before)
		// Exported as a tombstone, the message no longer stales the export.
		_, lines := open(t, r.C, r.Ctx, exportConv(r, "op-export-after"))
		for _, l := range lines {
			if strings.Contains(string(l.Data), secret) {
				t.Fatalf("export after redaction carries the erased content: %s", l.Data)
			}
		}
	})
	t.Run("attachment", func(t *testing.T) {
		r := enginetest.NewRig(t, withBlobs(&portstest.MemoryBlobs{}))
		_, att := note(t, r)
		before := exportConv(r, "op-export")
		var rev model.Revision
		err := r.E.Store().View(r.Ctx, enginetest.TenantA, func(rd store.Reader) error {
			rec, err := rd.Attachment(r.Ctx, att)
			rev = rec.Attachment.Revision
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.C.Privacy().Redact(r.Ctx, model.Redact{Command: cmd("op-redact", rev), Target: att.Ref(), Reason: "erasure"}); err != nil {
			t.Fatalf("Redact: %v", err)
		}
		requireSuperseded(t, r, before)
	})
}
