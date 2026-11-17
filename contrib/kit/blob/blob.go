package blob

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/url"

	kitblob "hop.top/kit/go/storage/blob"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// keyPrefix is the first segments of every object key.
const keyPrefix = "uma/attachments/"

// contentType is the type Put records: the engine keeps the attachment's
// media type in its own metadata.
const contentType = "application/octet-stream"

// New returns a ports.BlobStore that keeps blobs in store. The host builds
// store (any kit backend, or its own implementation) and owns its
// configuration and lifetime; the returned store never closes it.
func New(store kitblob.Store) (ports.BlobStore, error) {
	if store == nil {
		return nil, errors.New("contrib/kit/blob: nil store")
	}
	return &blobs{kit: store}, nil
}

type blobs struct{ kit kitblob.Store }

// Stage stores r's bytes under k, replacing what k held.
func (b *blobs) Stage(ctx context.Context, k ports.BlobKey, r io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := k.Validate(); err != nil {
		return err
	}
	key, ok := objectKey(k)
	if !ok {
		return model.NewError(model.Validation, "blob tenant is not a valid key segment")
	}
	body := &sized{ctx: ctx, r: r, size: size}
	err := b.kit.Put(ctx, key, body, contentType)
	if err == nil {
		err = body.complete()
	}
	if err != nil {
		// A failed Stage leaves the key not held, whatever Put kept.
		_ = b.kit.Delete(context.WithoutCancel(ctx), key)
		if body.err != nil {
			return body.err
		}
		return storeError(ctx, err)
	}
	return nil
}

// Open returns a reader over the blob at k.
func (b *blobs) Open(ctx context.Context, k ports.BlobKey) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, ok := objectKey(k)
	if !ok {
		return nil, errNotFound()
	}
	rc, err := b.kit.Get(ctx, key)
	if err != nil {
		if b.missing(ctx, key, err) {
			return nil, errNotFound()
		}
		return nil, storeError(ctx, err)
	}
	return &reader{rc: rc}, nil
}

// Delete removes the blob at k; a key the store does not hold succeeds.
func (b *blobs) Delete(ctx context.Context, k ports.BlobKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, ok := objectKey(k)
	if !ok {
		return nil
	}
	err := b.kit.Delete(ctx, key)
	if err == nil || b.missing(ctx, key, err) {
		return nil
	}
	return storeError(ctx, err)
}

// missing reports whether a failed Get or Delete failed because key is not
// held: the backend says so, or Exists confirms it.
func (b *blobs) missing(ctx context.Context, key string, err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	ok, xerr := b.kit.Exists(ctx, key)
	return xerr == nil && !ok
}

// segment escapes a tenant into one key segment.
func segment(t model.TenantID) (string, bool) {
	s := string(t)
	if s == "" || s == "." || s == ".." {
		return "", false
	}
	return url.PathEscape(s), true
}

// objectKey returns the object key of k, or false when k is not a valid
// key: a tenant that is no key segment or an ID that is not canonical.
func objectKey(k ports.BlobKey) (string, bool) {
	seg, ok := segment(k.Tenant)
	if !ok || k.ID.Validate() != nil {
		return "", false
	}
	return keyPrefix + seg + "/" + string(k.ID), true
}

func errNotFound() error { return model.NewError(model.NotFound, "blob not found") }

// storeError maps a backend failure to fixed text: context errors stay
// standard, anything else is model.Unavailable.
func storeError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return cerr
	}
	return model.NewError(model.Unavailable, "blob store unavailable")
}

// sized is the reader Put receives. It enforces a non-negative size,
// stops on cancellation and records its first failure.
type sized struct {
	ctx  context.Context
	r    io.Reader
	size int64
	n    int64
	eof  bool
	err  error
}

func (s *sized) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.eof {
		return 0, io.EOF
	}
	if err := s.ctx.Err(); err != nil {
		return 0, s.fail(err)
	}
	if s.size >= 0 {
		// At most one byte past the size, enough to see a longer body.
		if room := s.size - s.n + 1; int64(len(p)) > room {
			p = p[:room]
		}
	}
	n, err := s.r.Read(p)
	if s.size >= 0 && s.n+int64(n) > s.size {
		return 0, s.fail(model.NewError(model.Validation, "blob is longer than its size"))
	}
	s.n += int64(n)
	switch {
	case errors.Is(err, io.EOF):
		s.eof = true
		if s.size >= 0 && s.n != s.size {
			return 0, s.fail(model.NewError(model.Validation, "blob is shorter than its size"))
		}
		return n, io.EOF
	case err != nil:
		return 0, s.fail(err)
	}
	return n, nil
}

func (s *sized) fail(err error) error {
	s.err = err
	return err
}

// complete checks, after a successful Put, that the backend read the whole
// body: a backend that stopped before EOF is read on, and more bytes mean
// it kept a partial blob.
func (s *sized) complete() error {
	if s.err != nil || s.eof {
		return s.err
	}
	var b [1]byte
	for {
		n, err := s.Read(b[:])
		switch {
		case n > 0:
			return s.fail(model.NewError(model.Unavailable, "blob store kept a partial blob"))
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
}

// reader drops backend error text from reads and Close.
type reader struct{ rc io.ReadCloser }

func (r *reader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, model.NewError(model.Unavailable, "blob store unavailable")
	}
	return n, err
}

func (r *reader) Close() error {
	if err := r.rc.Close(); err != nil {
		return model.NewError(model.Unavailable, "blob store unavailable")
	}
	return nil
}
