package webhookauth

import (
	"errors"
	"net/http"
)

// Reject answers a failed verification: the status Status picks, no body
// and, for SchemeBearer, the WWW-Authenticate challenge RFC 6750 3 asks
// for. A request without credentials gets the bare challenge; a bad one
// names the error class.
func (v *Verifier) Reject(w http.ResponseWriter, err error) {
	status := Status(err)
	if v.plan.Scheme == SchemeBearer {
		switch {
		case status == http.StatusUnauthorized && errors.Is(err, ErrMissing):
			w.Header().Set("WWW-Authenticate", "Bearer")
		case status == http.StatusUnauthorized:
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		case status == http.StatusBadRequest:
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_request"`)
		}
	}
	w.WriteHeader(status)
}
