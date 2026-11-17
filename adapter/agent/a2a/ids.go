package a2a

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"

	"hop.top/uma/model"
)

func itoa(n int) string { return strconv.Itoa(n) }

// randomID is a new opaque context ID: 128 random bits.
func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", model.NewError(model.Unavailable, "no randomness")
	}
	return "ctx-" + hex.EncodeToString(b[:]), nil
}
