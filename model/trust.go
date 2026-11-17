package model

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Trust counters. A host counts how its reviewers decide the operations a
// plugin proposed, under an opaque key it computes (HeldBlock.TrustKey);
// uma stores the counts and never reads policy into them. Counters are
// scoped to one channel (the channel of the decided operation's inbox) and
// kept in uma's record storage. Whether a count means auto-approve is the
// host's business.

// Trust key, label and batch bounds.
const (
	// MaxTrustLabels, MaxTrustLabelBytes bound the optional listing labels:
	// at most that many, each name and value at most that many bytes.
	MaxTrustLabels     = 8
	MaxTrustLabelBytes = 64
	// MaxTrustBatchBytes bounds a batch ID.
	MaxTrustBatchBytes = 128
)

// ValidateTrustKey requires 1 to MaxTrustKeyBytes bytes of UTF-8 without NUL.
func ValidateTrustKey(key string) error { return validateToken("trust key", key, MaxTrustKeyBytes) }

// ValidateTrustBatch requires 1 to MaxTrustBatchBytes bytes of UTF-8
// without NUL.
func ValidateTrustBatch(batch string) error {
	return validateToken("trust batch", batch, MaxTrustBatchBytes)
}

// ValidateTrustLabels bounds the labels of a trust key.
func ValidateTrustLabels(labels map[string]string) error {
	if len(labels) > MaxTrustLabels {
		return NewError(Validation, "trust labels: too many")
	}
	for k, v := range labels {
		if err := validateToken("trust label name", k, MaxTrustLabelBytes); err != nil {
			return err
		}
		if len(v) > MaxTrustLabelBytes || !utf8.ValidString(v) || containsNUL(v) {
			return NewError(Validation, "trust label value: invalid")
		}
	}
	return nil
}

func validateToken(what, s string, max int) error {
	if s == "" || len(s) > max || !utf8.ValidString(s) || containsNUL(s) {
		return NewError(Validation, what+": must be 1 to "+strconv.Itoa(max)+" bytes of UTF-8 without NUL")
	}
	return nil
}

func containsNUL(s string) bool { return strings.IndexByte(s, 0) >= 0 }
