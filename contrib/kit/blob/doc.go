// Package blob provides a [ports.BlobStore] backed by a hop.top/kit blob
// store ([hop.top/kit/go/storage/blob.Store]: Put, Get, Delete, Exists).
//
// The host picks the backend, configures and constructs it, and passes it
// to [New]: kit's blob/local over a directory, kit's blob/s3 over a
// *s3.Client the host built, or its own implementation. This package
// imports kit's interface only, never a backend: it links no provider SDK,
// registers nothing, reads no environment variable and starts nothing. A
// host using blob/s3 links the AWS SDK through its own import.
//
//	local, err := local.New("/var/lib/uma/blobs")
//	blobs, err := blob.New(local)
//
//	s3store := s3.New(client, "bucket", "uma") // client built by the host
//	blobs, err := blob.New(s3store)
//
// # Keys
//
// The engine chooses every key: a tenant and a canonical ID (lowercase
// UUID text, see model.ParseID). A blob is stored under
//
//	uma/attachments/<tenant>/<id>
//
// with the tenant escaped by url.PathEscape, so a "/" inside it stays in
// its segment. Stage rejects (model.Validation) an empty tenant, the
// tenants "." and "..", and any ID that is not canonical; Open and Delete
// treat such a key as not held. A key from a tampered record therefore
// cannot reach another tenant's objects or escape the layout.
//
// # Staging
//
// kit's Put takes no length, so Stage wraps the reader: it stops at the
// declared size, fails on a longer or shorter body and on cancellation,
// and after Put returns checks that the whole body was read. Staging a
// key again replaces its object, so a repeated Stage of the same bytes is
// idempotent. A failed Stage deletes the key, leaving it not held whatever
// Put kept. Bounding to the attachment limit and verifying the digest is
// the engine's job; this package keeps bytes only.
//
// # Errors
//
// Errors carry fixed text only. Backend errors are dropped, never wrapped,
// because they name keys, paths or buckets. A key the store does not hold
// is model.NotFound on Open and success on Delete (checked with Exists
// when the backend's error does not say); any other backend failure is
// model.Unavailable. Context cancellation and deadline errors are returned
// as the standard context errors; a failure of the caller's reader is
// returned unchanged.
package blob
