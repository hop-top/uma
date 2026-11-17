package twilio

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/url"

	"github.com/twilio/twilio-go/client"
)

// bodyHashParam is the query parameter Twilio adds to the webhook URL of a
// JSON request: the hex SHA-256 of the raw body.
const bodyHashParam = "bodySHA256"

// Verify checks a Twilio webhook, form-encoded (VerifyForm) or JSON
// (VerifyJSON) by its content type, and returns its fields as a form. Any
// other content type is ErrMalformed.
func Verify(authToken []byte, signedURL, contentType string, body []byte, signature string) (url.Values, error) {
	if mt, _, err := mime.ParseMediaType(contentType); err == nil && mt == "application/json" {
		return VerifyJSON(authToken, signedURL, contentType, body, signature)
	}
	return VerifyForm(authToken, signedURL, contentType, body, signature)
}

// VerifyJSON checks a JSON-bodied Twilio webhook and returns the top-level
// scalar fields of its object, as text, only when the signature is valid
// for signedURL under authToken and the body is the one the URL names.
//
// Twilio signs such a request by the HMAC-SHA1 of the URL alone, with no
// POST parameters, and adds the hex SHA-256 of the raw body to that URL as
// its bodySHA256 query parameter (see "Webhooks security" in Twilio's
// documentation). The signature therefore covers the body only through the
// hash, so both checks are required: a URL without exactly one valid
// bodySHA256 is ErrMalformed, never accepted on its signature alone, which
// would authenticate any body. The signature and the hash are compared in
// constant time; a mismatch of either is ErrSignature. signedURL is as for
// VerifyForm, query included.
//
// Strings, numbers and booleans become their text; null, objects and
// arrays are left out, so a verified body reads like the form of the same
// field names. A body that is not one JSON object is ErrMalformed, but only
// after it was verified.
func VerifyJSON(authToken []byte, signedURL, contentType string, body []byte, signature string) (url.Values, error) {
	if mt, _, err := mime.ParseMediaType(contentType); err != nil || mt != "application/json" {
		return nil, ErrMalformed
	}
	u, err := url.Parse(signedURL)
	if err != nil {
		return nil, ErrMalformed
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q[bodyHashParam]) != 1 {
		return nil, ErrMalformed
	}
	want, err := hex.DecodeString(q.Get(bodyHashParam))
	if err != nil || len(want) != sha256.Size {
		return nil, ErrMalformed
	}
	if len(authToken) == 0 || signature == "" {
		return nil, ErrSignature
	}
	v := client.NewRequestValidator(string(authToken))
	if !v.Validate(signedURL, map[string]string{}, signature) {
		return nil, ErrSignature
	}
	got := sha256.Sum256(body)
	if subtle.ConstantTimeCompare(got[:], want) != 1 {
		return nil, ErrSignature
	}
	return scalarFields(body)
}

// scalarFields reads body as one JSON object and returns its scalar
// members as a form.
func scalarFields(body []byte) (url.Values, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, ErrMalformed
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrMalformed
	}
	form := make(url.Values, len(obj))
	for k, v := range obj {
		switch v := v.(type) {
		case string:
			form.Set(k, v)
		case json.Number:
			form.Set(k, v.String())
		case bool:
			if v {
				form.Set(k, "true")
			} else {
				form.Set(k, "false")
			}
		}
	}
	return form, nil
}
