package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// BlobFactory returns a fresh, empty blob store for one subtest. It may
// register cleanup with t.Cleanup.
type BlobFactory func(t *testing.T) ports.BlobStore

// RunBlobStoreSuite runs the ports.BlobStore contract against factory:
// round trips with known and unknown sizes under caller-chosen keys, key
// validation, idempotent re-staging, tenant scope, deletion and its retry,
// failed and mismatched staging, cancellation, errors free of locators,
// and concurrent use.
func RunBlobStoreSuite(t *testing.T, factory BlobFactory) {
	t.Helper()
	for _, c := range []struct {
		name string
		run  func(*testing.T, ports.BlobStore)
	}{
		{"RoundTrip", blobRoundTrip},
		{"KeyChoice", blobKeyChoice},
		{"InvalidKey", blobInvalidKey},
		{"IdempotentRestage", blobIdempotentRestage},
		{"TenantScope", blobTenantScope},
		{"Delete", blobDelete},
		{"FailedStage", blobFailedStage},
		{"SizeMismatch", blobSizeMismatch},
		{"Cancelled", blobCancelled},
		{"ErrorsCarryNoLocator", blobErrorsCarryNoLocator},
		{"Concurrent", blobConcurrent},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, factory(t)) })
	}
}

const (
	blobTenant model.TenantID = "conformance-blobs"
	blobOther  model.TenantID = "conformance-blobs-other"
)

// newBlobKey returns a fresh key in tenant with a random canonical ID, as
// the engine chooses one.
func newBlobKey(t *testing.T, tenant model.TenantID) ports.BlobKey {
	t.Helper()
	b := randomBytes(t, 16)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	id := model.ID(h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:])
	if err := id.Validate(); err != nil {
		t.Fatal(err)
	}
	return ports.BlobKey{Tenant: tenant, ID: id}
}

func stageAt(t *testing.T, s ports.BlobStore, k ports.BlobKey, b []byte, size int64) {
	t.Helper()
	if err := s.Stage(t.Context(), k, bytes.NewReader(b), size); err != nil {
		t.Fatalf("Stage: %v", err)
	}
}

func stageBytes(t *testing.T, s ports.BlobStore, tenant model.TenantID, b []byte, size int64) ports.BlobKey {
	t.Helper()
	k := newBlobKey(t, tenant)
	stageAt(t, s, k, b, size)
	return k
}

func readBlob(t *testing.T, s ports.BlobStore, k ports.BlobKey) []byte {
	t.Helper()
	rc, err := s.Open(t.Context(), k)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func requireBlobNotFound(t *testing.T, s ports.BlobStore, k ports.BlobKey) {
	t.Helper()
	rc, err := s.Open(t.Context(), k)
	if err == nil {
		_ = rc.Close()
		t.Fatalf("Open(%+v) succeeded, want not_found", k)
	}
	if !model.IsCode(err, model.NotFound) {
		t.Fatalf("Open(%+v) = %v, want not_found", k, err)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func blobRoundTrip(t *testing.T, s ports.BlobStore) {
	for _, tc := range []struct {
		name string
		data []byte
		size func(int) int64
	}{
		{"KnownSize", []byte("hello, attachment"), func(n int) int64 { return int64(n) }},
		{"UnknownSize", randomBytes(t, 3<<20+7), func(int) int64 { return -1 }},
		{"Empty", nil, func(int) int64 { return 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := stageBytes(t, s, blobTenant, tc.data, tc.size(len(tc.data)))
			if got := readBlob(t, s, k); !bytes.Equal(got, tc.data) {
				t.Fatalf("read %d bytes, want %d", len(got), len(tc.data))
			}
		})
	}
}

// blobKeyChoice: bytes land under exactly the key the caller chose, and
// distinct keys hold distinct blobs.
func blobKeyChoice(t *testing.T, s ports.BlobStore) {
	a, b := newBlobKey(t, blobTenant), newBlobKey(t, blobTenant)
	requireBlobNotFound(t, s, a)
	stageAt(t, s, a, []byte("same"), 4)
	requireBlobNotFound(t, s, b)
	stageAt(t, s, b, []byte("same"), 4)
	if got := readBlob(t, s, a); string(got) != "same" {
		t.Fatalf("blob at the chosen key = %q", got)
	}
	if err := s.Delete(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	requireBlobNotFound(t, s, a)
	if got := readBlob(t, s, b); string(got) != "same" {
		t.Fatalf("second blob = %q after deleting the first", got)
	}
}

// blobInvalidKey: a key without a tenant or with a non-canonical ID is
// rejected by Stage and never held.
func blobInvalidKey(t *testing.T, s ports.BlobStore) {
	good := newBlobKey(t, blobTenant)
	for _, k := range []ports.BlobKey{
		{ID: good.ID},
		{Tenant: blobTenant},
		{Tenant: blobTenant, ID: "blob-1"},
		{Tenant: blobTenant, ID: "../" + good.ID},
		{Tenant: blobTenant, ID: model.ID(strings.ToUpper(string(good.ID)))},
	} {
		err := s.Stage(t.Context(), k, strings.NewReader("x"), 1)
		if !model.IsCode(err, model.Validation) {
			t.Errorf("Stage(%+v) = %v, want validation", k, err)
		}
		requireBlobNotFound(t, s, k)
		if err := s.Delete(t.Context(), k); err != nil {
			t.Errorf("Delete(%+v) = %v, want success", k, err)
		}
	}
}

// blobIdempotentRestage: staging a key again with the same bytes succeeds
// and leaves one blob; other bytes replace it; a failed re-stage leaves
// the key not held.
func blobIdempotentRestage(t *testing.T, s ports.BlobStore) {
	k := stageBytes(t, s, blobTenant, []byte("again"), 5)
	stageAt(t, s, k, []byte("again"), 5)
	if got := readBlob(t, s, k); string(got) != "again" {
		t.Fatalf("re-staged blob = %q", got)
	}
	if err := s.Delete(t.Context(), k); err != nil {
		t.Fatal(err)
	}
	requireBlobNotFound(t, s, k)

	stageAt(t, s, k, []byte("first"), -1)
	stageAt(t, s, k, []byte("second!"), -1)
	if got := readBlob(t, s, k); string(got) != "second!" {
		t.Fatalf("re-staged blob = %q, want the later bytes", got)
	}
	if err := s.Stage(t.Context(), k, &failingReader{n: 3}, -1); err == nil {
		t.Fatal("re-stage over a failing reader succeeded")
	}
	requireBlobNotFound(t, s, k)
}

func blobTenantScope(t *testing.T, s ports.BlobStore) {
	k := stageBytes(t, s, blobTenant, []byte("tenant a"), -1)
	foreign := ports.BlobKey{Tenant: blobOther, ID: k.ID}
	requireBlobNotFound(t, s, foreign)
	if err := s.Delete(t.Context(), foreign); err != nil {
		t.Fatalf("Delete of a foreign key: %v", err)
	}
	if got := readBlob(t, s, k); string(got) != "tenant a" {
		t.Fatalf("blob = %q after a foreign delete", got)
	}
	requireBlobNotFound(t, s, ports.BlobKey{Tenant: blobTenant, ID: k.ID + "x"})
	requireBlobNotFound(t, s, ports.BlobKey{Tenant: blobTenant, ID: "../" + k.ID})
	// The same ID staged in the other tenant is another blob.
	stageAt(t, s, foreign, []byte("tenant b"), -1)
	if got := readBlob(t, s, k); string(got) != "tenant a" {
		t.Fatalf("blob = %q after staging the ID in another tenant", got)
	}
	if got := readBlob(t, s, foreign); string(got) != "tenant b" {
		t.Fatalf("foreign blob = %q", got)
	}
}

func blobDelete(t *testing.T, s ports.BlobStore) {
	k := stageBytes(t, s, blobTenant, []byte("gone"), 4)
	if err := s.Delete(t.Context(), k); err != nil {
		t.Fatal(err)
	}
	requireBlobNotFound(t, s, k)
	if err := s.Delete(t.Context(), k); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

// failingReader yields n bytes, then fails.
type failingReader struct{ n int }

var errReader = errors.New("conformance: reader failed")

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, errReader
	}
	n := min(len(p), r.n)
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.n -= n
	return n, nil
}

func blobFailedStage(t *testing.T, s ports.BlobStore) {
	k := newBlobKey(t, blobTenant)
	if err := s.Stage(t.Context(), k, &failingReader{n: 1 << 16}, -1); err == nil {
		t.Fatal("Stage succeeded over a failing reader")
	}
	requireBlobNotFound(t, s, k)
}

func blobSizeMismatch(t *testing.T, s ports.BlobStore) {
	for _, size := range []int64{3, 10} {
		k := newBlobKey(t, blobTenant)
		if err := s.Stage(t.Context(), k, strings.NewReader("12345"), size); err == nil {
			t.Fatalf("Stage of 5 bytes with size %d succeeded", size)
		}
		requireBlobNotFound(t, s, k)
	}
}

func blobCancelled(t *testing.T, s ports.BlobStore) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	k := newBlobKey(t, blobTenant)
	if err := s.Stage(ctx, k, strings.NewReader("late"), 4); err == nil {
		t.Fatal("Stage succeeded on a cancelled context")
	}
	requireBlobNotFound(t, s, k)
}

func blobErrorsCarryNoLocator(t *testing.T, s ports.BlobStore) {
	k := newBlobKey(t, blobTenant)
	_, err := s.Open(t.Context(), k)
	if err == nil {
		t.Fatal("Open of an unknown key succeeded")
	}
	if strings.Contains(err.Error(), string(k.ID)) {
		t.Fatalf("error %q carries the key", err)
	}
}

func blobConcurrent(t *testing.T, s ports.BlobStore) {
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	keys := make([]ports.BlobKey, 16)
	for i := range keys {
		keys[i] = newBlobKey(t, blobTenant)
	}
	for i := range 16 {
		wg.Go(func() {
			want := bytes.Repeat([]byte{"abcdefghijklmnop"[i]}, 1024+i)
			k := keys[i]
			if err := s.Stage(t.Context(), k, bytes.NewReader(want), int64(len(want))); err != nil {
				errs <- err
				return
			}
			rc, err := s.Open(t.Context(), k)
			if err != nil {
				errs <- err
				return
			}
			got, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil || !bytes.Equal(got, want) {
				errs <- errors.New("concurrent round trip mismatch")
				return
			}
			errs <- s.Delete(t.Context(), k)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}
