package auth

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/asn1tools"
	"github.com/jcmturner/gokrb5/v8/client"
	"github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/credentials"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/iana/chksumtype"
	"github.com/jcmturner/gokrb5/v8/iana/errorcode"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/flags"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/krberror"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/spnego"
	"github.com/jcmturner/gokrb5/v8/types"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// KerberosConfig configures GSSAPI for a ports.Kerberos credential.
type KerberosConfig struct {
	// Service is the GSSAPI host-based service name (RFC 4752): "imap" for
	// IMAP, "smtp" for SMTP. The service principal is Service/host.
	Service string
	// KDCs are "host:port" addresses of the credential realm's KDCs, tried
	// in order over TCP. Empty means DNS SRV lookup (_kerberos._tcp.REALM),
	// which Active Directory publishes.
	KDCs []string
}

// Security-layer bits of the RFC 4752 section 3.3 negotiation message.
const (
	securityLayerNone      byte = 0x01
	securityLayerIntegrity byte = 0x02
	securityLayerConf      byte = 0x04
)

// Wrap token flags (RFC 4121 section 4.2.2).
const (
	wrapSentByAcceptor byte = 0x01
	wrapSealed         byte = 0x02
	wrapAcceptorSubkey byte = 0x04
)

// gssapiEnctypes are the only encryption types negotiated: the RFC 4121
// (CFX) wrap tokens used for the security-layer messages exist for these;
// DES and RC4 keys would need the legacy RFC 1964 tokens.
var gssapiEnctypes = []int32{
	etypeID.AES256_CTS_HMAC_SHA1_96,
	etypeID.AES128_CTS_HMAC_SHA1_96,
	etypeID.AES256_CTS_HMAC_SHA384_192,
	etypeID.AES128_CTS_HMAC_SHA256_128,
}

type gssapiState int

const (
	gssapiAPRep gssapiState = iota + 1
	gssapiSecurityLayer
	gssapiDone
)

// gssapiClient is the SASL GSSAPI mechanism (RFC 4752) over Kerberos V5
// (RFC 4121): mutual authentication, then the security-layer negotiation,
// always answering "no security layer" because TLS protects the
// connection. Start obtains a service ticket from the KDC on every attempt.
//
// Client-side failures (KDC, server proof, negotiation message) are kept in
// err as a *model.Error; Connect reports it instead of the protocol error.
type gssapiClient struct {
	cred    ports.Kerberos
	krb     KerberosConfig
	host    string
	newConn func(ports.Kerberos, *config.Config) (*client.Client, error)

	state      gssapiState
	sessionKey types.EncryptionKey
	subkey     *types.EncryptionKey // acceptor subkey from AP-REP
	auth       types.Authenticator
	seq        uint64 // auth.SeqNumber
	err        error
}

func newGSSAPIClient(c ports.Kerberos, host string, krb KerberosConfig) (*gssapiClient, error) {
	switch {
	case strings.TrimSpace(krb.Service) == "":
		return nil, model.NewError(model.Validation, "GSSAPI service name not configured")
	case host == "":
		return nil, model.NewError(model.Validation, "GSSAPI server name not configured")
	case c.Principal == "" || c.Realm == "":
		return nil, model.NewError(model.Validation, "Kerberos credential lacks principal or realm")
	case (len(c.Keytab) == 0) == (len(c.CCache) == 0):
		return nil, model.NewError(model.Validation, "Kerberos credential needs exactly one of keytab and credential cache")
	}
	return &gssapiClient{cred: c, krb: krb, host: host, newConn: kerberosLogin}, nil
}

// kerberosConfig builds the client configuration for realm: AES only, TCP
// only, KDCs listed or found through DNS SRV.
func kerberosConfig(realm string, kdcs []string) *config.Config {
	cfg := config.New()
	cfg.LibDefaults.DefaultRealm = realm
	cfg.LibDefaults.UDPPreferenceLimit = 1
	cfg.LibDefaults.DNSLookupKDC = len(kdcs) == 0
	cfg.LibDefaults.DNSLookupRealm = false
	cfg.LibDefaults.DNSCanonicalizeHostname = false
	cfg.LibDefaults.RDNS = false
	cfg.LibDefaults.DefaultTktEnctypeIDs = gssapiEnctypes
	cfg.LibDefaults.DefaultTGSEnctypeIDs = gssapiEnctypes
	cfg.LibDefaults.PermittedEnctypeIDs = gssapiEnctypes
	cfg.Realms = []config.Realm{{Realm: realm, KDC: kdcs}}
	return cfg
}

// kerberosLogin returns a logged-in Kerberos client for c.
func kerberosLogin(c ports.Kerberos, cfg *config.Config) (*client.Client, error) {
	if len(c.Keytab) > 0 {
		kt := keytab.New()
		if err := kt.Unmarshal(c.Keytab); err != nil {
			return nil, model.NewError(model.Validation, "Kerberos keytab not readable")
		}
		cl := client.NewWithKeytab(c.Principal, c.Realm, kt, cfg, client.DisablePAFXFAST(true))
		if err := cl.Login(); err != nil {
			return nil, kerberosError(err)
		}
		return cl, nil
	}
	cc := new(credentials.CCache)
	if err := cc.Unmarshal(c.CCache); err != nil {
		return nil, model.NewError(model.Validation, "Kerberos credential cache not readable")
	}
	if !strings.EqualFold(cc.GetClientRealm(), c.Realm) ||
		cc.GetClientPrincipalName().PrincipalNameString() != c.Principal {
		return nil, model.NewError(model.Validation, "Kerberos credential cache is for another principal")
	}
	cl, err := client.NewFromCCache(cc, cfg, client.DisablePAFXFAST(true))
	if err != nil {
		return nil, kerberosError(err)
	}
	return cl, nil
}

// kerberosError maps a KDC exchange failure to a model error. Rejections of
// the client (unknown, revoked, wrong key, expired) are Unauthorized; an
// unreachable KDC is Unavailable.
func kerberosError(err error) *model.Error {
	if code, ok := krbErrorCode(err); ok {
		switch code {
		case errorcode.KDC_ERR_C_PRINCIPAL_UNKNOWN, errorcode.KDC_ERR_PREAUTH_FAILED,
			errorcode.KDC_ERR_CLIENT_REVOKED, errorcode.KDC_ERR_KEY_EXPIRED,
			errorcode.KRB_AP_ERR_TKT_EXPIRED, errorcode.KDC_ERR_POLICY:
			return model.NewError(model.Unauthorized, "GSSAPI: KDC rejected the credential")
		case errorcode.KDC_ERR_S_PRINCIPAL_UNKNOWN:
			return model.NewError(model.Permanent, "GSSAPI: KDC does not know the mail service principal")
		case errorcode.KRB_AP_ERR_SKEW:
			return model.NewError(model.Transient, "GSSAPI: clock skew with the KDC")
		}
		return model.NewError(model.Unauthorized, "GSSAPI: KDC refused the request")
	}
	if strings.Contains(err.Error(), krberror.NetworkingError) {
		return model.NewError(model.Unavailable, "GSSAPI: KDC unreachable")
	}
	return model.NewError(model.Unauthorized, "GSSAPI: Kerberos exchange failed")
}

// krbErrorCode extracts the KRB-ERROR code from err. gokrb5 often flattens
// a KRBError into a Krberror message ("KRB Error: (7) ..."), so the text is
// parsed when the typed error is gone.
func krbErrorCode(err error) (int32, bool) {
	var ke messages.KRBError
	if errors.As(err, &ke) {
		return ke.ErrorCode, true
	}
	const marker = "KRB Error: ("
	msg := err.Error()
	i := strings.Index(msg, marker)
	if i < 0 {
		return 0, false
	}
	rest := msg[i+len(marker):]
	j := strings.IndexByte(rest, ')')
	if j < 0 {
		return 0, false
	}
	n, perr := strconv.ParseInt(rest[:j], 10, 32)
	return int32(n), perr == nil
}

func (c *gssapiClient) fail(code model.ErrorCode, msg string) error {
	c.err = model.NewError(code, "GSSAPI: "+msg)
	return c.err
}

// Start obtains a service ticket for Service/host and returns the initial
// context token: an AP-REQ requesting mutual authentication and integrity.
func (c *gssapiClient) Start() (string, []byte, error) {
	c.state, c.subkey, c.err = 0, nil, nil
	cl, err := c.newConn(c.cred, kerberosConfig(c.cred.Realm, c.krb.KDCs))
	if err != nil {
		var me *model.Error
		if !errors.As(err, &me) {
			me = kerberosError(err)
		}
		c.err = me
		return "", nil, me
	}
	defer cl.Destroy()
	tkt, key, err := cl.GetServiceTicket(c.krb.Service + "/" + c.host)
	if err != nil {
		c.err = kerberosError(err)
		return "", nil, c.err
	}
	auth, err := types.NewAuthenticator(cl.Credentials.Domain(), cl.Credentials.CName())
	if err != nil {
		return "", nil, c.fail(model.Permanent, "authenticator not created")
	}
	// Initiator sequence number (RFC 4121 section 4.2.2), kept unsigned for
	// the wrap token.
	var sb [4]byte
	if _, err := rand.Read(sb[:]); err != nil {
		return "", nil, c.fail(model.Permanent, "sequence number not generated")
	}
	seq := binary.BigEndian.Uint32(sb[:]) & 0x3fffffff
	auth.SeqNumber = int64(seq)
	auth.Cksum = types.Checksum{
		CksumType: chksumtype.GSSAPI,
		Checksum:  gssChecksum(gssapi.ContextFlagMutual | gssapi.ContextFlagInteg),
	}
	req, err := messages.NewAPReq(tkt, key, auth)
	if err != nil {
		return "", nil, c.fail(model.Permanent, "AP-REQ not created")
	}
	types.SetFlag(&req.APOptions, flags.APOptionMutualRequired)
	reqBytes, err := req.Marshal()
	if err != nil {
		return "", nil, c.fail(model.Permanent, "AP-REQ not encoded")
	}
	c.sessionKey, c.auth, c.seq, c.state = key, auth, uint64(seq), gssapiAPRep
	return GSSAPI, initialContextToken(reqBytes), nil
}

// gssChecksum is the authenticator checksum of RFC 4121 section 4.1.1: no
// channel bindings, the requested context flags, no delegation.
func gssChecksum(flags uint32) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b[0:4], 16)
	binary.LittleEndian.PutUint32(b[20:24], flags)
	return b
}

// initialContextToken frames an AP-REQ as the GSS-API initial context token
// (RFC 2743 section 3.1, RFC 4121 section 4.1).
func initialContextToken(apReq []byte) []byte {
	oid, _ := asn1.Marshal(gssapi.OIDKRB5.OID())
	b := append(oid, 0x01, 0x00)
	return asn1tools.AddASNAppTag(append(b, apReq...), 0)
}

func (c *gssapiClient) Next(challenge []byte) ([]byte, error) {
	switch c.state {
	case gssapiAPRep:
		if err := c.acceptAPRep(challenge); err != nil {
			return nil, err
		}
		c.state = gssapiSecurityLayer
		// Context established with no further token: empty response.
		return []byte{}, nil
	case gssapiSecurityLayer:
		resp, err := c.securityLayer(challenge)
		if err != nil {
			return nil, err
		}
		c.state = gssapiDone
		return resp, nil
	}
	return nil, c.fail(model.Unauthorized, "unexpected server challenge")
}

// acceptAPRep verifies the server's AP-REP: it decrypts with the session
// key and echoes the authenticator time, proving the server holds the
// service key.
func (c *gssapiClient) acceptAPRep(token []byte) error {
	var t spnego.KRB5Token
	if err := t.Unmarshal(token); err != nil {
		return c.fail(model.Unauthorized, "server context token malformed")
	}
	if t.IsKRBError() {
		return c.fail(model.Unauthorized, "server rejected the ticket")
	}
	if !t.IsAPRep() {
		return c.fail(model.Unauthorized, "server sent no AP-REP")
	}
	plain, err := crypto.DecryptEncPart(t.APRep.EncPart, c.sessionKey, keyusage.AP_REP_ENCPART)
	if err != nil {
		return c.fail(model.Unauthorized, "server AP-REP not verifiable")
	}
	var part messages.EncAPRepPart
	if err := part.Unmarshal(plain); err != nil {
		return c.fail(model.Unauthorized, "server AP-REP malformed")
	}
	// KerberosTime has whole seconds; the microseconds travel in Cusec.
	if !part.CTime.Equal(c.auth.CTime.Truncate(time.Second)) || part.Cusec != c.auth.Cusec {
		return c.fail(model.Unauthorized, "server AP-REP does not match the authenticator")
	}
	if part.Subkey.KeyType != 0 && len(part.Subkey.KeyValue) > 0 {
		k := part.Subkey
		c.subkey = &k
	}
	return nil
}

// protectionKey is the key for wrap tokens: the acceptor subkey when the
// AP-REP carried one, else the ticket session key (no initiator subkey is
// sent).
func (c *gssapiClient) protectionKey() types.EncryptionKey {
	if c.subkey != nil {
		return *c.subkey
	}
	return c.sessionKey
}

// securityLayer answers the server's RFC 4752 section 3.1 negotiation
// message: it unwraps and verifies the 4-octet offer, requires that "no
// security layer" is offered, and returns the wrapped choice: octet 1 =
// no security layer, octets 2-4 = maximum buffer size 0, then the
// authorization identity.
func (c *gssapiClient) securityLayer(token []byte) ([]byte, error) {
	offer, err := c.unwrap(token)
	if err != nil {
		return nil, err
	}
	if len(offer) != 4 {
		return nil, c.fail(model.Unauthorized, "security-layer offer malformed")
	}
	if offer[0]&securityLayerNone == 0 {
		return nil, c.fail(model.Unsupported, "server requires a SASL security layer")
	}
	choice := append([]byte{securityLayerNone, 0, 0, 0}, c.cred.AuthzID...)
	return c.wrap(choice)
}

// unwrap verifies an acceptor wrap token without confidentiality (RFC 4121
// section 4.2.6.2) and returns its payload.
func (c *gssapiClient) unwrap(b []byte) ([]byte, error) {
	if len(b) < gssapi.HdrLen || b[0] != 0x05 || b[1] != 0x04 || b[3] != gssapi.FillerByte {
		return nil, c.fail(model.Unauthorized, "server wrap token malformed")
	}
	fl := b[2]
	switch {
	case fl&wrapSentByAcceptor == 0:
		return nil, c.fail(model.Unauthorized, "server wrap token not from the acceptor")
	case fl&wrapSealed != 0:
		return nil, c.fail(model.Unauthorized, "server wrap token is encrypted")
	case (fl&wrapAcceptorSubkey != 0) != (c.subkey != nil):
		return nil, c.fail(model.Unauthorized, "server wrap token uses the wrong key")
	}
	ec := binary.BigEndian.Uint16(b[4:6])
	rrc := int(binary.BigEndian.Uint16(b[6:8]))
	data := append([]byte{}, b[gssapi.HdrLen:]...)
	if len(data) > 0 && rrc%len(data) != 0 {
		r := rrc % len(data)
		data = append(data[r:], data[:r]...)
	}
	n := len(data) - int(ec)
	if n < 0 {
		return nil, c.fail(model.Unauthorized, "server wrap token malformed")
	}
	wt := gssapi.WrapToken{
		Flags:     fl,
		EC:        ec,
		SndSeqNum: binary.BigEndian.Uint64(b[8:16]),
		Payload:   data[:n],
		CheckSum:  data[n:],
	}
	if ok, err := wt.Verify(c.protectionKey(), keyusage.GSSAPI_ACCEPTOR_SEAL); err != nil || !ok {
		return nil, c.fail(model.Unauthorized, "server wrap token checksum invalid")
	}
	return wt.Payload, nil
}

// wrap returns an initiator wrap token without confidentiality for payload.
func (c *gssapiClient) wrap(payload []byte) ([]byte, error) {
	key := c.protectionKey()
	et, err := crypto.GetEtype(key.KeyType)
	if err != nil {
		return nil, c.fail(model.Unsupported, "session key type not supported")
	}
	ec, ok := wrapChecksumLen(key.KeyType)
	if !ok || et.GetHMACBitLength() != int(ec)*8 {
		return nil, c.fail(model.Unsupported, "session key type not supported")
	}
	var fl byte
	if c.subkey != nil {
		fl |= wrapAcceptorSubkey
	}
	wt := gssapi.WrapToken{
		Flags:     fl,
		EC:        ec,
		SndSeqNum: c.seq,
		Payload:   payload,
	}
	if err := wt.SetCheckSum(key, keyusage.GSSAPI_INITIATOR_SEAL); err != nil {
		return nil, c.fail(model.Permanent, "wrap token not created")
	}
	b, err := wt.Marshal()
	if err != nil {
		return nil, c.fail(model.Permanent, "wrap token not encoded")
	}
	return b, nil
}

// wrapChecksumLen is the RFC 4121 wrap token checksum length (EC) for the
// AES encryption types: the truncated HMAC size of RFC 3962 / RFC 8009.
func wrapChecksumLen(keyType int32) (uint16, bool) {
	switch keyType {
	case etypeID.AES128_CTS_HMAC_SHA1_96, etypeID.AES256_CTS_HMAC_SHA1_96:
		return 12, true
	case etypeID.AES128_CTS_HMAC_SHA256_128:
		return 16, true
	case etypeID.AES256_CTS_HMAC_SHA384_192:
		return 24, true
	}
	return 0, false
}
