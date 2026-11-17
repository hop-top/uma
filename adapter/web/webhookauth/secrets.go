package webhookauth

import (
	"context"
	"fmt"

	"hop.top/uma/ports"
)

// SecretSource yields the key material a Verifier checks requests against:
// the shared token or HMAC key, or the Ed25519 public key. It is called for
// every verification and its result is not kept, so a rotated credential
// takes effect at once. Returning more than one key accepts any of them,
// which is how a rotation overlaps old and new.
//
// Secrets never come from configuration: Config holds no secret field. The
// host supplies a source, normally FromCredentials.
type SecretSource interface {
	Secrets(ctx context.Context) ([][]byte, error)
}

// SecretFunc adapts a function to a SecretSource.
type SecretFunc func(ctx context.Context) ([][]byte, error)

// Secrets calls f.
func (f SecretFunc) Secrets(ctx context.Context) ([][]byte, error) { return f(ctx) }

// FromCredentials returns a SecretSource resolving each request through the
// host's credential port on every call. Each request names the channel,
// reference and purpose of one key; pass the current and the previous
// reference to overlap a rotation. A request with no Accept list accepts a
// ports.Secret (its Bytes) or a ports.Password (its Secret).
func FromCredentials(c ports.Credentials, reqs ...ports.CredentialRequest) SecretSource {
	return SecretFunc(func(ctx context.Context) ([][]byte, error) {
		out := make([][]byte, 0, len(reqs))
		for _, req := range reqs {
			if len(req.Accept) == 0 {
				req.Accept = []ports.CredentialKind{ports.KindSecret, ports.KindPassword}
			}
			cred, err := c.Resolve(ctx, req)
			if err != nil {
				return nil, err
			}
			switch cred := cred.(type) {
			case ports.Secret:
				out = append(out, cred.Bytes)
			case ports.Password:
				out = append(out, cred.Secret)
			default:
				return nil, fmt.Errorf("%w: credential kind %q", ErrSecret, kindOf(cred))
			}
		}
		return out, nil
	})
}

func kindOf(c ports.Credential) ports.CredentialKind {
	if c == nil {
		return ""
	}
	return c.Kind()
}
