package imessage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func openKeys(t *testing.T, dir string, c *testClock) *KeyStore {
	t.Helper()
	s, err := OpenKeyStore(dir, 24*time.Hour, c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestKeyStoreReservesOnce(t *testing.T) {
	s := openKeys(t, t.TempDir(), &testClock{time.Unix(1_760_000_000, 0)})
	if _, fresh, err := s.Reserve("k1"); err != nil || !fresh {
		t.Fatalf("first Reserve fresh=%v err=%v, want a new reservation", fresh, err)
	}
	r, fresh, err := s.Reserve("k1")
	if err != nil || fresh || r.State != StatePending {
		t.Fatalf("second Reserve = %+v fresh=%v err=%v, want the pending record", r, fresh, err)
	}
}

// What the store holds survives a restart: a settled key keeps its state
// and id, a released key stays forgotten.
func TestKeyStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	c := &testClock{time.Unix(1_760_000_000, 0)}
	s := openKeys(t, dir, c)
	for _, k := range []string{"sent", "rejected", "unknown", "released"} {
		if _, _, err := s.Reserve(k); err != nil {
			t.Fatal(err)
		}
	}
	must(t, s.Settle("sent", StateSent, "msg-1"))
	must(t, s.Settle("rejected", StateRejected, ""))
	must(t, s.Settle("unknown", StateUnknown, ""))
	must(t, s.Release("released"))
	must(t, s.Close())

	s = openKeys(t, dir, c)
	if r, ok := s.Get("sent"); !ok || r.State != StateSent || r.MessageID != "msg-1" {
		t.Errorf("sent = %+v %v", r, ok)
	}
	if r, ok := s.Get("rejected"); !ok || r.State != StateRejected {
		t.Errorf("rejected = %+v %v", r, ok)
	}
	if r, ok := s.Get("unknown"); !ok || r.State != StateUnknown {
		t.Errorf("unknown = %+v %v", r, ok)
	}
	if r, ok := s.Get("released"); ok {
		t.Errorf("released = %+v, want no record", r)
	}
}

// A key the process held pending when it stopped may have been sent: it
// comes back unknown, never free to send again.
func TestKeyStorePendingAtCrashIsUnknown(t *testing.T) {
	dir := t.TempDir()
	c := &testClock{time.Unix(1_760_000_000, 0)}
	s := openKeys(t, dir, c)
	if _, _, err := s.Reserve("k1"); err != nil {
		t.Fatal(err)
	}
	must(t, s.Close())
	s = openKeys(t, dir, c)
	r, fresh, err := s.Reserve("k1")
	if err != nil || fresh || r.State != StateUnknown {
		t.Fatalf("Reserve after restart = %+v fresh=%v err=%v, want unknown", r, fresh, err)
	}
}

func TestKeyStoreRetention(t *testing.T) {
	dir := t.TempDir()
	c := &testClock{time.Unix(1_760_000_000, 0)}
	s := openKeys(t, dir, c)
	if _, _, err := s.Reserve("old"); err != nil {
		t.Fatal(err)
	}
	must(t, s.Settle("old", StateSent, "m-old"))
	c.t = c.t.Add(12 * time.Hour)
	if _, _, err := s.Reserve("young"); err != nil {
		t.Fatal(err)
	}
	must(t, s.Settle("young", StateSent, "m-young"))
	must(t, s.Close())

	c.t = c.t.Add(13 * time.Hour) // old is 25 hours old, young 13
	s = openKeys(t, dir, c)
	if _, ok := s.Get("old"); ok {
		t.Error("a key past the retention is still held")
	}
	if _, ok := s.Get("young"); !ok {
		t.Error("a key within the retention was dropped")
	}
}

// A write the process did not finish leaves a torn last line, which is
// ignored; a damaged line before it is an error, never silently skipped.
func TestKeyStoreTornLastLine(t *testing.T) {
	dir := t.TempDir()
	c := &testClock{time.Unix(1_760_000_000, 0)}
	s := openKeys(t, dir, c)
	if _, _, err := s.Reserve("k1"); err != nil {
		t.Fatal(err)
	}
	must(t, s.Settle("k1", StateSent, "m1"))
	must(t, s.Close())
	path := filepath.Join(dir, keysFile)
	appendFile(t, path, `{"k":"k2","s":"pen`)
	s = openKeys(t, dir, c)
	if r, ok := s.Get("k1"); !ok || r.State != StateSent {
		t.Fatalf("k1 = %+v %v", r, ok)
	}
	must(t, s.Close())

	appendFile(t, path, "garbage\n"+`{"k":"k3","s":"sent","t":1760000000}`+"\n")
	if _, err := OpenKeyStore(dir, 24*time.Hour, c.now); err == nil {
		t.Fatal("a damaged line inside the log was skipped")
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	must(t, f.Close())
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
