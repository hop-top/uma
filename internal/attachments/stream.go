package attachments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"

	"hop.top/uma/model"
)

// stager is the reader Upload hands to the blob store. It bounds the
// caller's body, hashes it and checks it against the declared length and
// digest at EOF, so a store copying it fails before keeping anything. Its
// first failure sticks: every later Read returns it.
type stager struct {
	ctx    context.Context
	r      io.Reader
	limit  int64  // most bytes accepted
	size   int64  // declared length; 0 when undeclared
	digest string // declared digest; "" when undeclared
	h      hash.Hash
	n      int64
	err    error
	eof    bool
}

func newStager(ctx context.Context, r io.Reader, size, limit int64, digest string) *stager {
	if size > 0 {
		limit = min(limit, size)
	}
	return &stager{ctx: ctx, r: r, limit: limit, size: size, digest: digest, h: sha256.New()}
}

func (s *stager) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.eof {
		return 0, io.EOF
	}
	if err := s.ctx.Err(); err != nil {
		return 0, s.fail(err)
	}
	// Read at most one byte past the limit, enough to see an overflow.
	if room := s.limit - s.n + 1; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := s.r.Read(p)
	if n > 0 {
		if s.n+int64(n) > s.limit {
			return 0, s.fail(s.tooLarge())
		}
		s.h.Write(p[:n])
		s.n += int64(n)
	}
	switch {
	case errors.Is(err, io.EOF):
		if ferr := s.finish(); ferr != nil {
			return 0, s.fail(ferr)
		}
		return n, io.EOF
	case err != nil:
		if cerr := s.ctx.Err(); cerr != nil {
			return 0, s.fail(cerr)
		}
		return 0, s.fail(model.NewError(model.Transient, "upload body could not be read"))
	}
	return n, nil
}

func (s *stager) fail(err error) error {
	s.err = err
	return err
}

func (s *stager) tooLarge() error {
	if s.size > 0 {
		return model.NewError(model.Validation, "upload is longer than the declared size")
	}
	return model.NewError(model.Validation, "upload exceeds the attachment size limit")
}

// finish checks the complete body against the declared length and digest.
func (s *stager) finish() error {
	s.eof = true
	if s.size > 0 && s.n != s.size {
		return model.NewError(model.Validation, "upload is shorter than the declared size")
	}
	if s.digest != "" && s.digest != s.computed() {
		return model.NewError(model.Validation, "upload does not match the declared digest")
	}
	return nil
}

func (s *stager) computed() string {
	return digestPrefix + hex.EncodeToString(s.h.Sum(nil))
}

// complete reports whether the store consumed the whole, valid body after
// a successful Stage. A store that stopped before EOF is read on: no more
// bytes completes the body, more mean the store kept a partial one.
func (s *stager) complete() error {
	if s.err != nil || s.eof {
		return s.err
	}
	var b [1]byte
	for {
		n, err := s.Read(b[:])
		switch {
		case n > 0:
			return s.fail(model.NewError(model.Unavailable, "attachment storage kept a partial upload"))
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
}

// discard reads the whole body without staging it.
func (s *stager) discard() error {
	if _, err := io.Copy(io.Discard, s); err != nil {
		return err
	}
	return s.err
}

// stageError explains a failed Stage: the stager's own failure when it had
// one (limits, digest, the caller's body, cancellation), the blob store's
// otherwise.
func (s *stager) stageError(ctx context.Context, err error) error {
	if s.err != nil {
		return s.err
	}
	return blobError(ctx, err)
}

// sum returns the computed digest and length of a complete body.
func (s *stager) sum() (string, int64) { return s.computed(), s.n }

// blobError maps a blob store failure to a safe error: context errors stay
// standard, anything else is model.Unavailable without the store's text,
// which may name keys, paths or buckets.
func blobError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return cerr
	}
	return model.NewError(model.Unavailable, "attachment storage unavailable")
}

// verifiedReader is the reader Open returns. It yields at most the
// recorded length and fails at EOF when the bytes differ from the recorded
// length or digest.
type verifiedReader struct {
	rc     io.ReadCloser
	size   int64
	digest string
	h      hash.Hash
	n      int64
	err    error
}

func newVerifiedReader(rc io.ReadCloser, size int64, digest string) *verifiedReader {
	return &verifiedReader{rc: rc, size: size, digest: digest, h: sha256.New()}
}

func (v *verifiedReader) Read(p []byte) (int, error) {
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
		if v.n != v.size || digestPrefix+hex.EncodeToString(v.h.Sum(nil)) != v.digest {
			v.err = errCorrupt()
			return 0, v.err
		}
		v.err = io.EOF
		return n, io.EOF
	case err != nil:
		v.err = model.NewError(model.Unavailable, "attachment storage unavailable")
		return 0, v.err
	}
	return n, nil
}

func (v *verifiedReader) Close() error {
	if err := v.rc.Close(); err != nil {
		return model.NewError(model.Unavailable, "attachment storage unavailable")
	}
	return nil
}

func errCorrupt() error {
	return model.NewError(model.Permanent, "attachment bytes do not match their recorded digest")
}
