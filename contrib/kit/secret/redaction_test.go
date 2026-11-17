package secret

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	kitsecret "hop.top/kit/go/storage/secret"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// canary is planted in every position a stored value can carry text. It
// must never appear in an error or a formatted credential.
const canary = "CANARY-5e1f9c"

func leaks(v any) []string {
	var out []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		s := fmt.Sprintf(verb, v)
		if strings.Contains(s, canary) || strings.Contains(s, fmt.Sprintf("%x", canary)) {
			out = append(out, verb+" => "+s)
		}
	}
	return out
}

func checkErr(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error", name)
		return
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("%s: Error() leaks canary: %s", name, err.Error())
	}
	for _, l := range leaks(err) {
		t.Errorf("%s: %s", name, l)
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.Contains(e.Error(), canary) {
			t.Errorf("%s: wrapped error leaks canary: %s", name, e.Error())
		}
	}
}

func TestErrorsNeverCarrySecretMaterial(t *testing.T) {
	c := canary
	b64 := base64.StdEncoding.EncodeToString([]byte(c))
	values := map[string]string{
		"garbage":            c,
		"truncated":          `{"v":1,"kind":"password","user":"u","password":"` + c,
		"version":            `{"v":"` + c + `","kind":"secret","value":"s"}`,
		"version number":     `{"v":` + strings.Repeat("9", 30) + `,"kind":"secret","value":"` + c + `"}`,
		"kind":               `{"v":1,"kind":"` + c + `","value":"` + c + `"}`,
		"unknown field name": `{"v":1,"kind":"secret","value":"s","` + c + `":"x"}`,
		"unknown field":      `{"v":1,"kind":"secret","value":"s","extra":"` + c + `"}`,
		"other kind field":   `{"v":1,"kind":"password","user":"u","password":"` + c + `","access_token":"` + c + `"}`,
		"both forms":         `{"v":1,"kind":"secret","value":"` + c + `","value_base64":"` + b64 + `"}`,
		"bad base64":         `{"v":1,"kind":"secret","value_base64":"` + c + `!"}`,
		"bad expiry":         `{"v":1,"kind":"oauth2","access_token":"` + c + `","expires_at":"` + c + `"}`,
		"user wrong type":    `{"v":1,"kind":"password","user":["` + c + `"],"password":"` + c + `"}`,
		"password number":    `{"v":1,"kind":"password","user":"` + c + `","password":12}`,
		"bad pem":            `{"v":1,"kind":"client_cert","certificate_pem":"` + c + `","private_key_pem":"` + c + `"}`,
		"kerberos both":      `{"v":1,"kind":"kerberos","principal":"` + c + `","realm":"` + c + `","keytab_base64":"` + b64 + `","ccache_base64":"` + b64 + `"}`,
		"kerberos realm":     `{"v":1,"kind":"kerberos","principal":"` + c + `","keytab_base64":"` + b64 + `"}`,
		"invalid utf8 key":   "{\"v\":1,\"kind\":\"secret\",\"value\":\"" + c + "\",\"\xff" + c + "\":1}",
	}
	for name, v := range values {
		_, err := resolveStored(t, []byte(v))
		checkErr(t, name, err)
	}

	// A decoded credential outside Accept: the error names the kind only.
	_, err := resolveStored(t, []byte(`{"v":1,"kind":"password","user":"`+c+`","password":"`+c+`"}`), ports.KindOAuth2)
	checkErr(t, "not accepted", err)

	// Store errors carrying the canary (a backend echoing a key or value).
	for name, storeErr := range map[string]error{
		"store error":     errors.New("backend: " + c),
		"store not found": fmt.Errorf("%s: %w", c, kitsecret.ErrNotFound),
	} {
		store := newStore()
		store.err = storeErr
		_, err := mustNew(t, store).Resolve(context.Background(), request(allKinds...))
		checkErr(t, name, err)
	}

	// Key function errors carrying the canary.
	store := newStore()
	_, err = mustNew(t, store, WithKeyFunc(func(ports.CredentialRequest) (string, error) {
		return "", errors.New(c)
	})).Resolve(context.Background(), request(allKinds...))
	checkErr(t, "key func", err)
}

func TestReferenceNeverInErrors(t *testing.T) {
	req := request(allKinds...)
	req.Reference = canary
	_, err := mustNew(t, newStore()).Resolve(context.Background(), req)
	wantCode(t, err, model.NotFound)
	checkErr(t, "not found", err)
}

func TestDecodedCredentialsRedact(t *testing.T) {
	c := canary
	for _, v := range []string{
		`{"v":1,"kind":"secret","value":"` + c + `"}`,
		`{"v":1,"kind":"password","user":"u","password":"` + c + `"}`,
		`{"v":1,"kind":"oauth2","access_token":"` + c + `"}`,
	} {
		cred, err := resolveStored(t, []byte(v))
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range leaks(cred) {
			t.Errorf("%s: %s", cred.Kind(), l)
		}
	}
}
