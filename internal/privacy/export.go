package privacy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"slices"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// page bounds the records one export read unit returns.
const page = 200

// Export writes the records req.Filter selects to a new export artifact,
// as a committed operation, and returns the export's reference as the
// operation result. The filter names the subject: a conversation, a
// contact, or one or more inboxes; Kinds and Statuses are not supported.
// The principal must be allowed model.ActionPrivacyExport on every
// subject before a byte is staged.
//
// The artifact is JSONL, one model.ExportLine per line: the export header,
// then for a contact the contact and its identities, then the
// conversations, the messages in recording order, the interactions in
// recording order (never with a response token) and the metadata of the
// attachments the messages reference. A contact export holds the messages
// its identities authored, the interactions they performed and their
// conversations. Each record is included
// only when the principal may read it under its own audience, so an
// internal note appears only where the principal may read that note;
// attachment bytes stay behind Attachments.Open.
//
// The bytes go to the blob store under an engine-chosen key, recorded as a
// staging intent before any byte is written and bound in the committing
// unit. Retrying with the same operation ID and payload returns the stored
// export and keeps none of the retry's bytes; a different payload
// conflicts. Without a blob store Export is model.Disabled.
func (s Service) Export(ctx context.Context, req model.ExportRequest) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Format != model.ExportFormatJSONL {
		return model.Acceptance{}, model.NewError(model.Unsupported, "format: only jsonl is supported")
	}
	blobs := e.Blobs()
	if blobs == nil {
		return model.Acceptance{}, model.NewError(model.Disabled, "export storage not configured")
	}
	filter := req.Filter.Clone()
	subjects, err := subjectsOf(filter, c.Principal().Tenant)
	if err != nil {
		return model.Acceptance{}, err
	}
	if err := loadSubjects(ctx, c, filter); err != nil {
		return model.Acceptance{}, err
	}
	if err := authorizeSubjects(ctx, c, subjects, ""); err != nil {
		return model.Acceptance{}, err
	}
	ids, err := allocate(ctx, e, 4)
	if err != nil {
		return model.Acceptance{}, err
	}
	tenant := c.Principal().Tenant
	ref := model.ExportRef{Tenant: tenant, ID: ids[0]}
	key := ports.BlobKey{Tenant: tenant, ID: ids[1]}
	header := model.Export{
		Ref: ref, Filter: filter, Format: req.Format, CreatedAt: e.Clock().Now().UTC(), Revision: model.Revision(ids[0]),
	}
	if err := operations.Transact(ctx, e, tenant, func(tx store.Tx) error {
		return tx.RecordStaging(ctx, store.StagingIntent{Key: key, Export: ref})
	}); err != nil {
		return model.Acceptance{}, err
	}
	size, digest, manifest, err := stage(ctx, c, blobs, key, header)
	if err != nil {
		cleanup(ctx, c, blobs, key)
		return model.Acceptance{}, err
	}
	header.Size, header.Digest = size, digest
	a, created, err := commit(ctx, c, command{
		kind: model.ActionPrivacyExport, target: subjects[0], result: ref.Ref(), command: req.Command,
		fingerprint: exportFingerprint(filter, req.Format),
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			if err := tx.BindStaging(ctx, key); err != nil {
				return err
			}
			return tx.PutExport(ctx, store.ExportRecord{
				Export: header, Blob: key, Operation: op, Creator: c.Principal().Actor, Manifest: &manifest,
			}, "")
		},
	}, ids[2], model.Revision(ids[3]))
	if err != nil || !created {
		settle(ctx, c, blobs, ref, key)
	}
	return a, err
}

// Open returns an export's record and a reader over its bytes. On each
// call, before any byte, it authorizes model.ActionPrivacyExport on every
// subject of its filter and reauthorizes every record the export holds
// against its current state, as a direct read would: a record the opener
// may not read now, or one gone since, refuses the whole export with
// model.Unauthorized, as does an export recorded without a manifest; a
// message, interaction or attachment redacted since the export was built
// refuses it with model.Conflict, so erased content is never served. The opener
// exports again to get what it may read. The reader is caller-owned
// until closed; it fails with model.Permanent when the stored bytes differ
// from the recorded length or digest. An export past its retention
// horizon is model.NotFound.
func (s Service) Open(ctx context.Context, req model.Lookup[model.ExportRef]) (model.Export, io.ReadCloser, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Export{}, nil, err
	}
	defer done()
	if err := engine.FirstErr(req.Validate(), req.Ref.Validate()); err != nil {
		return model.Export{}, nil, err
	}
	blobs := e.Blobs()
	if blobs == nil {
		return model.Export{}, nil, model.NewError(model.Disabled, "export storage not configured")
	}
	if req.Ref.Tenant != c.Principal().Tenant {
		return model.Export{}, nil, engine.ErrNotFound()
	}
	var rec store.ExportRecord
	if err := view(ctx, c, func(r store.Reader) (err error) {
		rec, err = r.Export(ctx, req.Ref)
		return err
	}); err != nil {
		return model.Export{}, nil, err
	}
	if rec.Export.Ref != req.Ref {
		return model.Export{}, nil, engine.ErrNotFound()
	}
	subjects, err := subjectsOf(rec.Export.Filter, c.Principal().Tenant)
	if err != nil {
		return model.Export{}, nil, err
	}
	if err := authorizeSubjects(ctx, c, subjects, rec.Export.Revision); err != nil {
		return model.Export{}, nil, err
	}
	if err := reauthorize(ctx, c, rec.Manifest); err != nil {
		return model.Export{}, nil, err
	}
	rc, err := blobs.Open(ctx, rec.Blob)
	if err != nil {
		return model.Export{}, nil, storageError(ctx, err)
	}
	x := rec.Export.Clone()
	return x, &verified{rc: rc, size: x.Size, digest: x.Digest, h: sha256.New()}, nil
}

// subjectsOf returns the resources a filter exports: its conversation,
// its contact, or its inboxes. A conversation filter with inboxes must
// list the conversation's inbox.
func subjectsOf(f model.Filter, tenant model.TenantID) ([]model.Ref, error) {
	switch {
	case len(f.Kinds) > 0:
		return nil, model.NewError(model.Validation, "filter.kinds: not supported by export")
	case len(f.Statuses) > 0:
		return nil, model.NewError(model.Validation, "filter.statuses: not supported by export")
	case f.Identity != nil:
		return nil, model.NewError(model.Validation, "filter.identity: not supported by export")
	case f.Task != nil:
		return nil, model.NewError(model.Validation, "filter.task: not supported by export")
	case f.Text != "":
		return nil, model.NewError(model.Validation, "filter.text: not supported by export")
	case f.Since != nil:
		return nil, model.NewError(model.Validation, "filter.since: not supported by export")
	case f.Before != nil:
		return nil, model.NewError(model.Validation, "filter.before: not supported by export")
	case f.Attachments != nil:
		return nil, model.NewError(model.Validation, "filter.attachments: not supported by export")
	case f.Assignee != nil:
		return nil, model.NewError(model.Validation, "filter.assignee: not supported by export")
	case f.Snoozed != nil:
		return nil, model.NewError(model.Validation, "filter.snoozed: not supported by export")
	case f.SnoozeEnded != nil:
		return nil, model.NewError(model.Validation, "filter.snooze_ended: not supported by export")
	case f.Linked != nil:
		return nil, model.NewError(model.Validation, "filter.linked: not supported by export")
	case f.Unread != nil:
		return nil, model.NewError(model.Validation, "filter.unread: not supported by export")
	case f.Actor != nil:
		return nil, model.NewError(model.Validation, "filter.actor: not supported by export")
	case f.Conversation != nil && f.Contact != nil:
		return nil, model.NewError(model.Validation, "filter: conversation and contact are exclusive")
	}
	var out []model.Ref
	switch {
	case f.Conversation != nil:
		if len(f.Inboxes) > 0 && !slices.Contains(f.Inboxes, f.Conversation.Inbox) {
			return nil, model.NewError(model.Validation, "filter.inboxes: must list the conversation's inbox")
		}
		out = []model.Ref{f.Conversation.Ref()}
	case f.Contact != nil:
		out = []model.Ref{f.Contact.Ref()}
	case len(f.Inboxes) > 0:
		for _, in := range f.Inboxes {
			if r := in.Ref(); !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	default:
		return nil, model.NewError(model.Validation, "filter: a conversation, contact or inbox is required")
	}
	for _, in := range f.Inboxes {
		if in.Tenant != tenant {
			return nil, engine.ErrNotFound()
		}
	}
	for _, r := range out {
		if r.Tenant != tenant {
			return nil, engine.ErrNotFound()
		}
	}
	return out, nil
}

// loadSubjects requires every record the filter names to exist.
func loadSubjects(ctx context.Context, c *engine.Client, f model.Filter) error {
	return view(ctx, c, func(r store.Reader) error {
		for _, in := range f.Inboxes {
			got, err := r.GetInbox(ctx, in)
			if err != nil {
				return err
			}
			if got.Ref != in {
				return engine.ErrNotFound()
			}
		}
		if f.Conversation != nil {
			got, err := r.GetConversation(ctx, *f.Conversation)
			if err != nil {
				return err
			}
			if got.Ref != *f.Conversation {
				return engine.ErrNotFound()
			}
		}
		if f.Contact != nil {
			got, err := r.GetContact(ctx, *f.Contact)
			if err != nil {
				return err
			}
			if got.Ref != *f.Contact {
				return engine.ErrNotFound()
			}
		}
		return nil
	})
}

func authorizeSubjects(ctx context.Context, c *engine.Client, subjects []model.Ref, rev model.Revision) error {
	for _, s := range subjects {
		if err := c.Authorize(ctx, policy.Authorization{Action: model.ActionPrivacyExport, Resource: s, Revision: rev}, s); err != nil {
			return err
		}
	}
	return nil
}

func exportFingerprint(f model.Filter, format string) []byte {
	fp := operations.NewFingerprint()
	fp.Set("kind", operations.Field().Str(model.ActionPrivacyExport))
	filter, _ := json.Marshal(f)
	fp.Set("filter", operations.Field().Raw(filter))
	fp.Set("format", operations.Field().Str(format))
	return fp.Sum()
}

// stage writes the export's lines to the blob store under key, streaming
// them from the record reads, and returns their length, digest and
// manifest.
func stage(ctx context.Context, c *engine.Client, blobs ports.BlobStore, key ports.BlobKey, header model.Export) (int64, string, store.ExportManifest, error) {
	pr, pw := io.Pipe()
	h := sha256.New()
	w := &counter{w: io.MultiWriter(pw, h)}
	x := &exporter{ctx: ctx, c: c, enc: json.NewEncoder(w)}
	written := make(chan error, 1)
	go func() {
		err := x.run(header)
		_ = pw.CloseWithError(err)
		written <- err
	}()
	serr := blobs.Stage(ctx, key, pr, -1)
	_ = pr.CloseWithError(errors.New("export staging ended"))
	werr := <-written
	switch {
	case werr != nil:
		return 0, "", store.ExportManifest{}, werr
	case serr != nil:
		return 0, "", store.ExportManifest{}, storageError(ctx, serr)
	}
	return w.n, "sha256:" + hex.EncodeToString(h.Sum(nil)), x.manifest, nil
}

type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// settle keeps the staged bytes only when the stored export binds them:
// after a replay the stored export holds an earlier call's key, and after
// a failed or unknown commit the record is read back. When it cannot be
// read the bytes stay for the sweep.
func settle(ctx context.Context, c *engine.Client, blobs ports.BlobStore, ref model.ExportRef, key ports.BlobKey) {
	ctx = context.WithoutCancel(ctx)
	var rec store.ExportRecord
	err := view(ctx, c, func(r store.Reader) (err error) {
		rec, err = r.Export(ctx, ref)
		return err
	})
	switch {
	case err == nil && rec.Blob == key:
	case err == nil || model.IsCode(err, model.NotFound):
		cleanup(ctx, c, blobs, key)
	}
}

// cleanup deletes bytes nothing binds, then clears their intent. A failure
// of either leaves the intent for the retention sweep.
func cleanup(ctx context.Context, c *engine.Client, blobs ports.BlobStore, key ports.BlobKey) {
	ctx = context.WithoutCancel(ctx)
	if blobs.Delete(ctx, key) != nil {
		return
	}
	_ = operations.Transact(ctx, c.Engine(), c.Principal().Tenant, func(tx store.Tx) error {
		return tx.ClearStaging(ctx, key)
	})
}

// storageError keeps context errors standard and replaces any other blob
// store failure with fixed text.
func storageError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return cerr
	}
	if model.IsCode(err, model.NotFound) {
		return model.NewError(model.Permanent, "export bytes missing")
	}
	return model.NewError(model.Unavailable, "export storage unavailable")
}

// verified reads an export's bytes and fails when they differ from the
// recorded length or digest.
type verified struct {
	rc     io.ReadCloser
	size   int64
	digest string
	h      hash.Hash
	n      int64
	err    error
}

func errCorrupt() error {
	return model.NewError(model.Permanent, "export bytes differ from the recorded digest")
}

func (v *verified) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	if room := v.size - v.n + 1; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := v.rc.Read(p)
	if n > 0 {
		if v.n+int64(n) > v.size {
			v.err = errCorrupt()
			return 0, v.err
		}
		v.h.Write(p[:n])
		v.n += int64(n)
	}
	switch {
	case errors.Is(err, io.EOF):
		if v.n != v.size || "sha256:"+hex.EncodeToString(v.h.Sum(nil)) != v.digest {
			v.err = errCorrupt()
			return 0, v.err
		}
		v.err = io.EOF
		return n, io.EOF
	case err != nil:
		v.err = model.NewError(model.Unavailable, "export storage unavailable")
		return 0, v.err
	}
	return n, nil
}

func (v *verified) Close() error {
	if err := v.rc.Close(); err != nil {
		return model.NewError(model.Unavailable, "export storage unavailable")
	}
	return nil
}
