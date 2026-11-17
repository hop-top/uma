package webhookauth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// VerifyHTTP reads the request body once, verifies the request and returns
// the body exactly as received along with the result. The body is also put
// back on r, so a handler downstream can read it again; signatures cover
// the wire bytes, so parse the returned bytes and never re-encode a parsed
// body to verify it.
//
// A body over Config.MaxBodyBytes is ErrTooLarge and no secret is resolved
// for it. The host's own http.MaxBytesReader may be tighter; its error maps
// to ErrTooLarge too. A host that cannot process a verified request should
// call Result.Release before answering with a retryable status.
func (v *Verifier) VerifyHTTP(r *http.Request) ([]byte, *Result, error) {
	body, err := v.readBody(r)
	if err != nil {
		return nil, nil, err
	}
	res, err := v.Verify(r.Context(), Request{Header: r.Header, Body: body})
	if err != nil {
		return nil, nil, err
	}
	return body, res, nil
}

func (v *Verifier) readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	limit := v.plan.MaxBodyBytes
	if r.ContentLength > limit {
		return nil, fmt.Errorf("%w: %d bytes declared, limit %d", ErrTooLarge, r.ContentLength, limit)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, fmt.Errorf("%w: host limit %d", ErrTooLarge, tooLarge.Limit)
	case err != nil:
		return nil, fmt.Errorf("%w: body unreadable", ErrMalformed)
	case int64(len(body)) > limit:
		return nil, fmt.Errorf("%w: limit %d", ErrTooLarge, limit)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}
