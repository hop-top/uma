// Package auth authenticates IMAP and SMTP connections with a credential
// resolved through ports.Credentials.
//
// The package is protocol-neutral: an IMAP or SMTP client wraps its
// connection as a Conn (go-imap's imapclient.Client.Authenticate and go-smtp's
// Client.Auth both take a sasl.Client) and Connect runs the policy:
//
//   - TLS before AUTH. Implicit TLS first; STARTTLS only when no implicit
//     dialer is configured or the implicit port refuses the connection.
//     A plaintext connection may authenticate only when the peer address is
//     loopback and the channel opted in (Policy.AllowPlaintextLoopback).
//     Certificates are always verified.
//   - Mechanism = credential kind ∩ server-advertised, strongest first:
//     OAuth2: OAUTHBEARER, XOAUTH2. Password: SCRAM-SHA-256-PLUS
//     (tls-exporter channel binding), SCRAM-SHA-256, SCRAM-SHA-1, NTLM,
//     PLAIN, LOGIN, then the IMAP LOGIN command unless LOGINDISABLED.
//     ClientCert: EXTERNAL. Kerberos: GSSAPI. CRAM-MD5 is never used.
//   - NTLM (NTLMv2 only) is used only for a Password whose User is
//     "DOMAIN\user" and that has no AuthzID; "user@domain" is an ordinary
//     login name.
//   - GSSAPI (RFC 4752) obtains a service ticket for
//     KerberosConfig.Service/ServerName from the credential's keytab or
//     credential cache, requires mutual authentication and always selects
//     "no security layer": TLS already protects the connection. AES
//     encryption types only.
//   - No downgrade: a rejected mechanism ends the attempt; no weaker
//     mechanism is tried with the same credential.
//   - OAuth2 rejection: one Fresh resolve, one reconnect, one retry with the
//     same mechanism; a second rejection is model.Unauthorized.
//
// Errors are *model.Error (or a context error) whose safe message names the
// request Purpose ("imap", "smtp.submission", ...) and never a credential.
package auth
