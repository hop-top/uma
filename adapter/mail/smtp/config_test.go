package smtp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
)

func TestValidate(t *testing.T) {
	f := smtp.NewFactory(smtp.Options{})
	if issues := f.Validate(config(587).Extension()); len(issues) != 0 {
		t.Fatalf("valid configuration: %+v", issues)
	}
	for name, c := range map[string]struct {
		mut   func(*smtp.Config)
		field string
	}{
		"hostname":                 {func(c *smtp.Config) { c.Hostname = "mx example" }, "hostname"},
		"no recipients":            {func(c *smtp.Config) { c.Recipients = nil }, "recipients"},
		"recipient address":        {func(c *smtp.Config) { c.Recipients[0].Address = "Support <support@example.com>" }, "recipients[0].address"},
		"duplicate recipient":      {func(c *smtp.Config) { c.Recipients[1].Address = "support@EXAMPLE.com" }, "recipients[1].address"},
		"empty destination":        {func(c *smtp.Config) { c.Recipients[0].Destination = " " }, "recipients[0].destination"},
		"from":                     {func(c *smtp.Config) { c.From = "support" }, "from"},
		"subject injection":        {func(c *smtp.Config) { c.Subject = "x\r\nBcc: a@example.org" }, "subject"},
		"from name injection":      {func(c *smtp.Config) { c.FromName = "x\nBcc: a@example.org" }, "from_name"},
		"no relay port":            {func(c *smtp.Config) { c.Relay.StartTLSPort = 0 }, "relay"},
		"plaintext without opt-in": {func(c *smtp.Config) { c.Relay.StartTLSPort, c.Relay.PlainPort = 0, 2525 }, "relay.plain_port"},
		"relay host":               {func(c *smtp.Config) { c.Relay.Host = "" }, "relay.host"},
		"negative size":            {func(c *smtp.Config) { c.MaxMessageBytes = -1 }, "max_message_bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config(587)
			cfg.Recipients = append([]smtp.Recipient(nil), cfg.Recipients...)
			c.mut(&cfg)
			issues := f.Validate(cfg.Extension())
			if len(issues) == 0 || issues[0].Field != c.field {
				t.Fatalf("issues = %+v, want first on %s", issues, c.field)
			}
		})
	}
	t.Run("schema", func(t *testing.T) {
		x := config(587).Extension()
		x.Version = 2
		if issues := f.Validate(x); len(issues) != 1 || issues[0].Code != string(model.SchemaMismatch) {
			t.Fatalf("issues = %+v", issues)
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		x := config(587).Extension()
		var m map[string]any
		_ = json.Unmarshal(x.Data, &m)
		m["password"] = "nope"
		x.Data, _ = json.Marshal(m)
		if issues := f.Validate(x); len(issues) != 1 || !strings.HasPrefix(issues[0].Field, "configuration") {
			t.Fatalf("issues = %+v, want the configuration refused", issues)
		}
	})
}

func TestOpen(t *testing.T) {
	f := smtp.NewFactory(smtp.Options{})
	d := f.Describe()
	if d.Type != model.TypeSmtpMail || d.ContractMajor != adapter.ContractMajor || len(d.IdentityNamespaces) != 1 || d.IdentityNamespaces[0] != smtp.Namespace {
		t.Fatalf("descriptor = %+v", d)
	}
	ch := channel(channelA, config(587))
	bad := ch
	bad.Type = model.TypeImapMail
	if _, err := f.Open(context.Background(), bad, adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
		t.Errorf("Open(other type) = %v, want validation", err)
	}
	bad = ch
	bad.Configuration.Data = []byte(`{}`)
	if _, err := f.Open(context.Background(), bad, adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
		t.Errorf("Open(invalid configuration) = %v, want validation", err)
	}
	inst, err := f.Open(context.Background(), ch, adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	caps, err := inst.Capabilities(context.Background())
	if err != nil || len(caps) != 4 || caps[0].Available || caps[1].Available || caps[2].Available || caps[3].Available {
		t.Errorf("capabilities without listener or credentials = %+v, %v; want all unavailable", caps, err)
	}
	if err := inst.(adapter.Receiver).Run(context.Background(), &sink{}); !model.IsCode(err, model.Unavailable) {
		t.Errorf("Run without listener = %v, want unavailable", err)
	}
	if _, err := inst.(adapter.Sender).Send(context.Background(), textRequest(opA, "hi")); !model.IsCode(err, model.Unavailable) {
		t.Errorf("Send without credentials = %v, want unavailable", err)
	}
}
