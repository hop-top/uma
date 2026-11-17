package twilio

import (
	"errors"
	"mime"
	"net/url"

	"github.com/twilio/twilio-go/client"
)

// SignatureHeader carries Twilio's webhook signature.
const SignatureHeader = "X-Twilio-Signature"

// Webhook verification failures. Neither error carries request content.
var (
	// ErrSignature: the signature is missing or does not match.
	ErrSignature = errors.New("twilio: webhook signature invalid")
	// ErrMalformed: the request is not one Twilio signs: a body that is
	// neither a form nor a JSON object, a form that repeats a parameter, or
	// a JSON body whose URL lacks exactly one valid bodySHA256.
	ErrMalformed = errors.New("twilio: webhook body malformed")
)

// VerifyForm checks a form-encoded Twilio webhook (VerifyJSON takes a JSON
// body, Verify either) and returns its
// parameters only when signature is valid for signedURL and body under
// authToken.
//
// signedURL is the exact URL Twilio called, from the scheme through the
// query string, as configured on the Twilio number or service; Twilio signs
// the HMAC-SHA1 of that URL followed by every POST parameter name and value
// in sorted order. The comparison is constant time (twilio-go's
// RequestValidator, which also accepts the URL with or without its default
// port). A parameter that appears twice is refused before the check: the
// validator signs one value per name, so a repeat would let unsigned data
// through.
func VerifyForm(authToken []byte, signedURL, contentType string, body []byte, signature string) (url.Values, error) {
	if mt, _, err := mime.ParseMediaType(contentType); err != nil || mt != "application/x-www-form-urlencoded" {
		return nil, ErrMalformed
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, ErrMalformed
	}
	params := make(map[string]string, len(form))
	for k, vs := range form {
		if len(vs) != 1 {
			return nil, ErrMalformed
		}
		params[k] = vs[0]
	}
	if len(authToken) == 0 || signature == "" {
		return nil, ErrSignature
	}
	v := client.NewRequestValidator(string(authToken))
	if !v.Validate(signedURL, params, signature) {
		return nil, ErrSignature
	}
	return form, nil
}
