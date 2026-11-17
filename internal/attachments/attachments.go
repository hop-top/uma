// Package attachments is the Attachments service: scoped attachment
// metadata reserved by Prepare, bytes staged by Upload through the host's
// blob store with bounded, verified streaming, and authorized reads by
// Open. The blob locator stays in the storage-only record; no result or
// error carries it.
package attachments

import (
	"context"
	"io"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Lifecycle states of a stored attachment record.
const (
	// StatePrepared: metadata reserved, no bytes.
	StatePrepared = "prepared"
	// StateStored: bytes staged and bound to the record.
	StateStored = "stored"
)

// Service is the Attachments service of one client.
type Service struct{ c *engine.Client }

// New returns the Attachments service bound to c.
func New(c *engine.Client) Service { return Service{c} }

// Prepare reserves an attachment in an inbox, as a committed operation: its
// name, media type, declared size and digest, and audience. The attachment
// is created under the requested audience and keeps it. The declared size
// must not exceed the size limit; the media type must be well formed and,
// when the engine lists accepted types, one of them; a declared digest must
// be sha256:<64 lowercase hex>. The inbox must exist in the principal's
// tenant. Without a configured blob store Prepare is model.Disabled.
// Retrying with the same operation ID and payload returns the stored
// operation; a different payload conflicts.
func (s Service) Prepare(ctx context.Context, req model.PrepareAttachment) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if e.Blobs() == nil {
		return model.Acceptance{}, errNoBlobs()
	}
	meta := req.Metadata
	if err := engine.FirstErr(
		checkSize(meta.Size, e.AttachmentSizeLimit()),
		checkMediaType(meta.MediaType, e.AttachmentMediaTypes()),
		checkDigest(meta.Digest),
	); err != nil {
		return model.Acceptance{}, err
	}
	if req.Inbox.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if err := view(ctx, c, func(r store.Reader) error { return loadInbox(ctx, r, req.Inbox) }); err != nil {
		return model.Acceptance{}, err
	}
	ids, err := allocate(ctx, e, 4)
	if err != nil {
		return model.Acceptance{}, err
	}
	ref := model.AttachmentRef{Inbox: req.Inbox, ID: ids[0]}
	res := ref.Ref()
	if err := c.Authorize(ctx, policy.Authorization{
		Action: model.ActionAttachmentPrepare, Resource: res, Audience: meta.Audience,
	}, res); err != nil {
		return model.Acceptance{}, err
	}
	rec := store.AttachmentRecord{
		Attachment: model.Attachment{Ref: ref, Metadata: meta, Revision: model.Revision(ids[1])},
		Stager:     c.Principal().Actor,
		State:      StatePrepared,
	}
	a, _, err := commit(ctx, c, command{
		kind: model.ActionAttachmentPrepare, target: req.Inbox.Ref(), result: res, command: req.Command,
		fingerprint: prepareFingerprint(req),
		apply: func(ctx context.Context, tx store.Tx, _ model.OperationRef) error {
			if err := loadInbox(ctx, tx, req.Inbox); err != nil {
				return err
			}
			return tx.PutAttachment(ctx, rec, "")
		},
	}, ids[2], model.Revision(ids[3]))
	return a, err
}

// Upload streams an attachment's bytes into the blob store, as a committed
// operation, after authorizing the upload under the attachment's audience
// and before reading any byte. It reads at most the size limit, or the
// declared size when one was declared, computes the sha256 digest while
// staging and rejects, with model.Validation and nothing kept, bytes past
// the limit, a length other than a declared size and a digest other than a
// declared one. The stored metadata then records the computed digest and
// length. A set ExpectedRevision must equal the attachment's revision.
//
// The bytes go under a fresh key in the principal's tenant, recorded as a
// staging intent before Stage writes any byte. The commit binds the key
// in the record and the intent in one unit; an intent the retention sweep
// abandoned, because staging outlived the staging horizon, fails the
// commit with model.Conflict.
//
// The operation fingerprint covers the computed digest and length, so a
// retry with the same operation ID replays only for the same bytes and
// conflicts otherwise, declared digest or not. Uploading to an attachment
// that already holds bytes under another operation conflicts. A staged
// blob the commit does not bind is deleted and its intent cleared; after
// an unknown commit outcome the record is read back and the blob kept only
// if it is bound.
// Cancellation stops reading; context errors stay standard.
func (s Service) Upload(ctx context.Context, req model.Lookup[model.AttachmentRef], cmd model.Command, body io.Reader) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := engine.FirstErr(req.Validate(), req.Ref.Validate(), cmd.Validate(model.RevisionOptional)); err != nil {
		return model.Acceptance{}, err
	}
	blobs := e.Blobs()
	if blobs == nil {
		return model.Acceptance{}, errNoBlobs()
	}
	if body == nil {
		return model.Acceptance{}, model.NewError(model.Validation, "body: required")
	}
	rec, err := load(ctx, c, req.Ref)
	if err != nil {
		return model.Acceptance{}, err
	}
	res := req.Ref.Ref()
	if err := c.Authorize(ctx, policy.Authorization{
		Action: model.ActionAttachmentUpload, Resource: res,
		Audience: engine.ReadAudience(rec.Attachment.Metadata.Audience), Revision: cmd.ExpectedRevision,
	}, res); err != nil {
		return model.Acceptance{}, err
	}
	ids, err := allocate(ctx, e, 4)
	if err != nil {
		return model.Acceptance{}, err
	}
	declared := rec.Attachment.Metadata
	var key *ports.BlobKey
	var st *stager
	if rec.State == StateStored {
		// The attachment holds bytes already: this upload replays or
		// conflicts, and keeps nothing. Hash it without staging and let the
		// ledger compare it with the stored operation.
		st = newStager(ctx, body, 0, e.AttachmentSizeLimit(), "")
		if err := st.discard(); err != nil {
			return model.Acceptance{}, err
		}
	} else {
		k := ports.BlobKey{Tenant: c.Principal().Tenant, ID: ids[3]}
		if err := recordIntent(ctx, c, store.StagingIntent{Key: k, Attachment: req.Ref}); err != nil {
			return model.Acceptance{}, err
		}
		st = newStager(ctx, body, declared.Size, e.AttachmentSizeLimit(), declared.Digest)
		if err := blobs.Stage(ctx, k, st, stageSize(declared.Size)); err != nil {
			cleanup(ctx, c, blobs, k)
			return model.Acceptance{}, st.stageError(ctx, err)
		}
		if err := st.complete(); err != nil {
			cleanup(ctx, c, blobs, k)
			return model.Acceptance{}, err
		}
		key = &k
	}
	digest, n := st.sum()
	a, created, err := commit(ctx, c, command{
		kind: model.ActionAttachmentUpload, target: res, result: res, command: cmd,
		fingerprint: uploadFingerprint(req.Ref, digest, n, cmd.ExpectedRevision),
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			cur, err := getRecord(ctx, tx, req.Ref)
			if err != nil {
				return err
			}
			if cmd.ExpectedRevision != "" && cur.Attachment.Revision != cmd.ExpectedRevision {
				return model.NewError(model.Conflict, "attachment revision changed")
			}
			if cur.State != StatePrepared || key == nil {
				return model.NewError(model.Conflict, "attachment already uploaded")
			}
			if err := tx.BindStaging(ctx, *key); err != nil {
				if model.IsCode(err, model.NotFound) || model.IsCode(err, model.Conflict) {
					return model.NewError(model.Conflict, "staged bytes expired before the commit; upload again")
				}
				return err
			}
			next := cur.Clone()
			next.Attachment.Metadata.Digest, next.Attachment.Metadata.Size = digest, n
			next.Attachment.Revision = model.Revision(ids[1])
			next.Blob, next.StagingOperation, next.State = key, op, StateStored
			return tx.PutAttachment(ctx, next, cur.Attachment.Revision)
		},
	}, ids[0], model.Revision(ids[2]))
	if key != nil && (err != nil || !created) {
		reconcile(ctx, c, blobs, req.Ref, *key)
	}
	return a, err
}

// Open returns an attachment's public metadata and a reader over its
// bytes, after authorizing the read as Readable does on every call. The
// reader is caller-owned until closed; it fails with model.Permanent when
// the stored bytes differ from the recorded length or digest. An
// attachment without bytes is model.Conflict.
func (s Service) Open(ctx context.Context, req model.Lookup[model.AttachmentRef]) (model.Attachment, io.ReadCloser, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Attachment{}, nil, err
	}
	defer done()
	if err := engine.FirstErr(req.Validate(), req.Ref.Validate()); err != nil {
		return model.Attachment{}, nil, err
	}
	blobs := e.Blobs()
	if blobs == nil {
		return model.Attachment{}, nil, errNoBlobs()
	}
	rec, err := load(ctx, c, req.Ref)
	if err != nil {
		return model.Attachment{}, nil, err
	}
	if err := Readable(ctx, c, rec); err != nil {
		return model.Attachment{}, nil, err
	}
	if rec.State != StateStored || rec.Blob == nil {
		return model.Attachment{}, nil, model.NewError(model.Conflict, "attachment not uploaded")
	}
	rc, err := blobs.Open(ctx, *rec.Blob)
	if err != nil {
		return model.Attachment{}, nil, blobError(ctx, err)
	}
	meta := rec.Attachment.Metadata
	return rec.Attachment, newVerifiedReader(rc, meta.Size, meta.Digest), nil
}

func errNoBlobs() error {
	return model.NewError(model.Disabled, "attachment storage not configured")
}

// stageSize is the exact length passed to BlobStore.Stage: the declared
// size, or -1 when none was declared.
func stageSize(declared int64) int64 {
	if declared > 0 {
		return declared
	}
	return -1
}

// reconcile settles a staged blob after a commit that did not bind it, or
// whose outcome is unknown: the record is read back, and the blob cleaned
// up unless the record binds it. When the record cannot be read the blob
// and its intent are left in place, since it may be bound; the retention
// sweep deletes it past the staging horizon if it is not. Cleanup
// outlives the caller's cancellation.
func reconcile(ctx context.Context, c *engine.Client, blobs ports.BlobStore, ref model.AttachmentRef, key ports.BlobKey) {
	ctx = context.WithoutCancel(ctx)
	rec, err := load(ctx, c, ref)
	switch {
	case err == nil && rec.Blob != nil && *rec.Blob == key:
	case err == nil || model.IsCode(err, model.NotFound):
		cleanup(ctx, c, blobs, key)
	}
}

// recordIntent records, in its own unit before any byte is staged, the
// key an upload is about to stage under.
func recordIntent(ctx context.Context, c *engine.Client, in store.StagingIntent) error {
	return operations.Transact(ctx, c.Engine(), c.Principal().Tenant, func(tx store.Tx) error {
		return tx.RecordStaging(ctx, in)
	})
}

// cleanup deletes a staged blob nothing binds, then clears its intent. A
// failure of either leaves the intent for the retention sweep; it never
// fails the operation.
func cleanup(ctx context.Context, c *engine.Client, blobs ports.BlobStore, key ports.BlobKey) {
	ctx = context.WithoutCancel(ctx)
	if blobs.Delete(ctx, key) != nil {
		return
	}
	_ = operations.Transact(ctx, c.Engine(), c.Principal().Tenant, func(tx store.Tx) error {
		return tx.ClearStaging(ctx, key)
	})
}

// load reads an attachment record in the principal's tenant. A record
// under another reference is not found.
func load(ctx context.Context, c *engine.Client, ref model.AttachmentRef) (store.AttachmentRecord, error) {
	if ref.Inbox.Tenant != c.Principal().Tenant {
		return store.AttachmentRecord{}, engine.ErrNotFound()
	}
	var rec store.AttachmentRecord
	err := view(ctx, c, func(r store.Reader) (err error) {
		rec, err = getRecord(ctx, r, ref)
		return err
	})
	return rec, err
}

func getRecord(ctx context.Context, r store.Reader, ref model.AttachmentRef) (store.AttachmentRecord, error) {
	rec, err := r.Attachment(ctx, ref)
	if err != nil {
		return store.AttachmentRecord{}, err
	}
	if rec.Attachment.Ref != ref {
		return store.AttachmentRecord{}, engine.ErrNotFound()
	}
	return rec, nil
}

func loadInbox(ctx context.Context, r store.Reader, ref model.InboxRef) error {
	in, err := r.GetInbox(ctx, ref)
	if err != nil {
		return err
	}
	if in.Ref != ref {
		return engine.ErrNotFound()
	}
	return nil
}

func view(ctx context.Context, c *engine.Client, fn func(store.Reader) error) error {
	err := c.Engine().Store().View(ctx, c.Principal().Tenant, fn)
	if model.IsCode(err, model.NotFound) {
		return engine.ErrNotFound()
	}
	return err
}

// allocate returns n fresh IDs, allocated before any unit of work.
func allocate(ctx context.Context, e *engine.Engine, n int) ([]model.ID, error) {
	out := make([]model.ID, n)
	for i := range out {
		id, err := operations.NewID(ctx, e)
		if err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}
