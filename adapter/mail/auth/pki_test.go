package auth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// pki is a throwaway CA with one server and one client certificate.
type pki struct {
	roots      *x509.CertPool
	server     tls.Certificate
	client     tls.Certificate
	untrusted  tls.Certificate // server cert from an unknown CA
	clientName string
}

var (
	pkiOnce sync.Once
	testPKI pki
)

func testCerts(t *testing.T) pki {
	t.Helper()
	pkiOnce.Do(func() {
		caKey, caCert := mustCA(t, "uma test CA")
		testPKI.roots = x509.NewCertPool()
		testPKI.roots.AddCert(caCert)
		testPKI.server = mustLeaf(t, caKey, caCert, "localhost", true)
		testPKI.clientName = "alice@example.com"
		testPKI.client = mustLeaf(t, caKey, caCert, testPKI.clientName, false)
		otherKey, otherCert := mustCA(t, "other CA")
		testPKI.untrusted = mustLeaf(t, otherKey, otherCert, "localhost", true)
	})
	return testPKI
}

func mustCA(t *testing.T, name string) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert
}

func mustLeaf(t *testing.T, caKey *ecdsa.PrivateKey, ca *x509.Certificate, name string, server bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{name}
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// serverTLS is the test servers' TLS configuration: client certificates are
// verified when presented (SASL EXTERNAL) but not required.
func serverTLS(t *testing.T, cert tls.Certificate) *tls.Config {
	p := testCerts(t)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    p.roots,
		MinVersion:   tls.VersionTLS12,
	}
}
