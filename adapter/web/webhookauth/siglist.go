package webhookauth

import (
	"fmt"
	"strings"
)

// ListSeparator is the delimiter of a signature list.
type ListSeparator string

const (
	// SeparatorComma delimits entries with "," and nothing else, the shape
	// of a rotation header such as "v1=a,v1=b". Whitespace around a comma
	// belongs to the entry, which then does not decode.
	SeparatorComma ListSeparator = ","
	// SeparatorSpace delimits entries with one space, the shape of
	// "v1,a v1,b". Two spaces in a row are an empty entry.
	SeparatorSpace ListSeparator = " "
)

// SignatureList opts in to a signature header that carries several
// signatures, which a sender emits while it rotates its secret: one per
// secret in use. The zero value keeps the header a single signature.
//
// The request verifies when at least one entry does. Every entry is parsed
// under the same Prefix, PrefixOptional and Encoding as a lone signature,
// and all of them are checked before any is compared, so one entry that
// does not decode (wrong prefix, wrong alphabet, wrong size, empty, or past
// Max) makes the whole header ErrMalformed: nothing is skipped, ignored or
// normalized, and a request cannot hide a malformed entry behind a valid
// one. Every entry is compared against every secret, in constant time and
// without stopping at a match.
//
// Signature schemes only. See Replay for how a matched signature identifies
// a delivery.
type SignatureList struct {
	// Separator turns the option on.
	Separator ListSeparator

	// Max bounds the entries of one header, counted before any is decoded,
	// since each costs a comparison per secret. Default 8, at most 32.
	Max int
}

const (
	defaultListMax = 8
	maxListMax     = 32
)

func (l SignatureList) on() bool { return l.Separator != "" }

func (l SignatureList) validate() (max int, err error) {
	switch l.Separator {
	case "":
		if l.Max != 0 {
			return 0, cfgErr("signature list maximum needs a separator")
		}
		return 0, nil
	case SeparatorComma, SeparatorSpace:
	default:
		return 0, cfgErr("unknown signature list separator %q", l.Separator)
	}
	if l.Max < 0 || l.Max > maxListMax {
		return 0, cfgErr("signature list maximum %d is outside 0..%d", l.Max, maxListMax)
	}
	if l.Max == 0 {
		return defaultListMax, nil
	}
	return l.Max, nil
}

// decodeSignatures decodes the header value into the signatures it
// carries: one when the list option is off.
func (v *Verifier) decodeSignatures(raw string) ([][]byte, error) {
	if !v.plan.SignatureList.on() {
		sig, err := v.decodeSignature(raw)
		if err != nil {
			return nil, err
		}
		return [][]byte{sig}, nil
	}
	sep := string(v.plan.SignatureList.Separator)
	if n := strings.Count(raw, sep) + 1; n > v.plan.listMax {
		return nil, fmt.Errorf("%w: %s carries %d signatures, more than %d", ErrMalformed, v.plan.header, n, v.plan.listMax)
	}
	parts := strings.Split(raw, sep)
	sigs := make([][]byte, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("%w: %s has an empty entry", ErrMalformed, v.plan.header)
		}
		sig, err := v.decodeSignature(part)
		if err != nil {
			return nil, err
		}
		sigs[i] = sig
	}
	return sigs, nil
}
