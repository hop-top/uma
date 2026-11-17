package auth

import (
	"errors"
	"fmt"

	"github.com/emersion/go-sasl"
	"github.com/xdg-go/scram"

	"hop.top/uma/ports"
)

// TokenFunc returns the bearer access token for one authentication attempt.
type TokenFunc func() (string, error)

// errServerChallenge reports a server challenge a mechanism does not expect.
var errServerChallenge = errors.New("auth: unexpected server challenge")

// NewXOAuth2Client returns an XOAUTH2 client (Google and Microsoft bearer
// token SASL). user is the mailbox to access. Start calls token on every
// attempt, so a reconnect always sends the current token.
func NewXOAuth2Client(user string, token TokenFunc) sasl.Client {
	return &xoauth2Client{user: user, token: token}
}

type xoauth2Client struct {
	user    string
	token   TokenFunc
	errSeen bool
}

func (c *xoauth2Client) Start() (string, []byte, error) {
	c.errSeen = false
	tok, err := c.token()
	if err != nil {
		return "", nil, err
	}
	return XOAuth2, []byte("user=" + c.user + "\x01auth=Bearer " + tok + "\x01\x01"), nil
}

// Next answers the server's JSON error challenge with an empty response, so
// the server ends the exchange with a regular failure reply.
func (c *xoauth2Client) Next([]byte) ([]byte, error) {
	if c.errSeen {
		return nil, errServerChallenge
	}
	c.errSeen = true
	return []byte{}, nil
}

// NewOAuthBearerClient returns an OAUTHBEARER (RFC 7628) client. authzID is
// sent as the GS2 authorization identity, host as the host key. Start calls
// token on every attempt, so a reconnect always sends the current token.
func NewOAuthBearerClient(authzID, host string, token TokenFunc) sasl.Client {
	return &oauthBearerClient{authzID: authzID, host: host, token: token}
}

type oauthBearerClient struct {
	authzID, host string
	token         TokenFunc
	errSeen       bool
}

func (c *oauthBearerClient) Start() (string, []byte, error) {
	c.errSeen = false
	tok, err := c.token()
	if err != nil {
		return "", nil, err
	}
	return sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: c.authzID,
		Token:    tok,
		Host:     c.host,
	}).Start()
}

// Next answers the server's JSON error challenge with %x01 (RFC 7628 section
// 3.2.3), so the server ends the exchange with a regular failure reply.
func (c *oauthBearerClient) Next([]byte) ([]byte, error) {
	if c.errSeen {
		return nil, errServerChallenge
	}
	c.errSeen = true
	return []byte{0x01}, nil
}

// scramClient adapts an xdg-go/scram conversation to sasl.Client. Connect
// checks verify after the exchange: success without a valid server signature
// is a failure (the server did not prove it knows the password).
type scramClient struct {
	mech string
	conv *scram.ClientConversation
	err  error
}

func newSCRAMClient(mech string, p ports.Password, offer Offer) (*scramClient, error) {
	hash := scram.SHA256
	if mech == SCRAMSHA1 {
		hash = scram.SHA1
	}
	client, err := hash.NewClient(p.User, string(p.Secret), p.AuthzID)
	if err != nil {
		return nil, err
	}
	cb, cbOK := offer.channelBinding()
	var conv *scram.ClientConversation
	switch {
	case mech == SCRAMSHA256Plus:
		if !cbOK {
			return nil, errors.New("auth: channel binding unavailable")
		}
		conv = client.NewConversationWithChannelBinding(cb)
	case cbOK && !offer.advertises(mech+"-PLUS"):
		// "y": this client binds, the server offered no PLUS variant. A
		// server that did offer one rejects this as a stripped list.
		conv = client.NewConversationAdvertisingChannelBinding()
	default:
		conv = client.NewConversation()
	}
	return &scramClient{mech: mech, conv: conv}, nil
}

func (c *scramClient) Start() (string, []byte, error) {
	r, err := c.conv.Step("")
	if err != nil {
		c.err = err
		return "", nil, err
	}
	return c.mech, []byte(r), nil
}

func (c *scramClient) Next(challenge []byte) ([]byte, error) {
	if c.conv.Done() {
		c.err = errServerChallenge
		return nil, c.err
	}
	r, err := c.conv.Step(string(challenge))
	if err != nil {
		c.err = err
		return nil, err
	}
	// Never nil: go-smtp ends the exchange early on a nil response.
	return append([]byte{}, r...), nil
}

func (c *scramClient) verify() error {
	if c.err != nil {
		return c.err
	}
	if !c.conv.Valid() {
		return errors.New("auth: server did not complete SCRAM verification")
	}
	return nil
}

// loginClient is the LOGIN mechanism without an initial response: the user
// name answers the first challenge, the password the second. Servers differ
// in challenge text, so it is not checked.
type loginClient struct {
	user, password string
	step           int
}

func newLoginClient(user, password string) *loginClient {
	return &loginClient{user: user, password: password}
}

func (c *loginClient) Start() (string, []byte, error) {
	c.step = 0
	return Login, nil, nil
}

func (c *loginClient) Next([]byte) ([]byte, error) {
	c.step++
	switch c.step {
	case 1:
		return []byte(c.user), nil
	case 2:
		return []byte(c.password), nil
	}
	return nil, fmt.Errorf("%w (step %d)", errServerChallenge, c.step)
}
