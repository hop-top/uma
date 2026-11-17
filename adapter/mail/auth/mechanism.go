package auth

import (
	"crypto/tls"
	"slices"
	"strings"

	"github.com/emersion/go-sasl"
	"github.com/xdg-go/scram"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Mechanism names. IMAPLogin is not a SASL mechanism: it names the IMAP LOGIN
// command, the last password fallback.
const (
	OAuthBearer     = "OAUTHBEARER"
	XOAuth2         = "XOAUTH2"
	SCRAMSHA256Plus = "SCRAM-SHA-256-PLUS"
	SCRAMSHA256     = "SCRAM-SHA-256"
	SCRAMSHA1       = "SCRAM-SHA-1"
	Plain           = "PLAIN"
	Login           = "LOGIN"
	External        = "EXTERNAL"
	GSSAPI          = "GSSAPI"
	NTLM            = "NTLM"
	IMAPLogin       = "IMAP-LOGIN"
)

// Preference lists the mechanisms tried for each credential kind, strongest
// first. Kinds without an entry cannot authenticate mail. Across kinds the
// strength order is OAuth2, SCRAM, GSSAPI, NTLM, PLAIN, LOGIN: NTLM sits
// after SCRAM and before PLAIN (the server never sees the password) and is
// usable only for a "DOMAIN\user" name.
var preference = map[ports.CredentialKind][]string{
	ports.KindOAuth2:     {OAuthBearer, XOAuth2},
	ports.KindPassword:   {SCRAMSHA256Plus, SCRAMSHA256, SCRAMSHA1, NTLM, Plain, Login, IMAPLogin},
	ports.KindClientCert: {External},
	ports.KindKerberos:   {GSSAPI},
}

// Preference returns the mechanisms Choose considers for kind, strongest
// first. It is empty for kinds that cannot authenticate mail.
func Preference(kind ports.CredentialKind) []string {
	return slices.Clone(preference[kind])
}

// Offer is what one connection allows at the point AUTH may begin.
type Offer struct {
	// Mechanisms are the SASL mechanisms the server advertises.
	Mechanisms []string
	// LoginCommand reports that the IMAP LOGIN command is available: an IMAP
	// server that does not advertise LOGINDISABLED.
	LoginCommand bool
	// TLS is the connection's TLS state; nil on a plaintext connection.
	TLS *tls.ConnectionState
}

func (o Offer) advertises(mech string) bool {
	return slices.ContainsFunc(o.Mechanisms, func(m string) bool { return strings.EqualFold(m, mech) })
}

// channelBinding returns tls-exporter binding data, or false when the
// connection cannot provide it (plaintext, or TLS 1.2 without extended master
// secret).
func (o Offer) channelBinding() (scram.ChannelBinding, bool) {
	if o.TLS == nil || !o.TLS.HandshakeComplete {
		return scram.ChannelBinding{}, false
	}
	cb, err := scram.NewTLSExporterBinding(o.TLS)
	if err != nil {
		return scram.ChannelBinding{}, false
	}
	return cb, true
}

// usable reports whether mech can run for cred on this offer.
func (o Offer) usable(mech string, cred ports.Credential) bool {
	switch mech {
	case IMAPLogin:
		// LOGIN carries no authorization identity: never act as someone
		// else by dropping it.
		p := cred.(ports.Password)
		return o.LoginCommand && p.AuthzID == ""
	case Login:
		p := cred.(ports.Password)
		return o.advertises(mech) && p.AuthzID == ""
	case NTLM:
		// NTLM carries no authorization identity either.
		p := cred.(ports.Password)
		_, _, domain := ntlmDomainUser(p.User)
		return domain && p.AuthzID == "" && o.advertises(mech)
	case SCRAMSHA256Plus:
		_, ok := o.channelBinding()
		return ok && o.advertises(mech)
	case External:
		return o.TLS != nil && o.advertises(mech)
	}
	return o.advertises(mech)
}

// Choose returns the strongest mechanism usable for cred on offer. It fails
// with model.Unsupported when the intersection is empty.
func Choose(cred ports.Credential, offer Offer) (string, error) {
	cred = concrete(cred)
	if cred == nil {
		return "", model.NewError(model.Unsupported, "no credential")
	}
	for _, mech := range preference[cred.Kind()] {
		if offer.usable(mech, cred) {
			return mech, nil
		}
	}
	return "", model.NewError(model.Unsupported, "no usable authentication mechanism for a "+
		string(cred.Kind())+" credential (server offers: "+safeMechanisms(offer)+")")
}

// safeMechanisms renders server-advertised names for a safe message: SASL
// mechanism charset only, bounded length.
func safeMechanisms(o Offer) string {
	var names []string
	for _, m := range o.Mechanisms {
		m = strings.ToUpper(m)
		if len(m) == 0 || len(m) > 20 || strings.IndexFunc(m, func(r rune) bool {
			return (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
		}) >= 0 {
			continue
		}
		names = append(names, m)
		if len(names) == 16 {
			break
		}
	}
	if o.LoginCommand {
		names = append(names, "LOGIN command")
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

// Target is the server a SASL client authenticates to.
type Target struct {
	// Host is the server name: the OAUTHBEARER host and the host of the
	// GSSAPI service principal.
	Host string
	// Kerberos configures GSSAPI; only read for a Kerberos credential.
	Kerberos KerberosConfig
}

// NewClient returns the SASL client for mech, as chosen by Choose for cred and
// offer. IMAPLogin has no SASL client: Connect runs the LOGIN command through
// LoginConn instead. The GSSAPI client contacts the KDC when started.
func NewClient(mech string, cred ports.Credential, offer Offer, target Target) (sasl.Client, error) {
	cred = concrete(cred)
	if cred == nil || !slices.Contains(preference[cred.Kind()], mech) || mech == IMAPLogin {
		return nil, model.NewError(model.Unsupported, "mechanism "+mech+" not usable for this credential")
	}
	switch c := cred.(type) {
	case ports.OAuth2:
		token := func() (string, error) { return c.AccessToken, nil }
		if mech == OAuthBearer {
			return NewOAuthBearerClient(identity(c.User, c.AuthzID), target.Host, token), nil
		}
		return NewXOAuth2Client(identity(c.User, c.AuthzID), token), nil
	case ports.Password:
		switch mech {
		case SCRAMSHA256Plus, SCRAMSHA256, SCRAMSHA1:
			sc, err := newSCRAMClient(mech, c, offer)
			if err != nil {
				return nil, model.NewError(model.Validation, mech+": credential not usable (SASLprep or channel binding)")
			}
			return sc, nil
		case Plain:
			return sasl.NewPlainClient(c.AuthzID, c.User, string(c.Secret)), nil
		case Login:
			return newLoginClient(c.User, string(c.Secret)), nil
		case NTLM:
			return newNTLMClient(c)
		}
	case ports.ClientCert:
		return sasl.NewExternalClient(c.AuthzID), nil
	case ports.Kerberos:
		return newGSSAPIClient(c, target.Host, target.Kerberos)
	}
	return nil, model.NewError(model.Unsupported, "mechanism "+mech+" not usable for this credential")
}

// identity is the account a bearer token acts for: the authorization identity
// when set (a shared mailbox), else the user.
func identity(user, authzID string) string {
	if authzID != "" {
		return authzID
	}
	return user
}
