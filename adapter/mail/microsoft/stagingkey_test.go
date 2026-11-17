package microsoft

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// A key is "mail:" and the SHA-256 of the destination, the message and the
// attachment, NUL-separated. Changing it restages every attachment already
// staged.
func TestStagingKeyIsPinned(t *testing.T) {
	sum := sha256.Sum256([]byte("support\x00msg-1\x00att-1"))
	want := "mail:" + hex.EncodeToString(sum[:])
	if got := stagingKey("support", "msg-1", "att-1"); got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	if stagingKey("support", "msg-1", "att-2") == stagingKey("support", "msg-1", "att-1") ||
		stagingKey("sales", "msg-1", "att-1") == stagingKey("support", "msg-1", "att-1") {
		t.Error("keys collide across attachments or destinations")
	}
}
