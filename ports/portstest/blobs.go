package portstest

import (
	"bytes"
	"cmp"
	"context"
	"io"
	"slices"
	"sync"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// MemoryBlobs is an in-memory ports.BlobStore for fixtures. It is safe for
// concurrent use and keeps every staged blob until Delete; the zero value
// is ready to use.
type MemoryBlobs struct {
	mu    sync.Mutex
	blobs map[ports.BlobKey][]byte
}

// Stage reads r to EOF and stores its bytes under k, replacing what k
// held. It leaves k not held when k is invalid, r fails, ctx ends or the
// length differs from a non-negative size.
func (m *MemoryBlobs) Stage(ctx context.Context, k ports.BlobKey, r io.Reader, size int64) error {
	if err := k.Validate(); err != nil {
		return err
	}
	var buf bytes.Buffer
	_, err := io.Copy(&buf, r)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && size >= 0 && int64(buf.Len()) != size {
		err = model.NewError(model.Validation, "blob length differs from size")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		delete(m.blobs, k)
		return err
	}
	if m.blobs == nil {
		m.blobs = map[ports.BlobKey][]byte{}
	}
	m.blobs[k] = buf.Bytes()
	return nil
}

// Open returns a reader over the blob at k.
func (m *MemoryBlobs) Open(ctx context.Context, k ports.BlobKey) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[k]
	if !ok {
		return nil, model.NewError(model.NotFound, "blob not found")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Delete removes the blob at k; an unknown key succeeds.
func (m *MemoryBlobs) Delete(ctx context.Context, k ports.BlobKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, k)
	return nil
}

// Keys returns the keys held now, ordered by tenant then ID.
func (m *MemoryBlobs) Keys() []ports.BlobKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ports.BlobKey, 0, len(m.blobs))
	for k := range m.blobs {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b ports.BlobKey) int {
		return cmp.Or(cmp.Compare(a.Tenant, b.Tenant), cmp.Compare(a.ID, b.ID))
	})
	return out
}
