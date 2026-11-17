package secret

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	kitsecret "hop.top/kit/go/storage/secret"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// fakeStore is a kit secret.Store over a map. Every Get returns a fresh
// copy and remembers it, so tests can check the caller zeroed it.
type fakeStore struct {
	mu       sync.Mutex
	values   map[string][]byte
	err      error
	nilValue bool
	gets     []string
	returned [][]byte
}

func newStore() *fakeStore { return &fakeStore{values: map[string][]byte{}} }

func (s *fakeStore) put(key string, v []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = append([]byte(nil), v...)
}

func (s *fakeStore) Get(_ context.Context, key string) (*kitsecret.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets = append(s.gets, key)
	if s.err != nil {
		return nil, s.err
	}
	if s.nilValue {
		return nil, nil
	}
	v, ok := s.values[key]
	if !ok {
		return nil, kitsecret.ErrNotFound
	}
	cp := append([]byte(nil), v...)
	s.returned = append(s.returned, cp)
	return &kitsecret.Secret{Key: key, Value: cp}, nil
}

func (s *fakeStore) List(context.Context, string) ([]string, error) {
	return nil, kitsecret.ErrNotSupported
}

func (s *fakeStore) Exists(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.values[key]
	return ok, nil
}

// envelope marshals fields as a stored credential value.
func envelope(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// request is a channel request for tenant t1, reference "acct".
func request(accept ...ports.CredentialKind) ports.CredentialRequest {
	return ports.CredentialRequest{
		Channel:   model.ChannelRef{Tenant: "t1", ID: "0192f7a0-0000-7000-8000-000000000001"},
		Reference: "acct",
		Purpose:   "imap",
		Accept:    accept,
	}
}

const defaultKey = "uma/t1/acct"

var allKinds = []ports.CredentialKind{
	ports.KindSecret, ports.KindPassword, ports.KindOAuth2, ports.KindClientCert, ports.KindKerberos,
}

func mustNew(t *testing.T, store kitsecret.Store, opts ...Option) ports.Credentials {
	t.Helper()
	c, err := New(store, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wantCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	var me *model.Error
	if !errors.As(err, &me) {
		t.Fatalf("error %v (%T) is not a *model.Error", err, err)
	}
	if me.Code != code {
		t.Fatalf("code = %q, want %q (%v)", me.Code, code, err)
	}
}

// selfSigned returns a PEM certificate and PKCS#8 key with the given
// NotAfter.
func selfSigned(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "client"},
		NotBefore:    notAfter.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})
}
