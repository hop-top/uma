package httpmail

import (
	"strings"

	"github.com/emersion/go-msgauth/authres"

	"hop.top/uma/adapter/mail/mime"
)

// Source assurance values (Envelope.Assurance). They are the values package
// smtp derives from its own DMARC evaluation, so a consumer reads both
// adapters alike. They grade the sender; none is identity proof.
const (
	// AssuranceUnverified: the From address is the sender's claim. It is the
	// value of every message unless the channel trusts a stamp that says
	// otherwise.
	AssuranceUnverified = "unverified"
	// AssuranceDMARCPass: the trusted host reports DMARC pass for the From
	// domain.
	AssuranceDMARCPass = "dmarc_pass"
	// AssuranceDMARCFail: the From domain publishes DMARC and nothing
	// aligned passed.
	AssuranceDMARCFail = "dmarc_fail"
	// AssuranceDMARCNone: the From domain publishes no DMARC record.
	AssuranceDMARCNone = "dmarc_none"
	// AssuranceDMARCTempError: a DNS failure prevented evaluation.
	AssuranceDMARCTempError = "dmarc_temperror"
	// AssuranceDMARCPermError: the From header or the DMARC record is
	// unusable.
	AssuranceDMARCPermError = "dmarc_permerror"
)

// Bounds on the claims recorded from one message.
const (
	maxClaims     = 8
	maxClaimBytes = 4096
	maxDKIM       = 8
)

// evidence is the sender evidence of one message: what the sender claimed
// and, when the channel trusts the stamp, what the stamping host verified.
// The result fields are present only when Trusted.
type evidence struct {
	// Trusted reports that the results below come from an
	// Authentication-Results value the channel's trust declaration names.
	// False means every field of the message is a claim.
	Trusted bool `json:"trusted"`
	// AuthservID is the id of the host whose stamp was read.
	AuthservID string       `json:"authserv_id,omitempty"`
	SPF        *spfResult   `json:"spf,omitempty"`
	DKIM       []dkimResult `json:"dkim,omitempty"`
	DMARC      *dmarcResult `json:"dmarc,omitempty"`
	Claimed    claimed      `json:"claimed"`
}

type spfResult struct {
	// Result is an RFC 7208 result as the stamp gives it.
	Result string `json:"result"`
	// Identity is the checked identity as the stamp gives it (smtp.mailfrom
	// or smtp.helo).
	Identity string `json:"identity,omitempty"`
}

type dkimResult struct {
	Result     string `json:"result"`
	Domain     string `json:"domain,omitempty"`
	Identifier string `json:"identifier,omitempty"`
}

type dmarcResult struct {
	Result     string `json:"result"`
	FromDomain string `json:"from_domain"`
}

// claimed is the authentication fields a message carries, uninterpreted.
type claimed struct {
	AuthenticationResults []string `json:"authentication_results,omitempty"`
	ReceivedSPF           []string `json:"received_spf,omitempty"`
}

// claimsOf records the authentication fields of a parsed message.
func claimsOf(h mime.Header) evidence {
	return evidence{Claimed: claimed{
		AuthenticationResults: claims(h.Values("Authentication-Results")),
		ReceivedSPF:           claims(h.Values("Received-SPF")),
	}}
}

func claims(values []string) []string {
	if len(values) > maxClaims {
		values = values[:maxClaims]
	}
	var out []string
	for _, v := range values {
		out = append(out, cut(v, maxClaimBytes))
	}
	return out
}

// trust reads, from the Authentication-Results values of a message (topmost
// first), the stamp of the channel's trusted authserv-id and returns ev with
// its results. Only the topmost value naming exactly that id is read (RFC
// 8601 5: the relay that stamps puts its field on top, and removes any a
// sender supplied under its id). A value that does not parse is no stamp.
// A DMARC result counts only when it names the From domain of the message.
// Without a trusted id in the configuration, nothing changes.
func (i *Instance) trust(ev evidence, sender string, values []string) evidence {
	id := i.cfg.Evidence.TrustedAuthservID
	if id == "" {
		return ev
	}
	for _, v := range values {
		got, results, err := authres.Parse(v)
		if !strings.EqualFold(got, id) {
			continue
		}
		if err != nil {
			return ev
		}
		ev.Trusted, ev.AuthservID = true, got
		for _, r := range results {
			switch r := r.(type) {
			case *authres.SPFResult:
				if ev.SPF == nil {
					ev.SPF = &spfResult{Result: string(r.Value), Identity: firstNonEmpty(r.From, r.Helo)}
				}
			case *authres.DKIMResult:
				if len(ev.DKIM) < maxDKIM {
					ev.DKIM = append(ev.DKIM, dkimResult{Result: string(r.Value), Domain: strings.ToLower(r.Domain), Identifier: r.Identifier})
				}
			case *authres.DMARCResult:
				if ev.DMARC == nil && fromDomainIs(r.From, sender) {
					ev.DMARC = &dmarcResult{Result: string(r.Value), FromDomain: strings.ToLower(r.From)}
				}
			}
		}
		return ev
	}
	return ev
}

// fromDomainIs reports whether the DMARC result's header.from names the
// domain of the sender address.
func fromDomainIs(headerFrom, sender string) bool {
	at := strings.LastIndexByte(sender, '@')
	return at >= 0 && strings.EqualFold(strings.TrimSuffix(headerFrom, "."), strings.TrimSuffix(sender[at+1:], "."))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// assurance maps the DMARC result of a trusted stamp to the source
// assurance; any other evidence leaves the sender unverified. DMARC is set
// only from a trusted stamp.
func (e evidence) assurance() string {
	if e.DMARC == nil {
		return AssuranceUnverified
	}
	switch e.DMARC.Result {
	case "pass":
		return AssuranceDMARCPass
	case "fail":
		return AssuranceDMARCFail
	case "none":
		return AssuranceDMARCNone
	case "temperror":
		return AssuranceDMARCTempError
	case "permerror":
		return AssuranceDMARCPermError
	}
	return AssuranceUnverified
}
