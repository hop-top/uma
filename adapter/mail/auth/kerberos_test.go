package auth_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"hop.top/uma/adapter/mail/auth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// requireDocker skips a test when no container runtime is reachable, unless
// UMA_REQUIRE_DOCKER=1 (CI), where a missing runtime fails the test.
func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("UMA_REQUIRE_DOCKER") == "1" {
		return
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
}

// Fixed by testdata/kerberos/start.sh.
const (
	krbRealm     = "EXAMPLE.TEST"
	krbMailHost  = "mail.example.test"
	krbUser      = "alice"
	krbMailPass  = "alice-mail-pw" // Dovecot passdb (PLAIN)
	krbImageRepo = "uma-auth-kerberos-test"
)

// kerberosServer is an MIT krb5 KDC plus Dovecot (GSSAPI and PLAIN over
// implicit TLS) in one container built from testdata/kerberos.
type kerberosServer struct {
	kdc    string // host:port of the KDC (TCP)
	imaps  string // host:port of IMAPS
	roots  *x509.CertPool
	keytab []byte // alice's keytab
	ccache []byte // credential cache with a TGT for alice
}

func startKerberosServer(t *testing.T) *kerberosServer {
	t.Helper()
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	caKey, ca := mustCA(t, "uma kerberos test CA")
	leaf := mustLeaf(t, caKey, ca, krbMailHost, true)
	keyDER, err := x509.MarshalPKCS8PrivateKey(leaf.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Certificate[0]})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	ctr, err := testcontainers.Run(ctx, "",
		testcontainers.WithDockerfile(testcontainers.FromDockerfile{
			Context:   "testdata/kerberos",
			Repo:      krbImageRepo,
			Tag:       "1",
			KeepImage: true,
		}),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{Reader: bytes.NewReader(certPEM), ContainerFilePath: "/etc/dovecot/tls/cert.pem", FileMode: 0o644},
			testcontainers.ContainerFile{Reader: bytes.NewReader(keyPEM), ContainerFilePath: "/etc/dovecot/tls/key.pem", FileMode: 0o644},
		),
		testcontainers.WithExposedPorts("88/tcp", "993/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("kerberos test server ready"),
			wait.ForListeningPort("993/tcp"),
		),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start kerberos container: %v", err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		// Runs before the container is removed: KDC and Dovecot logs.
		if rc, err := ctr.Logs(context.Background()); err == nil {
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			t.Logf("container logs:\n%s", b)
		}
	})
	s := &kerberosServer{roots: x509.NewCertPool()}
	s.roots.AddCert(ca)
	s.kdc = endpoint(ctx, t, ctr, "88/tcp")
	s.imaps = endpoint(ctx, t, ctr, "993/tcp")
	s.keytab = copyOut(ctx, t, ctr, "/out/alice.keytab")
	s.ccache = copyOut(ctx, t, ctr, "/out/alice.ccache")
	return s
}

func endpoint(ctx context.Context, t *testing.T, ctr *testcontainers.DockerContainer, port string) string {
	t.Helper()
	ep, err := ctr.PortEndpoint(ctx, port, "")
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func copyOut(ctx context.Context, t *testing.T, ctr *testcontainers.DockerContainer, path string) []byte {
	t.Helper()
	rc, err := ctr.CopyFileFromContainer(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (s *kerberosServer) config(h ports.Credentials) auth.Config {
	cfg := config("imap", imapDialer(s.imaps), h)
	cfg.Request.Accept = []ports.CredentialKind{ports.KindKerberos, ports.KindPassword}
	cfg.ServerName = krbMailHost
	cfg.RootCAs = s.roots
	cfg.Kerberos = auth.KerberosConfig{Service: "imap", KDCs: []string{s.kdc}}
	return cfg
}

// TestDovecotGSSAPI authenticates against a real MIT KDC and Dovecot: GSSAPI
// from a keytab and from a credential cache, and the failure classes around
// it. Dovecot is built without NTLM (slated for removal upstream); NTLM is
// covered by the recorded-exchange tests.
func TestDovecotGSSAPI(t *testing.T) {
	s := startKerberosServer(t)
	ctx := context.Background()

	connect := func(t *testing.T, cfg auth.Config) (*auth.Session, error) {
		t.Helper()
		sess, err := auth.Connect(ctx, cfg)
		if err == nil {
			t.Cleanup(func() { _ = sess.Conn.Close() })
		}
		return sess, err
	}
	mustSession := func(t *testing.T, cfg auth.Config, mech string) *auth.Session {
		t.Helper()
		sess, err := connect(t, cfg)
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if sess.Mechanism != mech {
			t.Fatalf("mechanism = %s, want %s", sess.Mechanism, mech)
		}
		// The session is usable: an authenticated-state command succeeds.
		ic := sess.Conn.(*imapConn)
		if _, err := ic.c.List("", "%", nil).Collect(); err != nil {
			t.Fatalf("LIST after auth: %v", err)
		}
		return sess
	}
	kt := ports.Kerberos{Principal: krbUser, Realm: krbRealm, Keytab: s.keytab}

	t.Run("gssapi keytab", func(t *testing.T) {
		sess := mustSession(t, s.config(staticHost(kt)), auth.GSSAPI)
		if sess.Kind != ports.KindKerberos || !sess.ReconnectAt.IsZero() {
			t.Errorf("session = %+v", sess)
		}
	})
	t.Run("gssapi ccache", func(t *testing.T) {
		exp := time.Now().Add(time.Hour)
		cc := ports.Kerberos{Principal: krbUser, Realm: krbRealm, CCache: s.ccache, ExpiresAt: exp}
		sess := mustSession(t, s.config(staticHost(cc)), auth.GSSAPI)
		if want := exp.Add(-auth.DefaultReconnectMargin); !sess.ReconnectAt.Equal(want) {
			t.Errorf("ReconnectAt = %v, want %v", sess.ReconnectAt, want)
		}
	})
	t.Run("gssapi pointer credential", func(t *testing.T) {
		mustSession(t, s.config(staticHost(&kt)), auth.GSSAPI)
	})
	t.Run("plain password on the same server", func(t *testing.T) {
		p := ports.Password{User: krbUser, Secret: []byte(krbMailPass)}
		mustSession(t, s.config(staticHost(p)), auth.Plain)
	})
	t.Run("gssapi unknown service principal", func(t *testing.T) {
		cfg := s.config(staticHost(kt))
		cfg.Kerberos.Service = "smtp"
		_, err := connect(t, cfg)
		wantCode(t, err, model.Permanent)
	})
	t.Run("gssapi wrong keytab", func(t *testing.T) {
		other := ports.Kerberos{Principal: "bob", Realm: krbRealm, Keytab: s.keytab}
		_, err := connect(t, s.config(staticHost(other)))
		wantCode(t, err, model.Unauthorized)
	})
	t.Run("gssapi ccache for another principal", func(t *testing.T) {
		cc := ports.Kerberos{Principal: "bob", Realm: krbRealm, CCache: s.ccache}
		_, err := connect(t, s.config(staticHost(cc)))
		wantCode(t, err, model.Validation)
	})
	t.Run("gssapi kdc unreachable", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		dead := ln.Addr().String()
		_ = ln.Close()
		cfg := s.config(staticHost(kt))
		cfg.Kerberos.KDCs = []string{dead}
		_, err = connect(t, cfg)
		wantCode(t, err, model.Unavailable)
	})
	t.Run("gssapi without service name", func(t *testing.T) {
		cfg := s.config(staticHost(kt))
		cfg.Kerberos.Service = ""
		_, err := connect(t, cfg)
		wantCode(t, err, model.Validation)
	})
}
