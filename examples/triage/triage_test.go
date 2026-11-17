package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

// The loop runs offline against the fake provider's state variants: the
// labels variant stands in for Gmail (labels, stable references) and the
// folders variant for IMAP (one folder, a new reference per move). Both
// declare every state capability with account scope.

type secret struct{}

func (secret) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return ports.Secret{Bytes: []byte("test")}, nil
}

const tenant model.TenantID = "triage-test"

var mails = []struct{ id, from, text string }{
	{"n1", "digest@newsletter.example", "This week's links. Unsubscribe any time."},
	{"u1", "boss@corp.example", "URGENT: sign the contract today"},
	{"s1", "promo@spam.example", "You have won a prize"},
	{"p1", "friend@corp.example", "Lunch on Friday?"},
}

func startHost(t *testing.T) (*Host, map[string]*fakeprovider.Factory) {
	t.Helper()
	facs := map[string]*fakeprovider.Factory{
		"gmail": fakeprovider.NewStateLabels(),
		"imap":  fakeprovider.NewStateFolders(),
	}
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0))
	var boxes []Mailbox
	for _, name := range []string{"gmail", "imap"} {
		boxes = append(boxes, Mailbox{
			Name: name, Type: map[string]model.TypeKey{"gmail": fakeprovider.TypeStateLabels, "imap": fakeprovider.TypeStateFolders}[name],
			Configuration: facs[name].Config(), CredentialRef: name, Destination: "support",
		})
	}
	h, err := Start(t.Context(), Config{
		Tenant: tenant, Mailboxes: boxes,
		Adapters:    []adapter.Factory{facs["gmail"], facs["imap"]},
		Credentials: secret{}, Clock: clock, IDs: portstest.NewSequenceIDs(77),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return h, facs
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTriageLoopOnTwoMailboxShapes(t *testing.T) {
	h, facs := startHost(t)
	ctx := t.Context()
	for name, f := range facs {
		for _, m := range mails {
			f.Script(h.Channel(name)).Push(fakeprovider.MailIngress(h.Channel(name), fakeprovider.Email{
				EventID: name + "-" + m.id, Destination: "support", From: m.from, Text: m.text,
				Headers:    model.EmailHeaders{MessageID: "<" + m.id + "@" + name + ".example>"},
				OccurredAt: time.Unix(1_700_000_000, 0),
			}))
		}
	}
	for name := range facs {
		eventually(t, name+" mail fetched", func() bool {
			cs, err := h.Conversations(ctx, name)
			return err == nil && len(cs) == len(mails)
		})
	}

	outcomes, err := h.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Per mailbox: archive the newsletter, flag the urgent mail, trash the
	// junk, leave the rest.
	if len(outcomes) != 6 {
		t.Fatalf("queued %d changes, want 6: %+v", len(outcomes), outcomes)
	}
	for _, o := range outcomes {
		if o.Failure != nil || o.State == model.Queued {
			t.Errorf("%s %s: state %q failure %v", o.Mailbox, o.Action.Verb, o.State, o.Failure)
		}
	}

	for name, f := range facs {
		item := func(id string) fakeprovider.Item {
			it, ok := f.Script(h.Channel(name)).Item("<" + id + "@" + name + ".example>")
			if !ok {
				t.Fatalf("%s: provider holds no item %s", name, id)
			}
			return it
		}
		if !item("n1").Archived || item("n1").Trashed {
			t.Errorf("%s: newsletter not just archived: %+v", name, item("n1"))
		}
		if !slices.Contains(item("u1").Flags, model.FlagImportant) || item("u1").Archived {
			t.Errorf("%s: urgent mail not just flagged important: %+v", name, item("u1"))
		}
		if !item("s1").Trashed {
			t.Errorf("%s: junk not trashed: %+v", name, item("s1"))
		}
		if p := item("p1"); p.Archived || p.Trashed || len(p.Flags) > 0 {
			t.Errorf("%s: plain mail touched: %+v", name, p)
		}

		// uma holds what the provider answered, apart from its own status.
		convs, err := h.Conversations(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		var archived, trashed, flagged int
		for _, c := range convs {
			if c.Status != convs[0].Status {
				t.Errorf("%s: statuses differ, triage changed Status: %q vs %q", name, c.Status, convs[0].Status)
			}
			if len(c.Labels) > 0 {
				t.Errorf("%s: triage changed uma labels: %v", name, c.Labels)
			}
			for _, s := range c.ProviderState {
				if s.Archived {
					archived++
				}
				if s.Trashed {
					trashed++
				}
				if slices.Contains(s.Flags, model.FlagImportant) {
					flagged++
				}
			}
		}
		if archived != 1 || trashed != 1 || flagged != 1 {
			t.Errorf("%s: provider state archived %d, trashed %d, flagged %d; want 1 each", name, archived, trashed, flagged)
		}
	}

	// A second pass finds the provider already showing every change.
	again, err := h.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second pass queued %+v, want nothing", again)
	}
}

// The flag kind is the strongest the provider reports: Gmail offers both,
// IMAP only star.
func TestFlagKindFollowsTheProvider(t *testing.T) {
	status := func(kinds ...string) []model.CapabilityStatus {
		return []model.CapabilityStatus{{
			Name: model.CapabilityFlag, Available: true,
			Limits: model.StateLimits{Scope: model.ScopeAccount, Kinds: kinds}.Extension(),
		}}
	}
	for _, c := range []struct {
		caps []model.CapabilityStatus
		want string
	}{
		{status(model.FlagStar, model.FlagImportant), model.FlagImportant},
		{status(model.FlagStar), model.FlagStar},
		{status(), ""},
		{nil, ""},
	} {
		if got := flagKind(c.caps); got != c.want {
			t.Errorf("flagKind(%+v) = %q, want %q", c.caps, got, c.want)
		}
	}
}

// The environment-built channel configurations are ones the real adapters
// accept, so the loop can be pointed at live mailboxes.
func TestMailboxConfigurationsValidate(t *testing.T) {
	g := gmailMailbox("me@example.com")
	if issues := google.NewFactory(google.Options{}).Validate(g.Configuration); len(issues) > 0 {
		t.Errorf("google.mail rejects the configuration: %+v", issues)
	}
	i := imapMailbox("me@example.org", "imap.example.org")
	if _, issues := imap.ParseConfig(i.Configuration); len(issues) > 0 {
		t.Errorf("imap.mail rejects the configuration: %+v", issues)
	}
	if DefaultRule("a@newsletter.example", "") != (Verdict{Archive: true}) {
		t.Error("newsletter sender is not archived")
	}
}
