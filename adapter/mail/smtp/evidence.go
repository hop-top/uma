package smtp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"

	"hop.top/uma/adapter/mail/mime"
)

// Source assurance values (Envelope.Assurance) derived from the DMARC
// result for the From domain. They grade the sender; none is identity proof.
const (
	// AssuranceDMARCPass: an SPF or DKIM pass aligned with the From domain.
	AssuranceDMARCPass = "dmarc_pass"
	// AssuranceDMARCFail: the From domain publishes DMARC and nothing aligned
	// passed.
	AssuranceDMARCFail = "dmarc_fail"
	// AssuranceDMARCNone: the From domain publishes no DMARC record.
	AssuranceDMARCNone = "dmarc_none"
	// AssuranceDMARCTempError: a DNS failure prevented evaluation.
	AssuranceDMARCTempError = "dmarc_temperror"
	// AssuranceDMARCPermError: the From header or the DMARC record is
	// unusable.
	AssuranceDMARCPermError = "dmarc_permerror"
)

// Evaluation bounds.
const (
	evidenceTimeout   = 30 * time.Second
	maxDKIMSignatures = 8
)

// evidence is the sender evidence of one message.
type evidence struct {
	SPF   spfResult    `json:"spf"`
	DKIM  []dkimResult `json:"dkim,omitempty"`
	DMARC dmarcResult  `json:"dmarc"`
	ARC   arcRecord    `json:"arc"`
}

type spfResult struct {
	// Result is an RFC 7208 result: pass, fail, softfail, neutral, none,
	// temperror, permerror.
	Result string `json:"result"`
	// Domain is the checked identity's domain: MAIL FROM, or HELO for a null
	// reverse path.
	Domain string `json:"domain"`
	// Scope is "mailfrom" or "helo".
	Scope string `json:"scope"`
}

type dkimResult struct {
	// Result is pass, fail, temperror or permerror.
	Result     string `json:"result"`
	Domain     string `json:"domain"`
	Identifier string `json:"identifier,omitempty"`
}

type dmarcResult struct {
	// Result is pass, fail, none, temperror or permerror.
	Result     string `json:"result"`
	FromDomain string `json:"from_domain,omitempty"`
	// Policy is the applicable requested policy (p, or sp for a subdomain
	// covered by its organizational domain's record).
	Policy      string `json:"policy,omitempty"`
	SPFAligned  bool   `json:"spf_aligned"`
	DKIMAligned bool   `json:"dkim_aligned"`
}

// arcRecord preserves ARC header fields as found. ARC is not verified: no
// maintained Go verifier exists, so a sealed chain never raises assurance.
type arcRecord struct {
	Verified bool         `json:"verified"`
	Fields   []mime.Field `json:"fields,omitempty"`
}

// evidenceInput is what one SMTP transaction tells about its sender.
type evidenceInput struct {
	ip       net.IP
	helo     string
	mailFrom string
	// from is the RFC 5322.From addresses.
	from []mime.Address
	raw  []byte
	arc  []mime.Field
}

// evaluate runs SPF, DKIM and DMARC. DNS failures become temperror results,
// never errors: evidence is recorded either way.
func evaluate(ctx context.Context, r Resolver, in evidenceInput) evidence {
	ctx, cancel := context.WithTimeout(ctx, evidenceTimeout)
	defer cancel()
	ev := evidence{ARC: arcRecord{Fields: in.arc}}
	ev.SPF = checkSPF(ctx, r, in)
	ev.DKIM = checkDKIM(ctx, r, in.raw)
	ev.DMARC = checkDMARC(ctx, r, in.from, ev.SPF, ev.DKIM)
	return ev
}

func checkSPF(ctx context.Context, r Resolver, in evidenceInput) spfResult {
	res := spfResult{Scope: "mailfrom", Domain: domainOf(in.mailFrom)}
	if res.Domain == "" {
		res.Scope, res.Domain = "helo", strings.ToLower(in.helo)
	}
	result, _ := spf.CheckHostWithSender(in.ip, in.helo, in.mailFrom, spf.WithContext(ctx), spf.WithResolver(r))
	res.Result = string(result)
	return res
}

func checkDKIM(ctx context.Context, r Resolver, raw []byte) []dkimResult {
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		LookupTXT:        func(name string) ([]string, error) { return r.LookupTXT(ctx, name) },
		MaxVerifications: maxDKIMSignatures,
	})
	if err != nil && !errors.Is(err, dkim.ErrTooManySignatures) {
		return nil
	}
	out := make([]dkimResult, 0, len(verifs))
	for _, v := range verifs {
		res := dkimResult{Domain: strings.ToLower(v.Domain), Identifier: v.Identifier}
		switch {
		case v.Err == nil:
			res.Result = "pass"
		case dkim.IsTempFail(v.Err):
			res.Result = "temperror"
		case dkim.IsPermFail(v.Err):
			res.Result = "permerror"
		default:
			res.Result = "fail"
		}
		out = append(out, res)
	}
	return out
}

// checkDMARC evaluates RFC 7489 for the From domain: the record at the From
// domain, else at its organizational domain; pass when an SPF or DKIM pass
// aligns with the From domain under the record's alignment modes.
func checkDMARC(ctx context.Context, r Resolver, from []mime.Address, s spfResult, ds []dkimResult) dmarcResult {
	if len(from) != 1 || domainOf(from[0].Address) == "" {
		return dmarcResult{Result: "permerror"}
	}
	fromDomain := domainOf(from[0].Address)
	res := dmarcResult{FromDomain: fromDomain}
	lookup := &dmarc.LookupOptions{LookupTXT: func(name string) ([]string, error) { return r.LookupTXT(ctx, name) }}
	rec, err := dmarc.LookupWithOptions(fromDomain, lookup)
	org := orgDomain(fromDomain)
	usedOrg := false
	if errors.Is(err, dmarc.ErrNoPolicy) && org != fromDomain {
		rec, err = dmarc.LookupWithOptions(org, lookup)
		usedOrg = true
	}
	switch {
	case errors.Is(err, dmarc.ErrNoPolicy):
		res.Result = "none"
		return res
	case dmarc.IsTempFail(err):
		res.Result = "temperror"
		return res
	case err != nil:
		res.Result = "permerror"
		return res
	}
	res.Policy = string(rec.Policy)
	if usedOrg && rec.SubdomainPolicy != "" {
		res.Policy = string(rec.SubdomainPolicy)
	}
	res.SPFAligned = s.Result == string(spf.Pass) && aligned(s.Domain, fromDomain, rec.SPFAlignment)
	for _, d := range ds {
		if d.Result == "pass" && aligned(d.Domain, fromDomain, rec.DKIMAlignment) {
			res.DKIMAligned = true
		}
	}
	res.Result = "fail"
	if res.SPFAligned || res.DKIMAligned {
		res.Result = "pass"
	}
	return res
}

// aligned compares an authenticated domain with the From domain: equal in
// strict mode, same organizational domain in relaxed mode (the default).
func aligned(domain, from string, mode dmarc.AlignmentMode) bool {
	domain, from = strings.ToLower(strings.TrimSuffix(domain, ".")), strings.ToLower(strings.TrimSuffix(from, "."))
	if domain == "" {
		return false
	}
	if mode == dmarc.AlignmentStrict {
		return domain == from
	}
	return orgDomain(domain) == orgDomain(from)
}

// orgDomain is the organizational domain: the public suffix plus one label.
func orgDomain(d string) string {
	d = strings.ToLower(strings.TrimSuffix(d, "."))
	if org, err := publicsuffix.EffectiveTLDPlusOne(d); err == nil {
		return org
	}
	return d
}

func domainOf(addr string) string {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(addr[at+1:], "."))
}

// assurance maps the DMARC result to the source assurance.
func (e evidence) assurance() string {
	switch e.DMARC.Result {
	case "pass":
		return AssuranceDMARCPass
	case "fail":
		return AssuranceDMARCFail
	case "none":
		return AssuranceDMARCNone
	case "temperror":
		return AssuranceDMARCTempError
	}
	return AssuranceDMARCPermError
}

// authenticationResults formats the evidence as an RFC 8601
// Authentication-Results value under authservID.
func (e evidence) authenticationResults(authservID, mailFrom, helo string) string {
	results := []authres.Result{&authres.SPFResult{Value: authres.ResultValue(e.SPF.Result), From: mailFrom, Helo: helo}}
	if len(e.DKIM) == 0 {
		results = append(results, &authres.DKIMResult{Value: authres.ResultNone})
	}
	for _, d := range e.DKIM {
		results = append(results, &authres.DKIMResult{Value: authres.ResultValue(d.Result), Domain: d.Domain, Identifier: d.Identifier})
	}
	results = append(results, &authres.DMARCResult{Value: authres.ResultValue(e.DMARC.Result), From: e.DMARC.FromDomain})
	return authres.Format(authservID, results)
}
