package secret

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	kitsecret "hop.top/kit/go/storage/secret"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

var passwordValue = []byte(`{"v":1,"kind":"password","user":"alice","password":"hunter2"}`)

func TestNewRejectsMissingParts(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) succeeded")
	}
	if _, err := New(newStore(), WithKeyFunc(nil)); err == nil {
		t.Fatal("WithKeyFunc(nil) succeeded")
	}
}

func TestDefaultKey(t *testing.T) {
	tests := []struct {
		tenant, ref, want string
	}{
		{"t1", "acct", "uma/t1/acct"},
		{"acme/eu", "mail/support", "uma/acme%2Feu/mail%2Fsupport"},
		{"t1", "a b?c", "uma/t1/a%20b%3Fc"},
		{"t1", "..x", "uma/t1/..x"},
	}
	for _, tt := range tests {
		req := request(ports.KindPassword)
		req.Channel.Tenant = model.TenantID(tt.tenant)
		req.Reference = tt.ref
		got, err := DefaultKey(req)
		if err != nil || got != tt.want {
			t.Errorf("DefaultKey(%q, %q) = %q, %v; want %q", tt.tenant, tt.ref, got, err, tt.want)
		}
	}
}

func TestDefaultKeyRejects(t *testing.T) {
	for _, tt := range []struct{ tenant, ref string }{
		{"", "acct"}, {"t1", ""}, {"t1", "."}, {"t1", ".."}, {".", "acct"}, {"..", "acct"},
	} {
		store := newStore()
		req := request(ports.KindPassword)
		req.Channel.Tenant = model.TenantID(tt.tenant)
		req.Reference = tt.ref
		_, err := mustNew(t, store).Resolve(context.Background(), req)
		wantCode(t, err, model.Validation)
		if len(store.gets) != 0 {
			t.Errorf("(%q, %q): store read %q", tt.tenant, tt.ref, store.gets)
		}
	}
}

func TestDefaultKeyIgnoresPurpose(t *testing.T) {
	store := newStore()
	store.put(defaultKey, passwordValue)
	c := mustNew(t, store)
	for _, purpose := range []string{"imap", "smtp.submission"} {
		req := request(ports.KindPassword)
		req.Purpose = purpose
		if _, err := c.Resolve(context.Background(), req); err != nil {
			t.Fatalf("%s: %v", purpose, err)
		}
	}
	if !slices.Equal(store.gets, []string{defaultKey, defaultKey}) {
		t.Fatalf("keys read = %q", store.gets)
	}
}

func TestWithKeyFunc(t *testing.T) {
	store := newStore()
	store.put("mail/acct/smtp.submission", passwordValue)
	var seen ports.CredentialRequest
	c := mustNew(t, store, WithKeyFunc(func(r ports.CredentialRequest) (string, error) {
		seen = r
		return "mail/" + r.Reference + "/" + r.Purpose, nil
	}))
	req := request(ports.KindPassword)
	req.Purpose = "smtp.submission"
	req.Fresh = true
	if _, err := c.Resolve(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !seen.Fresh || seen.Purpose != "smtp.submission" || seen.Channel != req.Channel {
		t.Fatalf("key func saw %+v", seen)
	}
}

func TestKeyFuncErrors(t *testing.T) {
	denied := model.NewError(model.Unauthorized, "tenant may not use this reference")
	tests := []struct {
		name string
		fn   KeyFunc
		code model.ErrorCode
	}{
		{"model error passes through", func(ports.CredentialRequest) (string, error) { return "", denied }, model.Unauthorized},
		{"other error is validation", func(ports.CredentialRequest) (string, error) { return "", errors.New("no") }, model.Validation},
		{"empty key", func(ports.CredentialRequest) (string, error) { return "", nil }, model.Validation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore()
			_, err := mustNew(t, store, WithKeyFunc(tt.fn)).Resolve(context.Background(), request(ports.KindPassword))
			wantCode(t, err, tt.code)
			if len(store.gets) != 0 {
				t.Fatalf("store read %q", store.gets)
			}
		})
	}
}

func TestAcceptMismatchIsUnsupported(t *testing.T) {
	store := newStore()
	store.put(defaultKey, passwordValue)
	c := mustNew(t, store)

	_, err := c.Resolve(context.Background(), request(ports.KindOAuth2, ports.KindClientCert))
	wantCode(t, err, model.Unsupported)

	got, err := c.Resolve(context.Background(), request(ports.KindOAuth2, ports.KindPassword))
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind() != ports.KindPassword {
		t.Fatalf("kind = %s", got.Kind())
	}
}

func TestEmptyAcceptIsValidation(t *testing.T) {
	store := newStore()
	store.put(defaultKey, passwordValue)
	_, err := mustNew(t, store).Resolve(context.Background(), request())
	wantCode(t, err, model.Validation)
	if len(store.gets) != 0 {
		t.Fatalf("store read %q", store.gets)
	}
}

// TestEveryResolveReadsStore pins the no-cache contract: each Resolve,
// Fresh or not, reads the store, so a rotated value is seen at once.
func TestEveryResolveReadsStore(t *testing.T) {
	store := newStore()
	store.put(defaultKey, []byte(`{"v":1,"kind":"oauth2","access_token":"old"}`))
	c := mustNew(t, store)
	req := request(ports.KindOAuth2)

	first, err := c.Resolve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	store.put(defaultKey, []byte(`{"v":1,"kind":"oauth2","access_token":"new"}`))
	second, err := c.Resolve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	store.put(defaultKey, []byte(`{"v":1,"kind":"oauth2","access_token":"newer"}`))
	req.Fresh = true
	third, err := c.Resolve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{first.(ports.OAuth2).AccessToken, second.(ports.OAuth2).AccessToken, third.(ports.OAuth2).AccessToken}
	if !slices.Equal(got, []string{"old", "new", "newer"}) {
		t.Fatalf("tokens = %q", got)
	}
	if len(store.gets) != 3 {
		t.Fatalf("store reads = %d, want 3", len(store.gets))
	}
}

func TestStoreErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code model.ErrorCode
	}{
		{"not found", kitsecret.ErrNotFound, model.NotFound},
		{"wrapped not found", fmt.Errorf("keyring: %w", kitsecret.ErrNotFound), model.NotFound},
		{"not supported", fmt.Errorf("ghsecrets: %w", kitsecret.ErrNotSupported), model.Unsupported},
		{"other", errors.New("connection refused"), model.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore()
			store.err = tt.err
			_, err := mustNew(t, store).Resolve(context.Background(), request(allKinds...))
			wantCode(t, err, tt.code)
		})
	}

	t.Run("missing key", func(t *testing.T) {
		_, err := mustNew(t, newStore()).Resolve(context.Background(), request(allKinds...))
		wantCode(t, err, model.NotFound)
	})
	t.Run("nil secret", func(t *testing.T) {
		store := newStore()
		store.nilValue = true
		_, err := mustNew(t, store).Resolve(context.Background(), request(allKinds...))
		wantCode(t, err, model.Unavailable)
	})
}

func TestContextErrorsStayStandard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := newStore()
	store.put(defaultKey, passwordValue)
	_, err := mustNew(t, store).Resolve(ctx, request(ports.KindPassword))
	if !errors.Is(err, context.Canceled) || model.CodeOf(err) != "" {
		t.Fatalf("err = %v", err)
	}
	if len(store.gets) != 0 {
		t.Fatalf("store read %q after cancellation", store.gets)
	}

	store.err = fmt.Errorf("openbao: %w", context.DeadlineExceeded)
	_, err = mustNew(t, store).Resolve(context.Background(), request(ports.KindPassword))
	if !errors.Is(err, context.DeadlineExceeded) || model.CodeOf(err) != "" {
		t.Fatalf("err = %v", err)
	}
}

// TestStoreValueZeroed checks the bytes kit returned are cleared once
// decoded, on success and on every failure after the read.
func TestStoreValueZeroed(t *testing.T) {
	cases := map[string]struct {
		value  []byte
		accept []ports.CredentialKind
	}{
		"decoded":   {passwordValue, []ports.CredentialKind{ports.KindPassword}},
		"rejected":  {passwordValue, []ports.CredentialKind{ports.KindOAuth2}},
		"malformed": {[]byte(`{"v":1,"kind":"password","user":"alice","password":"hunter2","x":1}`), allKinds},
		"garbage":   {[]byte(`hunter2`), allKinds},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := newStore()
			store.put(defaultKey, tc.value)
			_, _ = mustNew(t, store).Resolve(context.Background(), request(tc.accept...))
			if len(store.returned) != 1 {
				t.Fatalf("store returned %d values", len(store.returned))
			}
			if v := store.returned[0]; !bytes.Equal(v, make([]byte, len(v))) {
				t.Fatalf("store value not zeroed: %q", v)
			}
		})
	}
}

// TestZeroCredential checks the cleanup Resolve applies to a decoded
// credential it does not return (kind outside Accept).
func TestZeroCredential(t *testing.T) {
	values := [][]byte{
		passwordValue,
		[]byte(`{"v":1,"kind":"secret","value":"s3"}`),
		[]byte(`{"v":1,"kind":"kerberos","principal":"p","realm":"R","keytab_base64":"AQID"}`),
		[]byte(`{"v":1,"kind":"kerberos","principal":"p","realm":"R","ccache_base64":"AQID"}`),
	}
	for _, v := range values {
		c, err := decode(bytes.Clone(v))
		if err != nil {
			t.Fatal(err)
		}
		var b []byte
		switch c := c.(type) {
		case ports.Password:
			b = c.Secret
		case ports.Secret:
			b = c.Bytes
		case ports.Kerberos:
			b = c.Keytab
			if b == nil {
				b = c.CCache
			}
		}
		zeroCredential(c)
		if len(b) == 0 || !bytes.Equal(b, make([]byte, len(b))) {
			t.Fatalf("%s bytes after zeroCredential = %q", c.Kind(), b)
		}
	}
}
