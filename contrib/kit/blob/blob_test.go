package blob_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kitblob "hop.top/kit/go/storage/blob"
	"hop.top/kit/go/storage/blob/local"

	"hop.top/uma/conformance"
	"hop.top/uma/contrib/kit/blob"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

func localStore(t *testing.T) (*local.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func newBlobs(t *testing.T, s kitblob.Store) ports.BlobStore {
	t.Helper()
	b, err := blob.New(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func localBlobs(t *testing.T) ports.BlobStore {
	s, _ := localStore(t)
	return newBlobs(t, s)
}

func memoryFixture(*testing.T) conformance.StoreFixture {
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	return conformance.StoreFixture{
		Store:   memory.New(memory.Options{Clock: clock}),
		Now:     clock.Now,
		Advance: func(d time.Duration) { clock.Advance(d) },
	}
}

func TestBlobStoreConformanceLocal(t *testing.T) {
	conformance.RunBlobStoreSuite(t, localBlobs)
}

func TestAttachmentConformanceLocal(t *testing.T) {
	conformance.RunAttachmentSuite(t, memoryFixture, localBlobs)
}

func TestNewRejectsNilStore(t *testing.T) {
	if _, err := blob.New(nil); err == nil {
		t.Fatal("New(nil) succeeded")
	}
}

func requireCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	var me *model.Error
	if !errors.As(err, &me) || me.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

// files lists every regular file under dir, staging temp files included.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// key returns a canonical key in tenant; n tells keys apart.
func key(tenant model.TenantID, n int) ports.BlobKey {
	return ports.BlobKey{Tenant: tenant, ID: model.ID(fmt.Sprintf("0190a5f2-0000-7000-8000-%012d", n))}
}

func TestKeyLayout(t *testing.T) {
	s, dir := localStore(t)
	b := newBlobs(t, s)
	k := key("acme/eu", 1)
	if err := b.Stage(t.Context(), k, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if got, want := files(t, dir), []string{"uma/attachments/acme%2Feu/" + string(k.ID)}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("files = %q, want %q", got, want)
	}
	for _, tenant := range []model.TenantID{"", ".", ".."} {
		err := b.Stage(t.Context(), key(tenant, 2), strings.NewReader("x"), 1)
		requireCode(t, err, model.Validation)
	}
	// Only canonical IDs reach the backend.
	for _, id := range []string{"../" + string(k.ID), strings.ToUpper(string(k.ID)), string(k.ID[:35]), strings.Repeat("a", 32)} {
		bad := ports.BlobKey{Tenant: "acme/eu", ID: model.ID(id)}
		requireCode(t, b.Stage(t.Context(), bad, strings.NewReader("x"), 1), model.Validation)
		_, err := b.Open(t.Context(), bad)
		requireCode(t, err, model.NotFound)
		if err := b.Delete(t.Context(), bad); err != nil {
			t.Fatalf("Delete(%q) = %v", id, err)
		}
	}
	if len(files(t, dir)) != 1 {
		t.Fatal("a malformed key reached the backend")
	}
}

// A re-stage replaces the object in place: one file, the later bytes.
func TestRestageReplaces(t *testing.T) {
	s, dir := localStore(t)
	b := newBlobs(t, s)
	k := key("t1", 1)
	for _, body := range []string{"first", "first", "second"} {
		if err := b.Stage(t.Context(), k, strings.NewReader(body), -1); err != nil {
			t.Fatal(err)
		}
	}
	if got := files(t, dir); len(got) != 1 {
		t.Fatalf("files = %q, want one object", got)
	}
	rc, err := b.Open(t.Context(), k)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if got, _ := io.ReadAll(rc); string(got) != "second" {
		t.Fatalf("blob = %q, want the later bytes", got)
	}
}

// faultyStore wraps a kit store with scripted failures whose text names
// the key, as backend errors do.
type faultyStore struct {
	kitblob.Store
	putRead   int  // bytes Put reads before returning
	putErr    bool // Put fails after reading
	getErr    bool
	deleteErr bool
	exists    *bool
}

func (f *faultyStore) Put(ctx context.Context, key string, r io.Reader, ct string) error {
	if f.putRead > 0 {
		buf := make([]byte, f.putRead)
		n, _ := io.ReadFull(r, buf)
		if err := f.Store.Put(ctx, key, bytes.NewReader(buf[:n]), ct); err != nil {
			return err
		}
		if f.putErr {
			return errors.New("put " + key + " failed")
		}
		return nil
	}
	return f.Store.Put(ctx, key, r, ct)
}

func (f *faultyStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if f.getErr {
		return nil, errors.New("get " + key + ": access denied")
	}
	return f.Store.Get(ctx, key)
}

func (f *faultyStore) Delete(ctx context.Context, key string) error {
	if f.deleteErr {
		return errors.New("delete " + key + ": access denied")
	}
	return f.Store.Delete(ctx, key)
}

func (f *faultyStore) Exists(ctx context.Context, key string) (bool, error) {
	if f.exists != nil {
		return *f.exists, nil
	}
	return f.Store.Exists(ctx, key)
}

func TestFailedPutLeavesNothing(t *testing.T) {
	s, dir := localStore(t)
	b := newBlobs(t, &faultyStore{Store: s, putRead: 3, putErr: true})
	err := b.Stage(t.Context(), key("t1", 1), strings.NewReader("abcdef"), -1)
	requireCode(t, err, model.Unavailable)
	if strings.Contains(err.Error(), "uma/") {
		t.Fatalf("error %q carries the key", err)
	}
	if got := files(t, dir); len(got) != 0 {
		t.Fatalf("failed Put left %q", got)
	}
}

// A backend that returns success before reading the whole body kept a
// partial blob: Stage fails and removes it.
func TestPartialPutIsRemoved(t *testing.T) {
	s, dir := localStore(t)
	b := newBlobs(t, &faultyStore{Store: s, putRead: 3})
	err := b.Stage(t.Context(), key("t1", 1), strings.NewReader("abcdef"), -1)
	requireCode(t, err, model.Unavailable)
	if got := files(t, dir); len(got) != 0 {
		t.Fatalf("partial Put left %q", got)
	}
	// Stopping exactly at a declared size is complete.
	k := key("t1", 2)
	if err := b.Stage(t.Context(), k, strings.NewReader("abc"), 3); err != nil {
		t.Fatalf("Stage at the declared size: %v", err)
	}
	rc, err := b.Open(t.Context(), k)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if got, _ := io.ReadAll(rc); string(got) != "abc" {
		t.Fatalf("blob = %q", got)
	}
}

var errBody = errors.New("caller body failed")

type failingBody struct{ n int }

func (f *failingBody) Read(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errBody
	}
	n := min(len(p), f.n)
	f.n -= n
	return n, nil
}

func TestReaderFailureReturnedUnchanged(t *testing.T) {
	s, dir := localStore(t)
	b := newBlobs(t, s)
	err := b.Stage(t.Context(), key("t1", 1), &failingBody{n: 10}, -1)
	if !errors.Is(err, errBody) {
		t.Fatalf("Stage = %v, want the body's error", err)
	}
	if got := files(t, dir); len(got) != 0 {
		t.Fatalf("failed body left %q", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := b.Stage(ctx, key("t1", 2), strings.NewReader("x"), 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stage on a cancelled context = %v", err)
	}
}

func TestBackendErrorsDropped(t *testing.T) {
	s, _ := localStore(t)
	f := &faultyStore{Store: s}
	b := newBlobs(t, f)
	k := key("t1", 1)
	if err := b.Stage(t.Context(), k, strings.NewReader("held"), 4); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	f.getErr, f.deleteErr, f.exists = true, true, &yes
	_, err := b.Open(t.Context(), k)
	requireCode(t, err, model.Unavailable)
	if strings.Contains(err.Error(), string(k.ID)) {
		t.Fatalf("Open error %q carries the key", err)
	}
	err = b.Delete(t.Context(), k)
	requireCode(t, err, model.Unavailable)
	if strings.Contains(err.Error(), string(k.ID)) {
		t.Fatalf("Delete error %q carries the key", err)
	}
	f.exists = &no
	_, err = b.Open(t.Context(), k)
	requireCode(t, err, model.NotFound)
	if err := b.Delete(t.Context(), k); err != nil {
		t.Fatalf("Delete of a key the backend does not hold = %v", err)
	}
}

func TestOpenDoesNotTouchCallerFiles(t *testing.T) {
	s, dir := localStore(t)
	outside := filepath.Join(filepath.Dir(dir), "outside")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := newBlobs(t, s)
	_, err := b.Open(t.Context(), key("..", 1))
	requireCode(t, err, model.NotFound)
}
