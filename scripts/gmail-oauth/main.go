// Command gmail-oauth obtains Gmail API tokens for recording the Gmail
// adapter's cassettes. It uses only the standard library.
//
//	gmail-oauth consent   one-time browser consent; prints a refresh token
//	gmail-oauth token     exchanges the refresh token for an access token
//
// Both read the OAuth client of a Google Cloud "Desktop app" from
// GMAIL_OAUTH_CLIENT_ID and GMAIL_OAUTH_CLIENT_SECRET; token also reads
// GMAIL_REFRESH_TOKEN. Tokens are written to standard output only, never to
// a file, so they can be captured straight into the recording shell:
//
//	export GMAIL_ACCESS_TOKEN="$(go run ./scripts/gmail-oauth token)"
//
// consent listens on a random 127.0.0.1 port for Google's redirect (the
// loopback flow for desktop clients), with PKCE and a state check. The
// default scopes are the ones the adapter declares, gmail.readonly,
// gmail.send and gmail.modify; -scopes overrides them.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	authURL         = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenURL        = "https://oauth2.googleapis.com/token" // #nosec G101 -- public endpoint, not a credential
	defaultScopes   = "https://www.googleapis.com/auth/gmail.readonly https://www.googleapis.com/auth/gmail.send https://www.googleapis.com/auth/gmail.modify"
	envClientID     = "GMAIL_OAUTH_CLIENT_ID"
	envClientSecret = "GMAIL_OAUTH_CLIENT_SECRET" // #nosec G101 -- environment variable name, not a credential
	envRefreshToken = "GMAIL_REFRESH_TOKEN"       // #nosec G101 -- environment variable name, not a credential
)

const usage = `usage: gmail-oauth consent [-scopes "scope ..."] [-login-hint address]
       gmail-oauth token

consent  run the one-time browser consent and print a refresh token
token    exchange GMAIL_REFRESH_TOKEN for an access token and print it

Both read GMAIL_OAUTH_CLIENT_ID and GMAIL_OAUTH_CLIENT_SECRET.
`

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gmail-oauth:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return errors.New("missing command")
	}
	c := client{
		id:       os.Getenv(envClientID),
		secret:   os.Getenv(envClientSecret),
		tokenURL: tokenURL,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
	if c.id == "" || c.secret == "" {
		return fmt.Errorf("set %s and %s", envClientID, envClientSecret)
	}
	switch args[0] {
	case "consent":
		fs := flag.NewFlagSet("consent", flag.ContinueOnError)
		fs.SetOutput(stderr)
		scopes := fs.String("scopes", defaultScopes, "space-separated OAuth scopes")
		hint := fs.String("login-hint", "", "address of the test mailbox, preselected on the consent screen")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		tok, err := c.consent(ctx, *scopes, *hint, stderr)
		if err != nil {
			return err
		}
		if tok.RefreshToken == "" {
			return errors.New("no refresh token in the answer; revoke the app's access in the Google account and run consent again")
		}
		_, err = fmt.Fprintln(stdout, tok.RefreshToken)
		return err
	case "token":
		rt := os.Getenv(envRefreshToken)
		if rt == "" {
			return fmt.Errorf("set %s", envRefreshToken)
		}
		tok, err := c.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, tok.AccessToken)
		return err
	default:
		_, _ = io.WriteString(stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type client struct {
	id, secret string
	tokenURL   string
	http       *http.Client
}

type token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// consent prints the authorization URL, waits for the loopback redirect and
// exchanges the code.
func (c client) consent(ctx context.Context, scopes, hint string, stderr io.Writer) (token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return token{}, err
	}
	redirect := "http://" + ln.Addr().String()
	state, verifier := randomString(), randomString()
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {c.id},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"scope":                 {scopes},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	if hint != "" {
		q.Set("login_hint", hint)
	}
	_, _ = fmt.Fprintf(stderr, "Open this URL in a browser signed in as the test mailbox:\n\n%s?%s\n\nWaiting for the redirect to %s ...\n", authURL, q.Encode(), redirect)

	codes := make(chan string, 1)
	fails := make(chan error, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			code, err := callback(r.URL.Query(), state)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				select {
				case fails <- err:
				default:
				}
				return
			}
			_, _ = io.WriteString(w, "Consent received. You can close this tab.\n")
			select {
			case codes <- code:
			default:
			}
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	select {
	case <-ctx.Done():
		return token{}, fmt.Errorf("waiting for consent: %w", ctx.Err())
	case err := <-fails:
		return token{}, err
	case code := <-codes:
		return c.exchange(ctx, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"redirect_uri":  {redirect},
			"code_verifier": {verifier},
		})
	}
}

// callback validates the redirect query and returns the authorization code.
func callback(q url.Values, state string) (string, error) {
	if e := q.Get("error"); e != "" {
		return "", fmt.Errorf("consent refused: %s", e)
	}
	if q.Get("state") != state {
		return "", errors.New("state mismatch; start consent again")
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("redirect carries no code")
	}
	return code, nil
}

// exchange posts form to the token endpoint with the client credentials.
func (c client) exchange(ctx context.Context, form url.Values) (token, error) {
	form.Set("client_id", c.id)
	form.Set("client_secret", c.secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return token{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return token{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "invalid_grant" {
			return token{}, fmt.Errorf("token endpoint: invalid_grant (%s); a refresh token of an app in testing mode expires after 7 days: run consent again", e.Description)
		}
		return token{}, fmt.Errorf("token endpoint: %s: %s %s", resp.Status, e.Error, e.Description)
	}
	var tok token
	if err := json.Unmarshal(body, &tok); err != nil {
		return token{}, fmt.Errorf("token endpoint answer: %w", err)
	}
	if tok.AccessToken == "" {
		return token{}, errors.New("token endpoint answer has no access token")
	}
	return tok, nil
}

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return base64.RawURLEncoding.EncodeToString(b)
}
