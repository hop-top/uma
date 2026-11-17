package xrrtest

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"strings"

	xrr "hop.top/xrr"
)

// credentialHeaders are dropped from every recording, whatever the
// allow-list says.
var credentialHeaders = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
	"Cookie":              true,
	"Set-Cookie":          true,
}

// headerRedactor classifies header names with xrr's built-in defaults only,
// so XRR_REDACT_ALLOW can never let a credential header through.
var headerRedactor = xrr.NewRedactor(xrr.RedactConfig{})

func isCredentialHeader(name string) bool {
	return credentialHeaders[http.CanonicalHeaderKey(name)] || headerRedactor.IsSecretKey(name)
}

// bodyRedactor classifies body fields and query parameters. It honours the
// XRR_REDACT_* environment variables, like the cassette writer.
func bodyRedactor() *xrr.Redactor { return xrr.NewRedactor(xrr.RedactConfigFromEnv()) }

// ScrubSecrets replaces credential-bearing values in JSON and
// form-encoded bodies with xrr's deterministic placeholders
// ("<redacted:NAME>"), one field at a time. A string value is replaced when
// its field name looks like a credential (token, secret, password, key, ...)
// or the value matches a known credential shape (GitHub, Slack, Stripe,
// AWS, JWT, bearer). Other fields, other content types and bodies without a
// match are returned unchanged.
//
// It runs before every save because xrr's own cassette redactor replaces
// the entire body string when a credential shape appears anywhere in it.
func ScrubSecrets(contentType string, body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	r := bodyRedactor()
	switch mediaType(contentType) {
	case "application/x-www-form-urlencoded":
		return scrubForm(r, body)
	default:
		if isJSON(contentType) {
			return scrubJSON(r, body)
		}
		return body
	}
}

func mediaType(contentType string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(contentType))
	}
	return mt
}

func isJSON(contentType string) bool {
	mt := mediaType(contentType)
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

func scrubForm(r *xrr.Redactor, body []byte) []byte {
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return body
	}
	if !redactValues(r, vals) {
		return body
	}
	return []byte(vals.Encode())
}

// redactValues redacts credential-bearing values in place and reports
// whether anything changed.
func redactValues(r *xrr.Redactor, vals url.Values) bool {
	changed := false
	for k, vs := range vals {
		for i, v := range vs {
			if red, ok := r.RedactField(k, v); ok {
				vs[i] = red
				changed = true
			}
		}
	}
	return changed
}

func scrubJSON(r *xrr.Redactor, body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return body
	}
	v, changed := redactJSON(r, v, "")
	if !changed {
		return body
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return body
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// redactJSON mirrors xrr's cassette redactor: string scalars are judged by
// the key they sit under (array elements inherit the array's key);
// numbers, booleans and null are left alone.
func redactJSON(r *xrr.Redactor, v any, key string) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		changed := false
		for k, c := range t {
			nv, ch := redactJSON(r, c, k)
			if ch {
				t[k] = nv
				changed = true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, c := range t {
			nv, ch := redactJSON(r, c, key)
			if ch {
				t[i] = nv
				changed = true
			}
		}
		return t, changed
	case string:
		if red, ok := r.RedactField(key, t); ok {
			return red, true
		}
		return t, false
	default:
		return v, false
	}
}
