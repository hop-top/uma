package mailboxes

import (
	"strings"
	"testing"

	"hop.top/uma/model"
)

func act(data string) model.Extension {
	return model.Extension{Name: "x.mail.mailbox_create", Version: 1, Data: []byte(data)}
}

func TestDefinitionsAreGatedByTheirOwnCapability(t *testing.T) {
	l, c := ListDefinition("x.mail"), CreateDefinition("x.mail")
	if l.Name != "x.mail.mailboxes" || l.RequiredCapability != "x.mail.mailboxes" || l.Kind != model.ExtensionKindRead || l.AuthorizeAs != model.ActionExtensionQuery {
		t.Errorf("list definition %+v", l)
	}
	if c.Name != "x.mail.mailbox_create" || c.RequiredCapability != "x.mail.mailbox_create" || c.Kind != model.ExtensionKindEffect || c.AuthorizeAs != model.ActionExtensionExecute {
		t.Errorf("create definition %+v", c)
	}
	if l.ResultSchema.Name != ResultName("x.mail") {
		t.Errorf("result schema %+v", l.ResultSchema)
	}
}

func TestCheckCreate(t *testing.T) {
	got, issue := CheckCreate(act(`{"name":"Clients/Acme","extra":1}`), model.MailboxFolder, model.MailboxCategory)
	if issue != nil || got.Name != "Clients/Acme" || got.Kind != model.MailboxFolder {
		t.Fatalf("got %+v, %v: unknown fields are ignored and the default kind is the first", got, issue)
	}
	got, issue = CheckCreate(act(`{"name":"VIP","kind":"category"}`), model.MailboxFolder, model.MailboxCategory)
	if issue != nil || got.Kind != model.MailboxCategory {
		t.Fatalf("got %+v, %v", got, issue)
	}
	for name, data := range map[string]string{
		"no data":       ``,
		"array":         `[]`,
		"null":          `null`,
		"no name":       `{}`,
		"name type":     `{"name":3}`,
		"empty segment": `{"name":"a//b"}`,
		"unknown kind":  `{"name":"a","kind":"bogus"}`,
		"absent kind":   `{"name":"a","kind":"label"}`,
		"control":       `{"name":"a\u0000"}`,
	} {
		if _, issue := CheckCreate(act(data), model.MailboxFolder, model.MailboxCategory); issue == nil || issue.Code != string(model.Validation) || issue.Message == "" {
			t.Errorf("%s: issue %+v, want a validation issue", name, issue)
		}
	}
}

func TestCheckList(t *testing.T) {
	for _, ok := range []string{``, `{}`, `{"x":1}`} {
		if issue := CheckList(model.Extension{Data: []byte(ok)}); issue != nil {
			t.Errorf("%q: %+v", ok, issue)
		}
	}
	for _, bad := range []string{`[]`, `null`, `3`} {
		if issue := CheckList(model.Extension{Data: []byte(bad)}); issue == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFindIgnoresCaseWithinAKind(t *testing.T) {
	boxes := []model.ProviderMailbox{
		{ID: "1", Name: "Clients/Acme", Kind: model.MailboxFolder},
		{ID: "2", Name: "VIP", Kind: model.MailboxCategory},
	}
	if b, ok := Find(boxes, "clients/ACME", model.MailboxFolder); !ok || b.ID != "1" {
		t.Errorf("Find = %+v, %v", b, ok)
	}
	if _, ok := Find(boxes, "VIP", model.MailboxFolder); ok {
		t.Error("a category matched as a folder")
	}
	if _, ok := Find(boxes, "Clients", model.MailboxFolder); ok {
		t.Error("a parent matched by prefix")
	}
}

func TestResultSortsAndBounds(t *testing.T) {
	out, err := Result("x.mail", []model.ProviderMailbox{
		{ID: "2", Name: "b", Kind: model.MailboxLabel}, {ID: "1", Name: "a", Kind: model.MailboxLabel}, {ID: "0", Name: "z", Kind: model.MailboxFolder},
	})
	if err != nil || out.Name != ResultName("x.mail") {
		t.Fatalf("result %+v, %v", out, err)
	}
	got, _ := model.DecodeProviderMailboxes(out)
	if got[0].ID != "0" || got[1].ID != "1" || got[2].ID != "2" {
		t.Errorf("order %+v, want kind then name", got)
	}
	var many []model.ProviderMailbox
	for i := 0; i < 5000; i++ {
		many = append(many, model.ProviderMailbox{ID: strings.Repeat("i", 40), Name: strings.Repeat("n", 40), Kind: model.MailboxLabel})
	}
	_, err = Result("x.mail", many)
	if model.CodeOf(err) != model.Permanent {
		t.Errorf("an oversize list = %v, want permanent", err)
	}
}
