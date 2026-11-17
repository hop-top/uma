package webhookauth

import (
	"errors"
	"net/http"
)

// Verification failures. Every error Verify returns wraps exactly one of
// them, so callers branch with errors.Is. None carries request content, a
// credential or a secret; the wrapped text names only configured headers.
var (
	// ErrMissing: the request carries no credential for the scheme.
	ErrMissing = errors.New("webhookauth: credential missing")
	// ErrInvalid: a credential is present and does not match, or its
	// signature does not verify.
	ErrInvalid = errors.New("webhookauth: credential invalid")
	// ErrMalformed: a header is repeated, or a credential, signature or
	// timestamp cannot be decoded. The request is refused before anything
	// is compared.
	ErrMalformed = errors.New("webhookauth: request malformed")
	// ErrStale: the timestamp is outside the tolerance, in the past or in
	// the future.
	ErrStale = errors.New("webhookauth: timestamp outside tolerance")
	// ErrReplay: the delivery was already accepted within the replay
	// window.
	ErrReplay = errors.New("webhookauth: replayed delivery")
	// ErrTooLarge: the body exceeds the configured limit.
	ErrTooLarge = errors.New("webhookauth: body too large")
	// ErrSecret: the host resolved no usable secret: none, an empty one,
	// one shorter than the minimum, or one that does not decode. The
	// request is refused whatever it carries.
	ErrSecret = errors.New("webhookauth: secret unusable")
	// ErrUnavailable: the host's credential port or replay store failed.
	// The failure is the host's, so the sender may retry.
	ErrUnavailable = errors.New("webhookauth: dependency unavailable")
	// ErrConfig: New refused the configuration.
	ErrConfig = errors.New("webhookauth: invalid configuration")
)

// Status maps a Verify error to the HTTP status a receiver answers with:
// 401 for a missing or invalid credential or a stale timestamp, 400 for a malformed request,
// 409 for a replayed delivery, 413 for a large body, 503 when the host cannot resolve a secret or reach
// its replay store, and 500 for anything else. A nil error is 200.
func Status(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrMissing), errors.Is(err, ErrInvalid), errors.Is(err, ErrStale):
		return http.StatusUnauthorized
	case errors.Is(err, ErrReplay):
		return http.StatusConflict
	case errors.Is(err, ErrMalformed):
		return http.StatusBadRequest
	case errors.Is(err, ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrSecret), errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}
