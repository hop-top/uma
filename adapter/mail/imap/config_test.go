package imap_test

import (
	"context"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
)

func validConfig() imap.Config {
	return imap.Config{
		Address:    mailUser,
		IMAP:       imap.Server{Host: "imap.example.com", TLSPort: 993},
		Submission: imap.Server{Host: "smtp.example.com", StartTLSPort: 587},
	}
}

func TestConfigValidation(t *testing.T) {
	f := imap.NewFactory(imap.Options{})
	if issues := f.Validate(validConfig().Extension()); len(issues) != 0 {
		t.Fatalf("valid configuration: %+v", issues)
	}
	for _, c := range []struct {
		field string
		edit  func(*imap.Config)
	}{
		{"address", func(c *imap.Config) { c.Address = "Desk <desk@example.com>" }},
		{"imap", func(c *imap.Config) { c.IMAP = imap.Server{Host: "imap.example.com"} }},
		{"imap.plain_port", func(c *imap.Config) { c.IMAP = imap.Server{Host: "127.0.0.1", PlainPort: 1143} }},
		{"submission.host", func(c *imap.Config) { c.Submission.Host = "bad host" }},
		{"idle_restart_ms", func(c *imap.Config) { c.IdleRestartMS = 29*60_000 + 1 }},
		{"sent.saved_by_provider", func(c *imap.Config) { c.Sent = imap.Sent{SavedByProvider: true} }},
		{"subject", func(c *imap.Config) { c.Subject = "two\nlines" }},
		{"reconnect_margin_ms", func(c *imap.Config) { c.ReconnectMarginMS = -1 }},
		{"watch_mailboxes", func(c *imap.Config) { c.WatchMailboxes = []string{"Archive", "Archive"} }},
		{"watch_mailboxes", func(c *imap.Config) { c.WatchMailboxes = []string{"INBOX"} }},
		{"watch_mailboxes", func(c *imap.Config) { c.WatchMailboxes = []string{""} }},
		{"state_sync_interval_ms", func(c *imap.Config) { c.StateSyncIntervalMS = -1 }},
	} {
		cfg := validConfig()
		c.edit(&cfg)
		issues := f.Validate(cfg.Extension())
		if !slices.ContainsFunc(issues, func(i model.ValidationIssue) bool { return i.Field == c.field }) {
			t.Errorf("%s: issues %+v", c.field, issues)
		}
	}
	ok := validConfig()
	ok.WatchMailboxes, ok.StateSyncIntervalMS = []string{"Archive", "Projects/2026"}, 30_000
	if issues := f.Validate(ok.Extension()); len(issues) != 0 {
		t.Errorf("watched mailboxes: %+v", issues)
	}
	if issues := f.Validate(model.Extension{Name: imap.ConfigSchema, Version: 2}); len(issues) != 1 || issues[0].Code != string(model.SchemaMismatch) {
		t.Errorf("version 2: %+v", issues)
	}
}

func TestOpenAndCapabilities(t *testing.T) {
	f := imap.NewFactory(imap.Options{})
	ch := channel(channelA, validConfig())
	ch.Type = model.TypeSmtpMail
	if _, err := f.Open(context.Background(), ch, adapter.Dependencies{}); model.CodeOf(err) != model.Validation {
		t.Fatalf("Open with another type = %v", err)
	}
	inst, err := f.Open(context.Background(), channel(channelA, validConfig()), adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	statuses, _ := inst.Capabilities(context.Background())
	for _, s := range statuses {
		if s.Available {
			t.Errorf("%s available without a credentials port", s.Name)
		}
	}
	d := f.Describe()
	if d.Type != model.TypeImapMail || !slices.Equal(d.RequiredPermissions, []string{imap.PurposeIMAP, imap.PurposeSubmission}) {
		t.Errorf("descriptor %+v", d)
	}
}
