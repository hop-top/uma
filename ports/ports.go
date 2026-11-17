// Package ports exposes injectable infrastructure, with lifecycle owned by the host.
package ports

import (
	"context"
	"io"
	"net/http"
	"time"

	"hop.top/uma/model"
)

// Clock is the engine's time source. SystemClock is the opt-in production
// clock; see portstest.FixedClock for fixtures.
type Clock interface{ Now() time.Time }

// IDs allocates internal resource IDs. Implementations are safe for concurrent
// use, return canonical RFC 9562 UUID text (see model.ParseID; UUIDv7 is the
// default choice, any version is accepted) and never return a value already
// returned by the same instance. Core validates every value: a generator error
// aborts the unit of work before any write as transient, malformed output as
// permanent. Core never derives time or ordering from an ID. UUIDv7 is the
// opt-in production generator; see portstest.SequenceIDs for fixtures.
type IDs interface {
	New(context.Context) (model.ID, error)
}
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// BlobKey locates one staged blob. The engine chooses it before staging:
// Tenant is the tenant the bytes belong to and ID a fresh internal ID from
// IDs, so the engine can record the key before any byte is written and
// find a blob a crash left behind. The engine keeps the key in
// storage-only records and never returns it through a public result or
// error.
type BlobKey struct {
	Tenant model.TenantID `json:"tenant"`
	ID     model.ID       `json:"id"`
}

// Validate requires a tenant and a canonical ID (see model.ParseID).
func (k BlobKey) Validate() error {
	if k.Tenant == "" {
		return model.NewError(model.Validation, "blob key tenant: required")
	}
	if k.ID.Validate() != nil {
		return model.NewError(model.Validation, "blob key id: must be canonical lowercase UUID text")
	}
	return nil
}

// BlobStore keeps attachment bytes. Implementations are safe for
// concurrent use. The engine authorizes every call, chooses every key,
// bounds and verifies the bytes it passes to Stage and owns attachment
// metadata and cleanup; a store keeps bytes only and never decides access.
//
// Stage reads r to EOF and stores its bytes under key. A key failing
// BlobKey.Validate is model.Validation and stores nothing. size is the
// exact length r yields, or -1 when the caller does not know it. Staging
// a key that already holds bytes replaces them, so repeating a Stage of
// the same bytes is idempotent: it succeeds and the key holds those bytes
// once. When r fails, the context ends or the length differs from a
// non-negative size, Stage returns an error and leaves the key not held.
//
// Open returns the bytes staged under a key; the caller closes the reader.
// A key the store does not hold is model.NotFound: keys of different
// tenants are different keys, whatever their IDs.
//
// Delete removes a key. Deleting a key the store does not hold succeeds,
// so cleanup can be retried.
//
// Errors carry no key, path or provider detail: the engine maps them to
// its own safe errors.
type BlobStore interface {
	Stage(ctx context.Context, key BlobKey, r io.Reader, size int64) error
	Open(context.Context, BlobKey) (io.ReadCloser, error)
	Delete(context.Context, BlobKey) error
}
type Waiter interface {
	Wait(context.Context, time.Duration) error
}
