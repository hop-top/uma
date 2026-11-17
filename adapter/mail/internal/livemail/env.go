package livemail

import (
	"crypto/x509"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Variables the live tests read. UMA_LIVE_MAIL=1 opts in; the others name
// endpoints. A group none of whose variables is set is skipped (unless
// UMA_REQUIRE_LIVE_MAIL=1, which makes it a failure, for CI); a group with
// some of them set must have all its required ones.
const (
	VarOptIn   = "UMA_LIVE_MAIL"
	VarRequire = "UMA_REQUIRE_LIVE_MAIL"
	VarCA      = "UMA_LIVE_CA"
)

// Group is a set of endpoints a test needs.
type Group int

// The groups.
const (
	// IMAP is an IMAP account: UMA_LIVE_IMAP_HOST, _USER, _PASS and at least
	// one of _TLS_PORT and _STARTTLS_PORT.
	IMAP Group = iota
	// Submission is an SMTP submission server that catches what it is given
	// and exposes it over HTTP (Mailpit): UMA_LIVE_SMTP_HOST, _USER, _PASS,
	// _API and one of _STARTTLS_PORT and _PLAIN_PORT.
	Submission
	// MTA is a sending MTA container started by up.sh:
	// UMA_LIVE_MTA_CONTAINER and UMA_LIVE_MTA_PORT.
	MTA
)

// Env is the endpoints of the live servers.
type Env struct {
	// Roots verifies the servers' certificates; nil uses the system roots.
	Roots *x509.CertPool

	IMAPHost         string
	IMAPTLSPort      int
	IMAPStartTLSPort int
	IMAPUser         string
	IMAPPass         string

	SMTPHost         string
	SMTPStartTLSPort int
	SMTPPlainPort    int
	SMTPUser         string
	SMTPPass         string
	SMTPAPI          string // base URL of the catcher's HTTP API

	MTAContainer string
	MTAPort      int    // the MTA's published SMTP port on 127.0.0.1
	MTARelayHost string // how the MTA reaches the test host
}

// Require returns the environment for the groups, or skips the test.
func Require(t testing.TB, groups ...Group) Env {
	t.Helper()
	if os.Getenv(VarOptIn) != "1" {
		t.Skipf("live mail test: set %s=1 and the UMA_LIVE_* endpoints (docs/recording/imap-smtp.md)", VarOptIn)
	}
	e := Env{
		IMAPHost: os.Getenv("UMA_LIVE_IMAP_HOST"), IMAPUser: os.Getenv("UMA_LIVE_IMAP_USER"), IMAPPass: os.Getenv("UMA_LIVE_IMAP_PASS"),
		SMTPHost: os.Getenv("UMA_LIVE_SMTP_HOST"), SMTPUser: os.Getenv("UMA_LIVE_SMTP_USER"), SMTPPass: os.Getenv("UMA_LIVE_SMTP_PASS"),
		SMTPAPI:      strings.TrimRight(os.Getenv("UMA_LIVE_SMTP_API"), "/"),
		MTAContainer: os.Getenv("UMA_LIVE_MTA_CONTAINER"),
		MTARelayHost: os.Getenv("UMA_LIVE_MTA_RELAY_HOST"),
	}
	if e.MTARelayHost == "" {
		e.MTARelayHost = "host.docker.internal"
	}
	if ca := os.Getenv(VarCA); ca != "" {
		pem, err := os.ReadFile(ca) // #nosec G304 G703 -- a path the operator named for the test run
		if err != nil {
			t.Fatalf("%s: %v", VarCA, err)
		}
		e.Roots = x509.NewCertPool()
		if !e.Roots.AppendCertsFromPEM(pem) {
			t.Fatalf("%s: no certificate in %s", VarCA, ca)
		}
	}
	e.IMAPTLSPort = port(t, "UMA_LIVE_IMAP_TLS_PORT")
	e.IMAPStartTLSPort = port(t, "UMA_LIVE_IMAP_STARTTLS_PORT")
	e.SMTPStartTLSPort = port(t, "UMA_LIVE_SMTP_STARTTLS_PORT")
	e.SMTPPlainPort = port(t, "UMA_LIVE_SMTP_PLAIN_PORT")
	e.MTAPort = port(t, "UMA_LIVE_MTA_PORT")

	for _, g := range groups {
		var set, missing []string
		add := func(name string, ok bool) {
			if ok {
				set = append(set, name)
			} else {
				missing = append(missing, name)
			}
		}
		switch g {
		case IMAP:
			add("UMA_LIVE_IMAP_HOST", e.IMAPHost != "")
			add("UMA_LIVE_IMAP_USER", e.IMAPUser != "")
			add("UMA_LIVE_IMAP_PASS", e.IMAPPass != "")
			add("UMA_LIVE_IMAP_TLS_PORT or UMA_LIVE_IMAP_STARTTLS_PORT", e.IMAPTLSPort != 0 || e.IMAPStartTLSPort != 0)
		case Submission:
			add("UMA_LIVE_SMTP_HOST", e.SMTPHost != "")
			add("UMA_LIVE_SMTP_USER", e.SMTPUser != "")
			add("UMA_LIVE_SMTP_PASS", e.SMTPPass != "")
			add("UMA_LIVE_SMTP_API", e.SMTPAPI != "")
			add("UMA_LIVE_SMTP_STARTTLS_PORT or UMA_LIVE_SMTP_PLAIN_PORT", e.SMTPStartTLSPort != 0 || e.SMTPPlainPort != 0)
		case MTA:
			add("UMA_LIVE_MTA_CONTAINER", e.MTAContainer != "")
			add("UMA_LIVE_MTA_PORT", e.MTAPort != 0)
		}
		switch {
		case len(missing) == 0:
		case len(set) == 0 && os.Getenv(VarRequire) != "1":
			t.Skipf("live mail test: no %s endpoint (%s unset)", groupName(g), strings.Join(missing, ", "))
		default:
			t.Fatalf("live mail test: %s endpoint incomplete: %s unset", groupName(g), strings.Join(missing, ", "))
		}
	}
	return e
}

func groupName(g Group) string {
	switch g {
	case IMAP:
		return "IMAP"
	case Submission:
		return "submission"
	default:
		return "MTA"
	}
}

func port(t testing.TB, name string) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 65535 {
		t.Fatalf("%s=%q is not a port", name, v)
	}
	return n
}

// Token returns a string unique to this call, for subjects, Message-IDs and
// mailbox names, so concurrent and repeated runs never see each other's mail.
func Token(t testing.TB) string {
	t.Helper()
	return fmt.Sprintf("%d-%x", os.Getpid(), seq.Add(1))
}

// seq starts at the process's start time, so tokens differ between runs.
var seq = func() *atomic.Uint64 {
	v := new(atomic.Uint64)
	v.Store(uint64(time.Now().UnixNano()))
	return v
}()

// HasSubmission reports whether the environment names a submission catcher.
func (e Env) HasSubmission() bool {
	return e.SMTPHost != "" && e.SMTPAPI != "" && (e.SMTPStartTLSPort != 0 || e.SMTPPlainPort != 0)
}
