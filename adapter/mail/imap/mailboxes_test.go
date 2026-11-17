package imap_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
)

func (r *stateRig) channelEffect() adapter.Effect {
	return adapter.Effect{Route: model.ExternalRef{Channel: channelA}}
}

func (r *stateRig) create(data string) (adapter.Outcome, error) {
	return r.inst.Execute(context.Background(), adapter.ExecuteRequest{
		Effect: r.channelEffect(),
		Action: model.Extension{Name: string(imap.ExtensionMailboxCreate), Version: 1, Data: json.RawMessage(data)},
	})
}

func (r *stateRig) list(t *testing.T) []model.ProviderMailbox {
	t.Helper()
	out, err := r.inst.Query(context.Background(), model.QueryExtension{Query: model.Extension{Name: string(imap.ExtensionMailboxes), Version: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != model.MailboxListName(imap.Type)+".result" {
		t.Errorf("result schema %q", out.Name)
	}
	boxes, err := model.DecodeProviderMailboxes(out)
	if err != nil {
		t.Fatal(err)
	}
	return boxes
}

func find(boxes []model.ProviderMailbox, name string) (model.ProviderMailbox, bool) {
	for _, b := range boxes {
		if b.Name == name {
			return b, true
		}
	}
	return model.ProviderMailbox{}, false
}

// createRef creates and requires an accepted outcome with one reference.
func (r *stateRig) createRef(t *testing.T, data string) model.ExternalRef {
	t.Helper()
	out, err := r.create(data)
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("outcome %+v, %v; want provider_accepted with one reference", out, err)
	}
	return out.References[0]
}

func TestMailboxExtensionsAreDeclared(t *testing.T) {
	f := imap.NewFactory(imap.Options{})
	if string(imap.ExtensionMailboxes) != model.MailboxListName(imap.Type) || string(imap.ExtensionMailboxCreate) != model.MailboxCreateName(imap.Type) {
		t.Fatalf("names %q, %q", imap.ExtensionMailboxes, imap.ExtensionMailboxCreate)
	}
	for _, c := range []model.Capability{model.CapabilityExtensionQuery, model.CapabilityExtensionExecute, imap.ExtensionMailboxes, imap.ExtensionMailboxCreate} {
		if !slices.Contains(f.Describe().Capabilities, c) {
			t.Errorf("capabilities lack %s", c)
		}
	}
	for _, name := range []string{string(imap.ExtensionMailboxes), string(imap.ExtensionMailboxCreate)} {
		if !slices.ContainsFunc(f.Extensions(), func(d adapter.ExtensionDefinition) bool {
			return d.Name == name && d.RequiredCapability == model.Capability(name)
		}) {
			t.Errorf("no definition %s gated by its own capability", name)
		}
	}
	var _ adapter.ExtensionReader = (*imap.Instance)(nil)
	var _ adapter.ExtensionExecutor = (*imap.Instance)(nil)
	r := newStateRig(t, nil)
	st, _ := r.inst.Capabilities(t.Context())
	for _, c := range []model.Capability{model.CapabilityExtensionQuery, model.CapabilityExtensionExecute, imap.ExtensionMailboxes, imap.ExtensionMailboxCreate} {
		if !slices.ContainsFunc(st, func(s model.CapabilityStatus) bool { return s.Name == c && s.Available }) {
			t.Errorf("%s not reported available", c)
		}
	}
}

// The list holds the account's mailboxes by name, the special-use ones
// with their roles, each ID the server's own spelling.
func TestListMailboxes(t *testing.T) {
	r := newStateRig(t, nil)
	got := r.list(t)
	for name, role := range map[string]string{"INBOX": model.RoleInbox, "Archive": model.RoleArchive, "Trash": model.RoleTrash, "Junk": model.RoleJunk, "Projects": ""} {
		b, ok := find(got, name)
		if !ok || b.Role != role || b.Kind != model.MailboxFolder || b.ID != name {
			t.Errorf("%q = %+v (found %v), want folder role %q ID %q", name, b, ok, role, name)
		}
	}
}

// Under a delimiter other than "/", names read with "/" and keep the
// server's spelling as the ID; a "/" inside a name cannot be told from a
// level and is left out.
func TestListMailboxesTranslatesTheDelimiter(t *testing.T) {
	r := newStateRig(t, func(o *serverOptions, _ *imap.Config) { o.delim = '.' })
	r.srv.create("Clients")
	r.srv.create("Clients.Acme")
	r.srv.create("Odd/Name")
	got := r.list(t)
	if b, ok := find(got, "Clients/Acme"); !ok || b.ID != "Clients.Acme" {
		t.Errorf("Clients/Acme = %+v, %v; want the server's spelling as ID", b, ok)
	}
	for _, b := range got {
		if b.Name == "Odd/Name" || b.ID == "Odd/Name" {
			t.Errorf("a mailbox named with the separator was listed: %+v", b)
		}
	}
}

// Create makes the mailbox and each missing parent, one CREATE each, and
// reports the mailbox's reference.
func TestCreateMakesTheMissingParents(t *testing.T) {
	r := newStateRig(t, nil)
	ref := r.createRef(t, `{"name":"Clients/Acme/2026"}`)
	if ref != model.ProviderMailboxRef(channelA, "Clients/Acme/2026") {
		t.Errorf("reference %+v", ref)
	}
	got := r.list(t)
	for _, n := range []string{"Clients", "Clients/Acme", "Clients/Acme/2026"} {
		if _, ok := find(got, n); !ok {
			t.Errorf("list lacks %q", n)
		}
	}
	if n := r.srv.commands("CREATE"); n != 3 {
		t.Errorf("CREATE commands = %d, want 3", n)
	}
}

// Under a "." delimiter the parents and the mailbox are created with it.
func TestCreateUsesTheServersDelimiter(t *testing.T) {
	r := newStateRig(t, func(o *serverOptions, _ *imap.Config) { o.delim = '.' })
	ref := r.createRef(t, `{"name":"Clients/Acme"}`)
	if ref.ID != "Clients.Acme" {
		t.Errorf("reference %+v, want the server's spelling", ref)
	}
	got := r.list(t)
	if b, ok := find(got, "Clients/Acme"); !ok || b.ID != "Clients.Acme" {
		t.Errorf("list = %+v", got)
	}
	if _, ok := find(got, "Clients"); !ok {
		t.Error("the parent was not made")
	}
	// A part holding the delimiter would nest silently: refused.
	out, err := r.create(`{"name":"a.b"}`)
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.Validation {
		t.Errorf("a name with the delimiter = %+v, %v; want rejected validation", out, err)
	}
}

// Create is idempotent by name: what exists is the result, at every level,
// with no CREATE; INBOX is INBOX in any case.
func TestCreateIsIdempotentByName(t *testing.T) {
	r := newStateRig(t, nil)
	first := r.createRef(t, `{"name":"Clients/Acme"}`)
	creates := r.srv.commands("CREATE")
	for range 2 {
		if again := r.createRef(t, `{"name":"Clients/Acme"}`); again != first {
			t.Fatalf("repeat = %+v, want %+v", again, first)
		}
	}
	if n := r.srv.commands("CREATE") - creates; n != 0 {
		t.Errorf("repeats sent %d CREATE", n)
	}
	// The parent exists, the child does not: one CREATE.
	r.createRef(t, `{"name":"Clients/Beta"}`)
	if n := r.srv.commands("CREATE") - creates; n != 1 {
		t.Errorf("a sibling sent %d CREATE, want 1", n)
	}
	if ref := r.createRef(t, `{"name":"inbox"}`); ref.ID != "inbox" {
		t.Errorf("create inbox = %+v", ref)
	}
	if n := r.srv.commands("CREATE") - creates; n != 1 {
		t.Errorf("INBOX in another case sent a CREATE")
	}
	// A mailbox the server reports as existing when created (ALREADYEXISTS)
	// is the result too, as when another creator won.
	if err := r.srv.user.Create("Raced", nil); err != nil {
		t.Fatal(err)
	}
	r.createRef(t, `{"name":"Raced"}`)
}

// A server's refusal is typed and final, and what breaks the model's rules
// is refused before the server is contacted.
func TestCreateRefusals(t *testing.T) {
	r := newStateRig(t, func(o *serverOptions, _ *imap.Config) { o.noCreate = true })
	before := len(r.srv.clientLines())
	for name, data := range map[string]string{
		"empty segment": `{"name":"a//b"}`, "no name": `{}`, "wrong kind": `{"name":"a","kind":"label"}`, "not json": `x`,
	} {
		out, err := r.create(data)
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
			t.Errorf("%s: %+v, %v; want rejected validation", name, out, err)
		}
	}
	if n := len(r.srv.clientLines()) - before; n != 0 {
		t.Errorf("%d commands for refused actions", n)
	}
	f := imap.NewFactory(imap.Options{})
	bad := model.Extension{Name: string(imap.ExtensionMailboxCreate), Version: 1, Data: json.RawMessage(`{"name":"a//b"}`)}
	if len(f.ValidateAction(model.Audience{}, bad)) != 1 {
		t.Error("ValidateAction accepted an empty segment")
	}
	if issues := f.ValidateAction(model.Audience{}, model.Extension{Name: bad.Name, Version: 1, Data: json.RawMessage(`{"name":"ok"}`)}); len(issues) != 0 {
		t.Errorf("valid action refused: %+v", issues)
	}
	out, err := r.create(`{"name":"Fine"}`)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Permanent {
		t.Errorf("a NO [CANNOT] = %+v, %v; want rejected permanent", out, err)
	}
}

func TestCreateNeedsItsChannel(t *testing.T) {
	r := newStateRig(t, nil)
	_, err := r.inst.Execute(context.Background(), adapter.ExecuteRequest{
		Effect: adapter.Effect{Route: model.ExternalRef{Channel: model.ChannelRef{Tenant: channelA.Tenant, ID: "other"}}},
		Action: model.Extension{Name: string(imap.ExtensionMailboxCreate), Version: 1, Data: json.RawMessage(`{"name":"a"}`)},
	})
	if model.CodeOf(err) != model.Permanent {
		t.Errorf("error = %v, want permanent", err)
	}
}
