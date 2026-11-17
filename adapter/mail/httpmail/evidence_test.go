package httpmail_test

import (
	"fmt"
	"strings"
	"testing"

	"hop.top/uma/adapter/mail/httpmail"
	"hop.top/uma/adapter/mail/smtp"
)

// trustedConfig trusts the stamp of mx.example.com, in a message header
// field or in the JSON member authentication_results.
func trustedConfig() httpmail.Config {
	cfg := rawConfig()
	cfg.Evidence = httpmail.Evidence{TrustedAuthservID: "mx.example.com", AuthenticationResultsField: "authentication_results"}
	return cfg
}

// stamped is a message from alice@peers.test with the given
// Authentication-Results header fields on top.
func stamped(ar ...string) string {
	var b strings.Builder
	for _, v := range ar {
		b.WriteString("Authentication-Results: " + v + "\n")
	}
	b.WriteString("From: Alice <alice@peers.test>\nTo: support@example.com\nMessage-ID: <e1@peers.test>\n\nhello\n")
	return b.String()
}

const passAll = "mx.example.com 1; spf=pass smtp.mailfrom=peers.test; dkim=pass header.d=peers.test header.i=@peers.test; dmarc=pass header.from=peers.test" // #nosec G101 -- Authentication-Results fixture

func TestTrustedStampBecomesEvidence(t *testing.T) {
	r := newRig(t, trustedConfig())
	wantStatus(t, r.postRaw(stamped(passAll)), 200)
	in := only(t, r)
	if in.Envelope.Assurance != smtp.AssuranceDMARCPass {
		t.Errorf("assurance %q", in.Envelope.Assurance)
	}
	ev := sub(t, payloadOf(t, in), "evidence").(map[string]any)
	if ev["trusted"] != true || ev["authserv_id"] != "mx.example.com" {
		t.Errorf("evidence %v", ev)
	}
	if sub(t, ev, "spf", "result") != "pass" || sub(t, ev, "dmarc", "result") != "pass" || sub(t, ev, "dmarc", "from_domain") != "peers.test" {
		t.Errorf("evidence %v", ev)
	}
	dkim := ev["dkim"].([]any)
	if len(dkim) != 1 || sub(t, dkim[0].(map[string]any), "result") != "pass" || sub(t, dkim[0].(map[string]any), "domain") != "peers.test" {
		t.Errorf("dkim %v", dkim)
	}
}

// The assurance constants are the ones package smtp derives from its own
// evaluation, so a consumer reads both adapters alike.
func TestAssuranceValuesMatchSMTP(t *testing.T) {
	for result, want := range map[string]string{
		"pass": smtp.AssuranceDMARCPass, "fail": smtp.AssuranceDMARCFail, "none": smtp.AssuranceDMARCNone,
		"temperror": smtp.AssuranceDMARCTempError, "permerror": smtp.AssuranceDMARCPermError,
	} {
		t.Run(result, func(t *testing.T) {
			r := newRig(t, trustedConfig())
			wantStatus(t, r.postRaw(stamped("mx.example.com; dmarc="+result+" header.from=peers.test")), 200)
			if got := only(t, r).Envelope.Assurance; got != want {
				t.Errorf("assurance %q, want %q", got, want)
			}
		})
	}
}

func TestStampOfAnotherHostIsAClaim(t *testing.T) {
	for name, msg := range map[string]string{
		"other id":         stamped("evil.example; dmarc=pass header.from=peers.test"),
		"id as a prefix":   stamped("mx.example.com.evil.test; dmarc=pass header.from=peers.test"),
		"id in a comment":  stamped("evil.example (mx.example.com); dmarc=pass header.from=peers.test"),
		"id in a result":   stamped("evil.example; dmarc=pass header.from=peers.test reason=mx.example.com"),
		"no authserv id":   stamped("; dmarc=pass header.from=peers.test"),
		"malformed":        stamped("mx.example.com 2; dmarc=pass header.from=peers.test"),
		"malformed result": stamped("mx.example.com; dmarc pass"),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, trustedConfig())
			wantStatus(t, r.postRaw(msg), 200)
			in := only(t, r)
			if in.Envelope.Assurance != "unverified" {
				t.Errorf("assurance %q", in.Envelope.Assurance)
			}
			ev := sub(t, payloadOf(t, in), "evidence").(map[string]any)
			if ev["trusted"] != false || ev["dmarc"] != nil || ev["spf"] != nil || ev["authserv_id"] != nil {
				t.Errorf("evidence %v", ev)
			}
			if got, _ := sub(t, ev, "claimed", "authentication_results").([]any); len(got) != 1 {
				t.Errorf("the value is not recorded as a claim: %v", ev)
			}
		})
	}
}

func TestStampIsNothingWithoutADeclaration(t *testing.T) {
	r := newRig(t, rawConfig())
	wantStatus(t, r.postRaw(stamped(passAll)), 200)
	in := only(t, r)
	if in.Envelope.Assurance != "unverified" || sub(t, payloadOf(t, in), "evidence", "trusted") != false {
		t.Errorf("assurance %q, evidence %v", in.Envelope.Assurance, sub(t, payloadOf(t, in), "evidence"))
	}
}

func TestTrustedIDMatchesIgnoringCase(t *testing.T) {
	r := newRig(t, trustedConfig())
	wantStatus(t, r.postRaw(stamped("MX.Example.COM; dmarc=pass header.from=peers.test")), 200)
	if got := only(t, r).Envelope.Assurance; got != smtp.AssuranceDMARCPass {
		t.Errorf("assurance %q", got)
	}
}

// The relay prepends its own stamp: a stamp of its id below another host's
// is read, and of two of its own the topmost, the later, decides.
func TestTopmostStampOfTheTrustedIDDecides(t *testing.T) {
	for name, tc := range map[string]struct {
		ar   []string
		want string
	}{
		"foreign on top": {[]string{"evil.example; dmarc=pass header.from=peers.test", "mx.example.com; dmarc=fail header.from=peers.test"}, smtp.AssuranceDMARCFail},
		"own on top":     {[]string{"mx.example.com; dmarc=fail header.from=peers.test", "mx.example.com; dmarc=pass header.from=peers.test"}, smtp.AssuranceDMARCFail},
		"own below":      {[]string{"mx.example.com; dmarc=pass header.from=peers.test", "mx.example.com; dmarc=fail header.from=peers.test"}, smtp.AssuranceDMARCPass},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, trustedConfig())
			wantStatus(t, r.postRaw(stamped(tc.ar...)), 200)
			if got := only(t, r).Envelope.Assurance; got != tc.want {
				t.Errorf("assurance %q, want %q", got, tc.want)
			}
		})
	}
}

// Only the topmost stamp of the trusted id is read: a lower one, which the
// sender or an earlier hop may have supplied, adds nothing.
func TestLowerStampsAreNotMerged(t *testing.T) {
	r := newRig(t, trustedConfig())
	wantStatus(t, r.postRaw(stamped(
		"mx.example.com; spf=fail smtp.mailfrom=peers.test",
		"mx.example.com; dmarc=pass header.from=peers.test; dkim=pass header.d=peers.test")), 200)
	in := only(t, r)
	if in.Envelope.Assurance != "unverified" {
		t.Errorf("assurance %q", in.Envelope.Assurance)
	}
	ev := sub(t, payloadOf(t, in), "evidence").(map[string]any)
	if ev["dmarc"] != nil || ev["dkim"] != nil || sub(t, ev, "spf", "result") != "fail" {
		t.Errorf("evidence %v", ev)
	}
}

func TestDKIMResultsAreBounded(t *testing.T) {
	r := newRig(t, trustedConfig())
	ar := "mx.example.com; " + strings.Repeat("dkim=pass header.d=peers.test; ", 12) + "dmarc=pass header.from=peers.test"
	wantStatus(t, r.postRaw(stamped(ar)), 200)
	if n := len(sub(t, payloadOf(t, only(t, r)), "evidence", "dkim").([]any)); n != 8 {
		t.Errorf("%d dkim results", n)
	}
}

// A result for another From domain says nothing about this sender.
func TestDMARCResultMustBeAboutTheFromDomain(t *testing.T) {
	for name, ar := range map[string]string{
		"other domain": "mx.example.com; dmarc=pass header.from=evil.test",
		"no domain":    "mx.example.com; dmarc=pass",
		"subdomain":    "mx.example.com; dmarc=pass header.from=sub.peers.test",
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, trustedConfig())
			wantStatus(t, r.postRaw(stamped(ar)), 200)
			in := only(t, r)
			if in.Envelope.Assurance != "unverified" {
				t.Errorf("assurance %q", in.Envelope.Assurance)
			}
			if sub(t, payloadOf(t, in), "evidence", "dmarc") != nil {
				t.Errorf("a result about another domain is recorded for this sender")
			}
		})
	}
}

func TestFromDomainComparesIgnoringCaseAndRootDot(t *testing.T) {
	for _, from := range []string{"PEERS.Test", "peers.test."} {
		r := newRig(t, trustedConfig())
		wantStatus(t, r.postRaw(stamped("mx.example.com; dmarc=pass header.from="+from)), 200)
		if got := only(t, r).Envelope.Assurance; got != smtp.AssuranceDMARCPass {
			t.Errorf("header.from=%s: assurance %q", from, got)
		}
	}
}

func TestStampWithoutDMARCRecordsResultsButNoAssurance(t *testing.T) {
	r := newRig(t, trustedConfig())
	wantStatus(t, r.postRaw(stamped("mx.example.com; spf=pass smtp.mailfrom=peers.test")), 200)
	in := only(t, r)
	if in.Envelope.Assurance != "unverified" {
		t.Errorf("assurance %q", in.Envelope.Assurance)
	}
	ev := sub(t, payloadOf(t, in), "evidence").(map[string]any)
	if ev["trusted"] != true || sub(t, ev, "spf", "result") != "pass" || ev["dmarc"] != nil {
		t.Errorf("evidence %v", ev)
	}
}

func TestUnknownDMARCValueGivesNoAssurance(t *testing.T) {
	r := newRig(t, trustedConfig())
	wantStatus(t, r.postRaw(stamped("mx.example.com; dmarc=bestguess header.from=peers.test")), 200)
	if got := only(t, r).Envelope.Assurance; got != "unverified" {
		t.Errorf("assurance %q", got)
	}
}

func TestTrustedStampLookupIsNotBoundedByTheClaimCap(t *testing.T) {
	r := newRig(t, trustedConfig())
	var ar []string
	for range 12 {
		ar = append(ar, "evil.example; dmarc=pass header.from=peers.test")
	}
	ar = append(ar, "mx.example.com; dmarc=fail header.from=peers.test")
	wantStatus(t, r.postRaw(stamped(ar...)), 200)
	if got := only(t, r).Envelope.Assurance; got != smtp.AssuranceDMARCFail {
		t.Errorf("assurance %q", got)
	}
}

func TestJSONTrustedStampMember(t *testing.T) {
	r := newRig(t, trustedConfig())
	wantStatus(t, r.postJSON(fmt.Sprintf(`{"from":"alice@peers.test","to":"support@example.com","body":"x","authentication_results":%q}`, passAll)), 200)
	in := only(t, r)
	if in.Envelope.Assurance != smtp.AssuranceDMARCPass {
		t.Errorf("assurance %q", in.Envelope.Assurance)
	}
	if sub(t, payloadOf(t, in), "evidence", "authserv_id") != "mx.example.com" {
		t.Errorf("evidence %v", sub(t, payloadOf(t, in), "evidence"))
	}
}

func TestJSONStampMemberBoundToTheFromDomain(t *testing.T) {
	r := newRig(t, trustedConfig())
	wantStatus(t, r.postJSON(`{"from":"ceo@example.com","to":"support@example.com","body":"x","authentication_results":"mx.example.com; dmarc=pass header.from=peers.test"}`), 200)
	if got := only(t, r).Envelope.Assurance; got != "unverified" {
		t.Errorf("assurance %q: a pass for peers.test vouches for no one at example.com", got)
	}
}

func TestJSONStampMemberOfAnotherHostIsAClaim(t *testing.T) {
	r := newRig(t, trustedConfig())
	wantStatus(t, r.postJSON(`{"from":"alice@peers.test","to":"support@example.com","body":"x","authentication_results":"evil.example; dmarc=pass header.from=peers.test"}`), 200)
	in := only(t, r)
	if in.Envelope.Assurance != "unverified" {
		t.Errorf("assurance %q", in.Envelope.Assurance)
	}
	if got, _ := sub(t, payloadOf(t, in), "evidence", "claimed", "authentication_results").([]any); len(got) != 1 {
		t.Errorf("claimed %v", got)
	}
}

func TestJSONStampMemberOfWrongTypeIsRefused(t *testing.T) {
	r := newRig(t, trustedConfig())
	wantStatus(t, r.postJSON(`{"from":"alice@peers.test","to":"support@example.com","body":"x","authentication_results":["mx.example.com; dmarc=pass header.from=peers.test"]}`), 400)
}
