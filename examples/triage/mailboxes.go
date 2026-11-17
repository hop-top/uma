package main

import (
	"context"
	"encoding/json"
	"os"

	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Credential references the channels carry; the host's credential port
// resolves them.
const (
	gmailRef = "gmail"
	imapRef  = "imap"
)

// mailboxesFromEnv describes the mailboxes the environment names.
func mailboxesFromEnv() []Mailbox {
	var out []Mailbox
	if addr := os.Getenv("GMAIL_ADDRESS"); addr != "" {
		out = append(out, gmailMailbox(addr))
	}
	if addr := os.Getenv("IMAP_ADDRESS"); addr != "" {
		out = append(out, imapMailbox(addr, os.Getenv("IMAP_HOST")))
	}
	return out
}

// gmailMailbox follows the mailbox by history polling; no Pub/Sub topic.
func gmailMailbox(addr string) Mailbox {
	data, _ := json.Marshal(google.Config{User: addr, Address: addr}) // plain struct: cannot fail
	return Mailbox{
		Name: "gmail", Type: model.TypeGoogleMail, CredentialRef: gmailRef, Destination: addr,
		Configuration: model.Extension{Name: google.ConfigSchema, Version: google.ConfigVersion, Data: data},
	}
}

// imapMailbox follows INBOX over implicit TLS. The state verbs need no
// submission, but the adapter's configuration names a server for it.
func imapMailbox(addr, host string) Mailbox {
	return Mailbox{
		Name: "imap", Type: model.TypeImapMail, CredentialRef: imapRef, Destination: addr,
		Configuration: imap.Config{
			Address:        addr,
			IMAP:           imap.Server{Host: host, TLSPort: 993},
			Submission:     imap.Server{Host: host, StartTLSPort: 587},
			WatchMailboxes: []string{"Archive", "Trash"},
		}.Extension(),
	}
}

// secrets is the host's credential port. Both adapters ask again on every
// connection, so a token renewed in the environment's source is picked up.
type secrets struct{ gmailToken func() string }

func secretsFromEnv() *secrets {
	return &secrets{gmailToken: func() string { return os.Getenv("GMAIL_ACCESS_TOKEN") }}
}

// Resolve implements ports.Credentials.
func (s *secrets) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	switch {
	case r.Reference == gmailRef && r.Purpose == google.PurposeGmail && r.Accepts(ports.KindOAuth2):
		return ports.OAuth2{User: os.Getenv("GMAIL_ADDRESS"), AccessToken: s.gmailToken()}, nil
	case r.Reference == imapRef && (r.Purpose == imap.PurposeIMAP || r.Purpose == imap.PurposeSubmission) && r.Accepts(ports.KindPassword):
		user := os.Getenv("IMAP_USER")
		if user == "" {
			user = os.Getenv("IMAP_ADDRESS")
		}
		return ports.Password{User: user, Secret: []byte(os.Getenv("IMAP_PASSWORD"))}, nil
	}
	return nil, model.NewError(model.Unsupported, "no credential for this request")
}

var _ ports.Credentials = (*secrets)(nil)
