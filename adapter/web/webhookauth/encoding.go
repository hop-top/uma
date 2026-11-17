package webhookauth

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Encoding values. Signatures use the text encodings; keys may also be raw.
const (
	// EncodingHex is hexadecimal, upper or lower case. It is the default
	// for signatures.
	EncodingHex Encoding = "hex"
	// EncodingBase64 is standard base64 with padding (RFC 4648 4).
	EncodingBase64 Encoding = "base64"
	// EncodingBase64URL is URL-safe base64 with padding (RFC 4648 5).
	EncodingBase64URL Encoding = "base64url"
	// EncodingBase64Raw is standard base64 without padding.
	EncodingBase64Raw Encoding = "base64raw"
	// EncodingBase64URLRaw is URL-safe base64 without padding.
	EncodingBase64URLRaw Encoding = "base64urlraw"
)

func (e Encoding) text() bool {
	switch e {
	case EncodingHex, EncodingBase64, EncodingBase64URL, EncodingBase64Raw, EncodingBase64URLRaw:
		return true
	}
	return false
}

// decode returns the bytes s encodes. Decoding is strict: the exact
// alphabet and padding of the encoding, no embedded line breaks, no
// non-zero trailing bits (RFC 4648 3.5). Nothing falls back to another
// encoding.
func (e Encoding) decode(s string) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, fmt.Errorf("line break in %s value", e)
	}
	switch e {
	case EncodingHex:
		return hex.DecodeString(s)
	case EncodingBase64:
		return base64.StdEncoding.Strict().DecodeString(s)
	case EncodingBase64URL:
		return base64.URLEncoding.Strict().DecodeString(s)
	case EncodingBase64Raw:
		return base64.RawStdEncoding.Strict().DecodeString(s)
	case EncodingBase64URLRaw:
		return base64.RawURLEncoding.Strict().DecodeString(s)
	}
	return nil, fmt.Errorf("unknown encoding %q", e)
}

// decodeKey decodes secret bytes the host stored under e. Text encodings
// ignore the whitespace a secret file usually ends with; raw keys are the
// bytes exactly.
func (e Encoding) decodeKey(b []byte) ([]byte, error) {
	if e == EncodingRaw {
		return b, nil
	}
	return e.decode(strings.TrimSpace(string(b)))
}
