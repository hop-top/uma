package imap

import (
	"strconv"
	"testing"
)

// The import ledger keeps the newest trackedLimit messages of a mailbox, so
// importing years of mail neither grows it nor makes the sync scan it all.
func TestImportLedgerIsBounded(t *testing.T) {
	i := &Instance{mem: &memState{}}
	msgs := map[uint32]entry{}
	for u := uint32(1); u <= trackedLimit+50; u++ {
		msgs[u] = entry{MessageID: "<" + strconv.Itoa(int(u)) + "@x>"}
	}
	if err := i.followImported(t.Context(), "INBOX", 7, msgs); err != nil {
		t.Fatal(err)
	}
	l, err := i.loadImports(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f := l.Mailboxes["INBOX"]
	if f == nil || len(f.Messages) != trackedLimit {
		t.Fatalf("holds %v, want %d messages", f, trackedLimit)
	}
	if _, ok := f.Messages[51]; !ok {
		t.Fatal("the lowest UIDs must be the ones dropped")
	}
	if _, ok := f.Messages[50]; ok {
		t.Fatal("UID 50 kept")
	}
}
