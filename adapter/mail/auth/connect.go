package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/emersion/go-sasl"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// ErrRejected is wrapped by Conn.Authenticate and LoginConn.Login errors when
// the server refused the credential (IMAP NO, SMTP 535). Other errors are
// transport or protocol failures and never trigger a credential retry.
var ErrRejected = errors.New("auth: credentials rejected")

// Conn is one IMAP or SMTP connection at the point AUTH may begin: greeting
// read, STARTTLS done when used, capabilities known.
type Conn interface {
	// Mechanisms returns the SASL mechanisms the server advertises.
	Mechanisms() []string
	// TLS returns the connection's TLS state; false on plaintext. The
	// handshake must be complete: a client that handshakes lazily (go-smtp
	// after STARTTLS) sends its next command (EHLO) first, else Connect
	// refuses the connection as plaintext.
	TLS() (tls.ConnectionState, bool)
	// RemoteAddr is the connected peer address.
	RemoteAddr() net.Addr
	// Authenticate runs one SASL exchange (imapclient.Client.Authenticate,
	// smtp.Client.Auth). A server refusal wraps ErrRejected. An empty client
	// response must go out as an empty line: go-imap v2 sends "=", which
	// RFC 9051 allows only for the initial response and Dovecot rejects
	// (GSSAPI answers the server's AP-REP with an empty response).
	Authenticate(sasl.Client) error
	Close() error
}

// LoginConn is a Conn with the IMAP LOGIN command.
type LoginConn interface {
	Conn
	// LoginCommand reports whether LOGIN is available (no LOGINDISABLED).
	LoginCommand() bool
	// Login runs the LOGIN command. A server refusal wraps ErrRejected.
	Login(user, password string) error
}

// Dialer opens connections to one server. Each function returns a Conn ready
// for AUTH. The tls.Config passed in comes from TLSConfig and may be adjusted
// (e.g. NextProtos) but never to skip verification.
type Dialer struct {
	// TLS connects with implicit TLS (IMAPS 993, submissions 465). Nil when
	// the server has no implicit TLS port.
	TLS func(context.Context, *tls.Config) (Conn, error)
	// StartTLS connects and upgrades with STARTTLS (143, 587). Used when TLS
	// is nil or its port refuses the connection.
	StartTLS func(context.Context, *tls.Config) (Conn, error)
	// Plain connects without TLS. Used only when TLS and StartTLS are both
	// nil and Policy.AllowPlaintextLoopback is set; the peer must be
	// loopback.
	Plain func(context.Context) (Conn, error)
}

// Config is one authenticated-connection request.
type Config struct {
	Credentials ports.Credentials
	// Request identifies the credential. Accept must list the kinds the
	// adapter can use; Fresh is managed by Connect.
	Request ports.CredentialRequest
	// ServerName is verified against the server certificate, sent as the
	// OAUTHBEARER host and names the GSSAPI service principal host.
	ServerName string
	// Kerberos configures GSSAPI for a Kerberos credential.
	Kerberos KerberosConfig
	// RootCAs verifies server certificates; nil means the system roots.
	RootCAs *x509.CertPool
	Policy  Policy
	Dialer  Dialer
}

// Session is an authenticated connection.
type Session struct {
	Conn      Conn
	Mechanism string
	Kind      ports.CredentialKind
	// ReconnectAt is when the session must be replaced (credential expiry
	// minus the policy margin); zero when the credential has no expiry.
	ReconnectAt time.Time
}

// Connect resolves a credential, dials, enforces TLS before AUTH, negotiates
// the strongest common mechanism and authenticates. An OAuth2 rejection
// triggers one Fresh resolve and one retry on a new connection with the same
// mechanism. The returned session's Conn is owned by the caller.
func Connect(ctx context.Context, cfg Config) (*Session, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	s, err := cfg.attempt(ctx, false, "")
	if err == nil || !errors.Is(err, errRetryFresh) {
		return s, err
	}
	var retry *retryError
	errors.As(err, &retry)
	return cfg.attempt(ctx, true, retry.mechanism)
}

func (cfg Config) validate() error {
	switch {
	case cfg.Credentials == nil:
		return cfg.fail(model.Validation, "credentials port not configured")
	case len(cfg.Request.Accept) == 0:
		return cfg.fail(model.Validation, "credential request accepts no kind")
	case cfg.Policy.ReconnectMargin < 0:
		return cfg.fail(model.Validation, "reconnect margin is negative")
	case cfg.Dialer.TLS == nil && cfg.Dialer.StartTLS == nil && cfg.Dialer.Plain == nil:
		return cfg.fail(model.Validation, "no dialer configured")
	}
	return nil
}

// errRetryFresh marks an OAuth2 rejection that may be retried once with a
// fresh token.
var errRetryFresh = errors.New("auth: retry with fresh credential")

type retryError struct{ mechanism string }

func (*retryError) Error() string        { return errRetryFresh.Error() }
func (*retryError) Is(target error) bool { return target == errRetryFresh }

// attempt runs one resolve-dial-authenticate cycle. On a retry, the mechanism
// must equal the one rejected first.
func (cfg Config) attempt(ctx context.Context, fresh bool, mechanism string) (*Session, error) {
	cred, err := cfg.resolve(ctx, fresh)
	if err != nil {
		return nil, err
	}
	conn, err := cfg.dial(ctx, cred)
	if err != nil {
		return nil, err
	}
	s, err := cfg.authenticate(conn, cred, fresh, mechanism)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return s, nil
}

func (cfg Config) resolve(ctx context.Context, fresh bool) (ports.Credential, error) {
	req := cfg.Request
	req.Fresh = fresh
	cred, err := cfg.Credentials.Resolve(ctx, req)
	if err != nil {
		var me *model.Error
		if errors.As(err, &me) || isContextErr(err) {
			return nil, err
		}
		return nil, cfg.fail(model.Unavailable, "credential store unavailable")
	}
	cred = concrete(cred)
	if cred == nil || !req.Accepts(cred.Kind()) {
		return nil, cfg.fail(model.Unsupported, "credential kind not accepted")
	}
	if len(preference[cred.Kind()]) == 0 {
		return nil, cfg.fail(model.Unsupported, "a "+string(cred.Kind())+" credential cannot authenticate mail")
	}
	return cred, nil
}

// dial tries implicit TLS, then STARTTLS when implicit TLS is not configured
// or its port refused the connection. Plaintext is used only when no TLS
// dialer exists and the policy opted in.
func (cfg Config) dial(ctx context.Context, cred ports.Credential) (Conn, error) {
	tlsCfg := TLSConfig(cfg.ServerName, cfg.RootCAs, cred)
	d := cfg.Dialer
	var conn Conn
	var err error
	switch {
	case d.TLS != nil:
		conn, err = d.TLS(ctx, tlsCfg.Clone())
		if err != nil && d.StartTLS != nil && errors.Is(err, syscall.ECONNREFUSED) {
			conn, err = d.StartTLS(ctx, tlsCfg.Clone())
		}
	case d.StartTLS != nil:
		conn, err = d.StartTLS(ctx, tlsCfg.Clone())
	default:
		if !cfg.Policy.AllowPlaintextLoopback {
			return nil, cfg.fail(model.Unsupported, "TLS required before authentication")
		}
		conn, err = d.Plain(ctx)
	}
	if err != nil {
		var me *model.Error
		if errors.As(err, &me) || isContextErr(err) {
			return nil, err
		}
		return nil, cfg.fail(model.Unavailable, "connection failed")
	}
	if err := CheckTransport(conn, cfg.Policy); err != nil {
		_ = conn.Close()
		return nil, cfg.fail(model.Unsupported, "TLS required before authentication")
	}
	return conn, nil
}

func (cfg Config) authenticate(conn Conn, cred ports.Credential, fresh bool, want string) (*Session, error) {
	offer := Offer{Mechanisms: conn.Mechanisms()}
	if state, ok := conn.TLS(); ok {
		offer.TLS = &state
	}
	lc, isLogin := conn.(LoginConn)
	offer.LoginCommand = isLogin && lc.LoginCommand()

	mech, err := Choose(cred, offer)
	if err != nil {
		return nil, cfg.wrap(err)
	}
	if want != "" && mech != want {
		// The retry server offers something else: never switch mechanism
		// after a rejection.
		return nil, cfg.fail(model.Unauthorized, "mechanism changed after rejection")
	}

	var client sasl.Client
	if mech == IMAPLogin {
		p := cred.(ports.Password)
		err = lc.Login(p.User, string(p.Secret))
	} else {
		client, err = NewClient(mech, cred, offer, Target{Host: cfg.ServerName, Kerberos: cfg.Kerberos})
		if err != nil {
			return nil, cfg.wrap(err)
		}
		err = conn.Authenticate(client)
	}
	if v, ok := client.(*scramClient); ok {
		if verr := v.verify(); verr != nil && (err == nil || !errors.Is(err, ErrRejected)) {
			return nil, cfg.fail(model.Unauthorized, mech+" server verification failed")
		}
	}
	// A client-side GSSAPI or NTLM failure (KDC, server proof, negotiation)
	// explains the aborted exchange better than the protocol error.
	if cerr := clientErr(client); cerr != nil && (err == nil || !errors.Is(err, ErrRejected)) {
		return nil, cfg.wrap(cerr)
	}
	switch {
	case err == nil:
		return &Session{Conn: conn, Mechanism: mech, Kind: cred.Kind(), ReconnectAt: cfg.Policy.ReconnectAt(cred)}, nil
	case isContextErr(err):
		return nil, err
	case !errors.Is(err, ErrRejected):
		return nil, cfg.fail(model.Transient, mech+" authentication exchange failed")
	case cred.Kind() == ports.KindOAuth2 && !fresh:
		return nil, &retryError{mechanism: mech}
	}
	return nil, cfg.fail(model.Unauthorized, mech+" credentials rejected")
}

// fail returns a model error whose safe message names the request purpose.
func (cfg Config) fail(code model.ErrorCode, msg string) *model.Error {
	if p := strings.TrimSpace(cfg.Request.Purpose); p != "" {
		msg = p + ": " + msg
	}
	return model.NewError(code, msg)
}

// wrap prefixes a package model error with the request purpose.
func (cfg Config) wrap(err error) error {
	var me *model.Error
	if errors.As(err, &me) {
		return cfg.fail(me.Code, me.SafeMessage)
	}
	return cfg.fail(model.Permanent, "authentication setup failed")
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// clientErr returns the failure a mechanism client recorded, if any.
func clientErr(c sasl.Client) error {
	switch v := c.(type) {
	case *gssapiClient:
		return v.err
	case *ntlmClient:
		return v.err
	}
	return nil
}

// concrete dereferences pointer credentials so type switches see values.
func concrete(c ports.Credential) ports.Credential {
	switch v := c.(type) {
	case *ports.Secret:
		if v != nil {
			return *v
		}
	case *ports.Password:
		if v != nil {
			return *v
		}
	case *ports.OAuth2:
		if v != nil {
			return *v
		}
	case *ports.ClientCert:
		if v != nil {
			return *v
		}
	case *ports.Kerberos:
		if v != nil {
			return *v
		}
	default:
		return c
	}
	return nil
}
