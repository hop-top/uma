package model

import (
	"strings"
	"testing"
)

func TestValidateMailboxName(t *testing.T) {
	for _, ok := range []string{"Clients", "Clients/Acme", "a b/c d", "Reçus/été", strings.Repeat("x", MaxMailboxNameBytes)} {
		if err := ValidateMailboxName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"empty": "", "leading sep": "/a", "trailing sep": "a/", "doubled sep": "a//b", "blank segment": "a/ /b",
		"edge space": " a", "control": "a\x00b", "newline": "a\nb", "too long": strings.Repeat("x", MaxMailboxNameBytes+1), "not utf8": "a\xffb",
	} {
		if err := ValidateMailboxName(bad); CodeOf(err) != Validation {
			t.Errorf("%s: %q = %v, want validation", name, bad, err)
		}
	}
}

func TestMailboxExtensionNames(t *testing.T) {
	if got := MailboxListName("google.mail"); got != "google.mail.mailboxes" {
		t.Errorf("list name %q", got)
	}
	if got := MailboxCreateName("google.mail"); got != "google.mail.mailbox_create" {
		t.Errorf("create name %q", got)
	}
	for _, n := range []string{MailboxListName("google.mail"), MailboxCreateName("imap.mail")} {
		if err := ValidateExtensionName(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
}

func TestProviderMailboxesRoundTrip(t *testing.T) {
	in := []ProviderMailbox{{ID: "Label_1", Name: "Clients/Acme", Kind: MailboxLabel}, {ID: "INBOX", Name: "INBOX", Kind: MailboxFolder, Role: RoleInbox}}
	e, err := NewProviderMailboxesResult("x.y.mailboxes", in)
	if err != nil {
		t.Fatal(err)
	}
	if e.Version != MailboxExtensionVersion || e.Validate() != nil {
		t.Fatalf("result %+v: %v", e, e.Validate())
	}
	got, err := DecodeProviderMailboxes(e)
	if err != nil || len(got) != 2 || got[0] != in[0] || got[1] != in[1] {
		t.Fatalf("decoded %+v, %v", got, err)
	}
	if empty, _ := NewProviderMailboxesResult("x.y.mailboxes", nil); string(empty.Data) != `{}` {
		t.Errorf("empty list encodes as %s", empty.Data)
	}
	bad := Extension{Name: "x.y.mailboxes", Version: 1, Data: []byte(`{"mailboxes":[{"id":"a","name":"a","kind":"bogus","role":""}]}`)}
	if _, err := DecodeProviderMailboxes(bad); CodeOf(err) != Validation {
		t.Errorf("a bad kind decoded: %v", err)
	}
}

func TestCreateProviderMailbox(t *testing.T) {
	a, err := CreateProviderMailbox{Name: "A/B", Kind: MailboxFolder}.Extension("imap.mail")
	if err != nil || a.Name != "imap.mail.mailbox_create" || string(a.Data) != `{"name":"A/B","kind":"folder"}` {
		t.Fatalf("action %+v, %v", a, err)
	}
	if CodeOf(CreateProviderMailbox{Name: "A", Kind: "bogus"}.Validate()) != Validation {
		t.Error("a bad kind is valid")
	}
	if err := (CreateProviderMailbox{Name: "A"}).Validate(); err != nil {
		t.Error(err)
	}
}
