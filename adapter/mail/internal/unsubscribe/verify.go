package unsubscribe

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// Header names RFC 8058 requires a DKIM signature to cover.
const (
	HeaderUnsubscribe = "List-Unsubscribe"
	HeaderPost        = "List-Unsubscribe-Post"
)

const (
	maxSignatures = 8
	// dkimTimeout bounds the key lookups of one verification.
	dkimTimeout = 10 * time.Second
)

// Resolver looks up TXT records, as net.Resolver does.
type Resolver func(ctx context.Context, name string) ([]string, error)

// SystemResolver is the process resolver.
func SystemResolver(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, name)
}

// CheckDKIM verifies the DKIM signatures of the raw message and reports
// whether one is valid, covers both List-Unsubscribe and
// List-Unsubscribe-Post, and was made by the sender's organization (its
// signing domain has the organizational domain fromOrg): RFC 8058 section 4
// has a receiver not offer one-click for a message whose signature does not
// cover them, and a signature by an unrelated domain says nothing about the
// header's author. It returns nil when it holds. A key that cannot be
// fetched is a refusal like any other: the effect never posts on evidence it
// could not check. temporary reports that the refusal came from a DNS
// failure, which a later attempt may not meet; the caller decides whether
// that is retried.
func CheckDKIM(ctx context.Context, raw []byte, fromOrg string, lookup Resolver) (ref *Refusal, temporary bool) {
	if lookup == nil {
		lookup = SystemResolver
	}
	ctx, cancel := context.WithTimeout(ctx, dkimTimeout)
	defer cancel()
	var dnsFailed bool
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			txt, err := lookup(ctx, name)
			var dnsErr *net.DNSError
			if err != nil && (!errors.As(err, &dnsErr) || dnsErr.Temporary() || dnsErr.IsTimeout) {
				dnsFailed = true
			}
			return txt, err
		},
		MaxVerifications: maxSignatures,
	})
	if err != nil && !errors.Is(err, dkim.ErrTooManySignatures) {
		return refuse("the message has no DKIM signature that can be verified"), false
	}
	for _, v := range verifs {
		if v.Err != nil || !covers(v.HeaderKeys, HeaderUnsubscribe) || !covers(v.HeaderKeys, HeaderPost) {
			continue
		}
		if d, r := OrgDomain(v.Domain); r == nil && d == fromOrg {
			return nil, false
		}
	}
	if dnsFailed && ctx.Err() == nil {
		return refuse("the DKIM signature could not be checked: key lookup failed"), true
	}
	return refuse("the message has no valid DKIM signature by the sender's organization covering List-Unsubscribe and List-Unsubscribe-Post"), false
}

func covers(keys []string, name string) bool {
	for _, k := range keys {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}
