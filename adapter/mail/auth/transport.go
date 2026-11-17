package auth

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/netip"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// DefaultReconnectMargin is how long before credential expiry a session is
// replaced when Policy.ReconnectMargin is zero.
const DefaultReconnectMargin = 5 * time.Minute

// Policy is the per-channel authentication policy.
type Policy struct {
	// AllowPlaintextLoopback lets a plaintext connection authenticate when
	// the peer address is loopback (127.0.0.0/8, ::1), e.g. a local mail
	// bridge. Default off; never applies to other addresses.
	AllowPlaintextLoopback bool
	// ReconnectMargin is how long before credential expiry the session must
	// be replaced. Zero means DefaultReconnectMargin; negative is invalid.
	ReconnectMargin time.Duration
}

func (p Policy) margin() time.Duration {
	if p.ReconnectMargin == 0 {
		return DefaultReconnectMargin
	}
	return p.ReconnectMargin
}

// ReconnectAt returns when a session authenticated with cred must be
// replaced: expiry minus the policy margin. It is zero when cred has no known
// expiry.
func (p Policy) ReconnectAt(cred ports.Credential) time.Time {
	if cred == nil || cred.Expiry().IsZero() {
		return time.Time{}
	}
	return cred.Expiry().Add(-p.margin())
}

// TLSConfig returns the client TLS configuration for serverName: TLS 1.2 or
// later, certificates verified against roots (nil: system roots) and, for a
// ClientCert credential, the client certificate. It never skips
// verification.
func TLSConfig(serverName string, roots *x509.CertPool, cred ports.Credential) *tls.Config {
	cfg := &tls.Config{
		ServerName: serverName,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	}
	if c, ok := concrete(cred).(ports.ClientCert); ok {
		cfg.Certificates = []tls.Certificate{c.Certificate}
	}
	return cfg
}

// CheckTransport enforces TLS before AUTH: conn must have completed a TLS
// handshake, or be a plaintext loopback connection the policy opted into.
func CheckTransport(conn Conn, p Policy) error {
	if state, ok := conn.TLS(); ok && state.HandshakeComplete {
		return nil
	}
	if p.AllowPlaintextLoopback && isLoopback(conn.RemoteAddr()) {
		return nil
	}
	return model.NewError(model.Unsupported, "TLS required before authentication")
}

// isLoopback reports whether addr is a loopback IP address. Host names are
// not resolved: only the connected peer address counts.
func isLoopback(addr net.Addr) bool {
	if addr == nil {
		return false
	}
	if tcp, ok := addr.(*net.TCPAddr); ok {
		ip, ok := netip.AddrFromSlice(tcp.IP)
		return ok && ip.Unmap().IsLoopback()
	}
	ap, err := netip.ParseAddrPort(addr.String())
	return err == nil && ap.Addr().Unmap().IsLoopback()
}
