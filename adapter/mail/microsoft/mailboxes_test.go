package microsoft

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func channelEffect() adapter.Effect { return effect(model.ExternalRef{Channel: testChan}, "a1") }

func createMailbox(e *env, data string) (adapter.Outcome, error) {
	return e.inst.Execute(context.Background(), adapter.ExecuteRequest{
		Effect: channelEffect(),
		Action: model.Extension{Name: string(ExtensionMailboxCreate), Version: ExtensionVersion, Data: json.RawMessage(data)},
	})
}

func queryMailboxes(e *env) ([]model.ProviderMailbox, error) {
	out, err := e.inst.Query(context.Background(), model.QueryExtension{Query: model.Extension{Name: string(ExtensionMailboxes), Version: ExtensionVersion}})
	if err != nil {
		return nil, err
	}
	if out.Name != model.MailboxListName(Type)+".result" {
		return nil, model.NewError(model.Permanent, "result schema "+out.Name)
	}
	return model.DecodeProviderMailboxes(out)
}

func byName(boxes []model.ProviderMailbox, kind, name string) (model.ProviderMailbox, bool) {
	for _, b := range boxes {
		if b.Kind == kind && b.Name == name {
			return b, true
		}
	}
	return model.ProviderMailbox{}, false
}

func TestMailboxExtensionsAreDeclared(t *testing.T) {
	if string(ExtensionMailboxes) != model.MailboxListName(Type) || string(ExtensionMailboxCreate) != model.MailboxCreateName(Type) {
		t.Fatalf("names %q, %q", ExtensionMailboxes, ExtensionMailboxCreate)
	}
	f := NewFactory(Options{})
	for _, c := range []model.Capability{model.CapabilityExtensionQuery, model.CapabilityExtensionExecute, ExtensionMailboxes, ExtensionMailboxCreate} {
		if !slices.Contains(f.Describe().Capabilities, c) {
			t.Errorf("capabilities lack %s", c)
		}
	}
	for _, name := range []string{string(ExtensionMailboxes), string(ExtensionMailboxCreate)} {
		if !slices.ContainsFunc(f.Extensions(), func(d adapter.ExtensionDefinition) bool {
			return d.Name == name && d.RequiredCapability == model.Capability(name)
		}) {
			t.Errorf("no definition %s gated by its own capability", name)
		}
	}
	e := newEnv(t, Config{})
	st, _ := e.inst.Capabilities(context.Background())
	for _, c := range []model.Capability{model.CapabilityExtensionQuery, ExtensionMailboxes, ExtensionMailboxCreate} {
		if !slices.ContainsFunc(st, func(s model.CapabilityStatus) bool { return s.Name == c && s.Available }) {
			t.Errorf("%s not reported available", c)
		}
	}
}

// The list holds every folder at every level, named by path, with the
// roles of the well-known ones, and the master categories by display name.
func TestListMailboxes(t *testing.T) {
	e := newEnv(t, Config{})
	clients := e.g.addFolder(e.box, "", "Clients")
	acme := e.g.addFolder(e.box, clients, "Acme")
	e.g.addFolder(e.box, acme, "2026")
	e.g.addFolder(e.box, "AAMk-inbox", "Projects")
	e.g.mu.Lock()
	e.box.cats = []gCategory{{ID: "c1", DisplayName: "VIP", Color: "preset3"}, {ID: "c2", DisplayName: "Follow up"}}
	e.g.mu.Unlock()
	got, err := queryMailboxes(e)
	if err != nil {
		t.Fatal(err)
	}
	for name, role := range map[string]string{
		"Inbox": model.RoleInbox, "Archive": model.RoleArchive, "Deleted Items": model.RoleTrash, "Junk Email": model.RoleJunk,
		"Sent Items": model.MailboxRoleSent, "Drafts": model.MailboxRoleDrafts, "Clients": "", "Clients/Acme": "", "Clients/Acme/2026": "", "Inbox/Projects": "",
	} {
		b, ok := byName(got, model.MailboxFolder, name)
		if !ok || b.Role != role {
			t.Errorf("folder %q = %+v (found %v), want role %q", name, b, ok, role)
		}
	}
	if b, _ := byName(got, model.MailboxFolder, "Clients/Acme"); b.ID != acme {
		t.Errorf("Clients/Acme ID = %q, want the folder's %q", b.ID, acme)
	}
	for _, n := range []string{"VIP", "Follow up"} {
		if b, ok := byName(got, model.MailboxCategory, n); !ok || b.ID != n {
			t.Errorf("category %q = %+v, want its name as its ID", n, b)
		}
	}
}

// A paged folder list is read whole, following the next links.
func TestListMailboxesFollowsPages(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.mu.Lock()
	e.box.pageSize = 2
	e.g.mu.Unlock()
	for _, n := range []string{"A", "B", "C", "D", "E"} {
		e.g.addFolder(e.box, "", n)
	}
	got, err := queryMailboxes(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"A", "B", "C", "D", "E", "Inbox", "Sent Items"} {
		if _, ok := byName(got, model.MailboxFolder, n); !ok {
			t.Errorf("folder %q missing from %+v", n, got)
		}
	}
	if n := e.g.count("GET /v1.0/users/" + testMailbox + "/mailFolders?"); n < 3 {
		t.Errorf("root pages = %d, want at least 3", n)
	}
}

// A tree over the depth bound is a permanent error, never a cut list.
func TestListMailboxesRefusesATreeOverTheBound(t *testing.T) {
	e := newEnv(t, Config{})
	parent := ""
	for range maxFolderDepth + 1 {
		parent = e.g.addFolder(e.box, parent, "L")
	}
	_, err := queryMailboxes(e)
	wantCode(t, err, model.Permanent)
}

func TestListMailboxesKeepsRefusals(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/mailFolders") {
			graphError(w, http.StatusForbidden, "ErrorAccessDenied")
			return true
		}
		return false
	}
	e.g.mu.Unlock()
	_, err := queryMailboxes(e)
	wantCode(t, err, model.Unauthorized)
}

func TestQueryRefusesOtherQueriesAndVersions(t *testing.T) {
	e := newEnv(t, Config{})
	for name, q := range map[string]struct {
		x    model.Extension
		code model.ErrorCode
	}{
		"name":    {model.Extension{Name: "microsoft.mail.nope", Version: 1}, model.Unsupported},
		"version": {model.Extension{Name: string(ExtensionMailboxes), Version: 9}, model.SchemaMismatch},
		"data":    {model.Extension{Name: string(ExtensionMailboxes), Version: 1, Data: json.RawMessage(`[]`)}, model.Validation},
	} {
		_, err := e.inst.Query(context.Background(), model.QueryExtension{Query: q.x})
		if model.CodeOf(err) != q.code {
			t.Errorf("%s: %v, want %s", name, err, q.code)
		}
	}
}

// A folder path creates each missing parent, under the root or under a
// well-known folder, and reports the last folder's reference.
func TestCreateFolderMakesTheMissingParents(t *testing.T) {
	e := newEnv(t, Config{})
	out, err := createMailbox(e, `{"name":"Clients/Acme/2026"}`)
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	if got := e.g.pathOf(e.box, out.References[0].ID); got != "Clients/Acme/2026" {
		t.Errorf("the referenced folder is %q", got)
	}
	if out.References[0] != model.ProviderMailboxRef(testChan, out.References[0].ID) {
		t.Errorf("reference %+v", out.References[0])
	}
	// Under a well-known folder.
	out, err = createMailbox(e, `{"name":"Inbox/Projects"}`)
	if err != nil || e.g.pathOf(e.box, out.References[0].ID) != "Inbox/Projects" {
		t.Fatalf("Inbox/Projects: %+v, %v", out, err)
	}
	got, _ := queryMailboxes(e)
	for _, n := range []string{"Clients", "Clients/Acme", "Clients/Acme/2026", "Inbox/Projects"} {
		if _, ok := byName(got, model.MailboxFolder, n); !ok {
			t.Errorf("list lacks %q", n)
		}
	}
}

// Create is idempotent by name, at every level: parents that exist are
// reused (in any case), the folder that exists is the result, and a create
// that stopped midway finishes when repeated.
func TestCreateFolderIsIdempotentByName(t *testing.T) {
	e := newEnv(t, Config{})
	first, err := createMailbox(e, `{"name":"Clients/Acme"}`)
	if err != nil {
		t.Fatal(err)
	}
	posts := e.g.count("POST ")
	for _, name := range []string{"Clients/Acme", "clients/ACME"} {
		out, err := createMailbox(e, `{"name":"`+name+`"}`)
		if err != nil || out.References[0] != first.References[0] {
			t.Fatalf("create %q = %+v, %v; want the existing folder", name, out, err)
		}
	}
	if n := e.g.count("POST "); n != posts {
		t.Errorf("repeats made %d creates", n-posts)
	}
	// A sibling under the existing parent adds one folder only.
	if _, err := createMailbox(e, `{"name":"Clients/Beta"}`); err != nil {
		t.Fatal(err)
	}
	if n := e.g.count("POST "); n != posts+1 {
		t.Errorf("creates = %d, want %d", n, posts+1)
	}
	// Midway: the parent exists, the child does not.
	e.g.addFolder(e.box, "", "Half")
	out, err := createMailbox(e, `{"name":"Half/Done"}`)
	if err != nil || e.g.pathOf(e.box, out.References[0].ID) != "Half/Done" {
		t.Fatalf("finishing a half-made path: %+v, %v", out, err)
	}
	// A well-known folder is a folder that exists.
	out, err = createMailbox(e, `{"name":"Inbox"}`)
	if err != nil || out.References[0].ID != "AAMk-inbox" {
		t.Fatalf("create Inbox = %+v, %v", out, err)
	}
}

// A create that loses a race on one level reads the winner back.
func TestCreateFolderReadsTheWinnerOfARace(t *testing.T) {
	e := newEnv(t, Config{})
	id := e.g.addFolder(e.box, "", "Race")
	hidden := true
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		// The first root list is answered before the folder existed.
		if hidden && r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/mailFolders") {
			hidden = false
			writeJSON(w, http.StatusOK, gFolderPage{})
			return true
		}
		return false
	}
	e.g.mu.Unlock()
	out, err := createMailbox(e, `{"name":"Race"}`)
	if err != nil || out.State != model.ProviderAccepted || out.References[0].ID != id {
		t.Fatalf("outcome %+v, %v; want the folder that won", out, err)
	}
}

// A category is created whole, "/" included, idempotently by name.
func TestCreateCategory(t *testing.T) {
	e := newEnv(t, Config{})
	out, err := createMailbox(e, `{"name":"Clients/VIP","kind":"category"}`)
	if err != nil || out.State != model.ProviderAccepted || out.References[0].ID != "Clients/VIP" {
		t.Fatalf("outcome %+v, %v; want the display name as ID", out, err)
	}
	again, err := createMailbox(e, `{"name":"clients/vip","kind":"category"}`)
	if err != nil || again.References[0].ID != "Clients/VIP" {
		t.Fatalf("repeat = %+v, %v", again, err)
	}
	if n := e.g.count("POST /v1.0/users/" + testMailbox + "/outlook/masterCategories"); n != 1 {
		t.Errorf("category creates = %d, want 1", n)
	}
	e.g.mu.Lock()
	cats := slices.Clone(e.box.cats)
	e.g.mu.Unlock()
	if len(cats) != 1 || cats[0].Color == "" {
		t.Errorf("categories = %+v, want one with a color", cats)
	}
	// A category and a folder of one name are two things.
	if _, err := createMailbox(e, `{"name":"Clients/VIP"}`); err != nil {
		t.Fatal(err)
	}
	got, _ := queryMailboxes(e)
	if _, ok := byName(got, model.MailboxFolder, "Clients/VIP"); !ok {
		t.Error("the folder was not made")
	}
	if _, ok := byName(got, model.MailboxCategory, "Clients/VIP"); !ok {
		t.Error("the category is gone from the list")
	}
}

func TestCreateMailboxRefusals(t *testing.T) {
	e := newEnv(t, Config{})
	before := len(e.g.requests())
	for name, data := range map[string]string{
		"empty segment": `{"name":"a//b"}`, "no name": `{}`, "wrong kind": `{"name":"a","kind":"label"}`, "not json": `x`,
	} {
		out, err := createMailbox(e, data)
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
			t.Errorf("%s: %+v, %v; want rejected validation", name, out, err)
		}
	}
	if n := len(e.g.requests()) - before; n != 0 {
		t.Errorf("%d requests for refused actions", n)
	}
	f := NewFactory(Options{})
	bad := model.Extension{Name: string(ExtensionMailboxCreate), Version: 1, Data: json.RawMessage(`{"name":"a//b"}`)}
	if len(f.ValidateAction(model.Audience{}, bad)) != 1 {
		t.Error("ValidateAction accepted an empty segment")
	}
	good := model.Extension{Name: string(ExtensionMailboxCreate), Version: 1, Data: json.RawMessage(`{"name":"ok","kind":"category"}`)}
	if issues := f.ValidateAction(model.Audience{}, good); len(issues) != 0 {
		t.Errorf("valid action refused: %+v", issues)
	}
	// Graph refusing the name (400) is a final rejection.
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "POST" {
			graphError(w, http.StatusBadRequest, "ErrorInvalidRequest")
			return true
		}
		return false
	}
	e.g.mu.Unlock()
	out, err := createMailbox(e, `{"name":"Fine"}`)
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.Permanent {
		t.Errorf("a 400 = %+v, %v; want rejected permanent", out, err)
	}
}

func TestCreateMailboxNeedsItsChannel(t *testing.T) {
	e := newEnv(t, Config{})
	other := effect(model.ExternalRef{Channel: model.ChannelRef{Tenant: testChan.Tenant, ID: "other"}}, "a1")
	_, err := e.inst.Execute(context.Background(), adapter.ExecuteRequest{
		Effect: other, Action: model.Extension{Name: string(ExtensionMailboxCreate), Version: 1, Data: json.RawMessage(`{"name":"a"}`)},
	})
	wantCode(t, err, model.Permanent)
}
