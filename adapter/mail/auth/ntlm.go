package auth

import (
	"strings"

	"github.com/Azure/go-ntlmssp"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// ntlmDomainUser splits a domain-qualified user "DOMAIN\user". Only the
// down-level form qualifies: "user@domain" is how every hosted provider
// spells an ordinary login, so treating it as NTLM-eligible would move those
// accounts off SCRAM/PLAIN on servers that also advertise NTLM.
func ntlmDomainUser(user string) (domain, name string, ok bool) {
	domain, name, ok = strings.Cut(user, `\`)
	return domain, name, ok && domain != "" && name != "" && !strings.Contains(name, `\`)
}

type ntlmState int

const (
	ntlmChallenge ntlmState = iota + 1
	ntlmDone
)

// ntlmClient is SASL "NTLM" as Exchange ([MS-OXIMAP4], [MS-SMTPNTLM]) and
// Dovecot implement it: the NEGOTIATE_MESSAGE is the initial response, the
// server's CHALLENGE_MESSAGE is the one challenge, the AUTHENTICATE_MESSAGE
// (NTLMv2, via go-ntlmssp) the final response. Sending NEGOTIATE as the
// initial response avoids Exchange SMTP's non-base64 "334 ntlm supported"
// prompt; go-imap waits for the server's empty continuation itself when
// SASL-IR is not advertised.
type ntlmClient struct {
	user, password string // user is "DOMAIN\name", split by go-ntlmssp
	state          ntlmState
	err            error
}

func newNTLMClient(p ports.Password) (*ntlmClient, error) {
	if _, _, ok := ntlmDomainUser(p.User); !ok {
		return nil, model.NewError(model.Validation, `NTLM needs a DOMAIN\user name`)
	}
	return &ntlmClient{user: p.User, password: string(p.Secret)}, nil
}

func (c *ntlmClient) fail(msg string) error {
	c.err = model.NewError(model.Unauthorized, "NTLM: "+msg)
	return c.err
}

func (c *ntlmClient) Start() (string, []byte, error) {
	c.state, c.err = ntlmChallenge, nil
	// Domain and workstation name the client machine, not the user; empty
	// is the library's recommendation.
	neg, err := ntlmssp.NewNegotiateMessage("", "")
	if err != nil {
		return "", nil, c.fail("negotiate message not created")
	}
	return NTLM, neg, nil
}

func (c *ntlmClient) Next(challenge []byte) ([]byte, error) {
	if c.state != ntlmChallenge {
		return nil, c.fail("unexpected server challenge")
	}
	c.state = ntlmDone
	am, err := ntlmssp.NewAuthenticateMessage(challenge, c.user, c.password, nil)
	if err != nil {
		// Malformed challenge, or one demanding NTLMv1 or key exchange.
		return nil, c.fail("server challenge not usable (NTLMv2 only)")
	}
	return am, nil
}
