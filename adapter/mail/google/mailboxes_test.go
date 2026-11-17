package google

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func channelEffect() adapter.Effect {
	e := effect(model.ExternalRef{Channel: testChan}, "a1")
	return e
}

func createMailbox(e *env, data string) (adapter.Outcome, error) {
	return e.inst.Execute(context.Background(), adapter.ExecuteRequest{
		Effect: channelEffect(),
		Action: model.Extension{Name: string(ExtensionMailboxCreate), Version: ExtensionVersion, Data: json.RawMessage(data)},
	})
}

func listMailboxes(t *testing.T, e *env) []model.ProviderMailbox {
	t.Helper()
	out, err := e.inst.Query(t.Context(), model.QueryExtension{Query: model.Extension{Name: string(ExtensionMailboxes), Version: ExtensionVersion}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != model.MailboxListName(Type)+".result" {
		t.Errorf("result schema %q", out.Name)
	}
	boxes, err := model.DecodeProviderMailboxes(out)
	if err != nil {
		t.Fatal(err)
	}
	return boxes
}

// The names are the model's, and the capabilities that gate them are
// declared with the query capability.
func TestMailboxExtensionsAreDeclared(t *testing.T) {
	if string(ExtensionMailboxes) != model.MailboxListName(Type) || string(ExtensionMailboxCreate) != model.MailboxCreateName(Type) {
		t.Fatalf("names %q, %q", ExtensionMailboxes, ExtensionMailboxCreate)
	}
	f := NewFactory(Options{})
	caps := f.Describe().Capabilities
	for _, c := range []model.Capability{model.CapabilityExtensionQuery, model.CapabilityExtensionExecute, ExtensionMailboxes, ExtensionMailboxCreate} {
		if !slices.Contains(caps, c) {
			t.Errorf("capabilities lack %s", c)
		}
	}
	defs := f.Extensions()
	for _, name := range []string{string(ExtensionMailboxes), string(ExtensionMailboxCreate)} {
		if !slices.ContainsFunc(defs, func(d adapter.ExtensionDefinition) bool {
			return d.Name == name && d.RequiredCapability == model.Capability(name)
		}) {
			t.Errorf("no definition %s gated by its own capability", name)
		}
	}
	e := newEnv(t, Config{})
	st, _ := e.inst.Capabilities(t.Context())
	for _, c := range []model.Capability{model.CapabilityExtensionQuery, ExtensionMailboxes, ExtensionMailboxCreate} {
		if !slices.ContainsFunc(st, func(s model.CapabilityStatus) bool { return s.Name == c && s.Available }) {
			t.Errorf("%s not reported available", c)
		}
	}
}

// The list is the account's labels: the system ones, with the roles Gmail
// gives them, and the user's own, by ID and name.
func TestListMailboxes(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.addUserLabel(e.box, "Clients/Acme")
	got := listMailboxes(t, e)
	byID := map[string]model.ProviderMailbox{}
	for _, b := range got {
		if b.Kind != model.MailboxLabel {
			t.Errorf("%+v is not a label", b)
		}
		byID[b.ID] = b
	}
	if b := byID["INBOX"]; b.Role != model.RoleInbox || b.Name != "INBOX" {
		t.Errorf("INBOX = %+v", b)
	}
	for id, role := range map[string]string{"TRASH": model.RoleTrash, "SPAM": model.RoleJunk, "SENT": model.MailboxRoleSent, "DRAFT": model.MailboxRoleDrafts, "STARRED": ""} {
		if byID[id].Role != role {
			t.Errorf("%s role %q, want %q", id, byID[id].Role, role)
		}
	}
	if b := byID["Label_1"]; b.Name != "Clients/Acme" || b.Role != "" {
		t.Errorf("user label = %+v", b)
	}
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/labels"); n != 1 {
		t.Errorf("label reads = %d, want 1", n)
	}
}

// A refused list is typed: a missing scope is unauthorized.
func TestListMailboxesKeepsRefusals(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		googleError(w, http.StatusForbidden, "insufficientPermissions", "PERMISSION_DENIED")
		return true
	}
	e.g.mu.Unlock()
	_, err := e.inst.Query(t.Context(), model.QueryExtension{Query: model.Extension{Name: string(ExtensionMailboxes), Version: ExtensionVersion}})
	_ = wantCode(t, err, model.Unauthorized)
}

func TestQueryRefusesOtherQueriesAndVersions(t *testing.T) {
	e := newEnv(t, Config{})
	_, err := e.inst.Query(t.Context(), model.QueryExtension{Query: model.Extension{Name: "google.mail.nope", Version: 1}})
	_ = wantCode(t, err, model.Unsupported)
	_, err = e.inst.Query(t.Context(), model.QueryExtension{Query: model.Extension{Name: string(ExtensionMailboxes), Version: 9}})
	_ = wantCode(t, err, model.SchemaMismatch)
	_, err = e.inst.Query(t.Context(), model.QueryExtension{Query: model.Extension{Name: string(ExtensionMailboxes), Version: 1, Data: json.RawMessage(`[]`)}})
	_ = wantCode(t, err, model.Validation)
}

// Create makes the label, keeping "/" in its name, and reports its
// reference; the label is then in the list.
func TestCreateMailbox(t *testing.T) {
	e := newEnv(t, Config{})
	out, err := createMailbox(e, `{"name":"Clients/Acme/2026"}`)
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	ref := out.References[0]
	if ref != model.ProviderMailboxRef(testChan, "Label_1") {
		t.Errorf("reference %+v", ref)
	}
	got := listMailboxes(t, e)
	i := slices.IndexFunc(got, func(b model.ProviderMailbox) bool { return b.ID == ref.ID })
	if i < 0 || got[i].Name != "Clients/Acme/2026" {
		t.Fatalf("list %+v lacks the new label", got)
	}
	if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/labels"); n != 1 {
		t.Errorf("creates = %d, want 1", n)
	}
}

// Create is idempotent by name: the same name, in any case, is the label
// that exists, with no second create and no error.
func TestCreateMailboxIsIdempotentByName(t *testing.T) {
	e := newEnv(t, Config{})
	first, err := createMailbox(e, `{"name":"Clients"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Clients", "clients", "CLIENTS"} {
		out, err := createMailbox(e, `{"name":"`+name+`"}`)
		if err != nil || out.State != model.ProviderAccepted || out.References[0] != first.References[0] {
			t.Fatalf("create %q = %+v, %v; want the existing label %+v", name, out, err, first.References)
		}
	}
	// A system label is a label that exists.
	out, err := createMailbox(e, `{"name":"INBOX"}`)
	if err != nil || out.References[0].ID != "INBOX" {
		t.Fatalf("create INBOX = %+v, %v", out, err)
	}
	if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/labels"); n != 1 {
		t.Errorf("creates = %d, want 1", n)
	}
}

// A create that loses a race (the list showed no label, the create is
// 409) reads the winner back.
func TestCreateMailboxReadsTheWinnerOfARace(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.addUserLabel(e.box, "Race")
	var lists atomic.Int32
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		// The first list is answered before the label existed.
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/labels") && lists.Add(1) == 1 {
			writeJSON(w, http.StatusOK, gLabelList{})
			return true
		}
		return false
	}
	e.g.mu.Unlock()
	out, err := createMailbox(e, `{"name":"Race"}`)
	if err != nil || out.State != model.ProviderAccepted || out.References[0].ID != "Label_1" {
		t.Fatalf("outcome %+v, %v; want the label that won", out, err)
	}
	if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/labels"); n != 1 {
		t.Errorf("creates = %d, want the one that lost", n)
	}
}

// A name Gmail refuses is a final rejection, and one that breaks the
// model's rules is refused before any request.
func TestCreateMailboxRefusals(t *testing.T) {
	e := newEnv(t, Config{})
	before := len(e.g.requests())
	for name, data := range map[string]string{
		"empty segment": `{"name":"a//b"}`, "no name": `{}`, "wrong kind": `{"name":"a","kind":"folder"}`, "not json": `x`,
	} {
		out, err := createMailbox(e, data)
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
			t.Errorf("%s: %+v, %v; want rejected validation", name, out, err)
		}
	}
	if n := len(e.g.requests()) - before; n != 0 {
		t.Errorf("%d requests for refused actions", n)
	}
	// The factory refuses them at the call too.
	issues := NewFactory(Options{}).ValidateAction(model.Audience{}, model.Extension{Name: string(ExtensionMailboxCreate), Version: 1, Data: json.RawMessage(`{"name":"a//b"}`)})
	if len(issues) != 1 {
		t.Errorf("ValidateAction issues = %+v", issues)
	}
	if issues := NewFactory(Options{}).ValidateAction(model.Audience{}, model.Extension{Name: string(ExtensionMailboxCreate), Version: 1, Data: json.RawMessage(`{"name":"ok"}`)}); len(issues) != 0 {
		t.Errorf("valid action refused: %+v", issues)
	}
	// A name Gmail itself refuses (400) is rejected as permanent.
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "POST" {
			googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
			return true
		}
		return false
	}
	e.g.mu.Unlock()
	out, err := createMailbox(e, `{"name":"Fine Name"}`)
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.Permanent {
		t.Errorf("a 400 = %+v, %v; want rejected permanent", out, err)
	}
}

// An effect not on the adapter's channel is refused.
func TestCreateMailboxNeedsItsChannel(t *testing.T) {
	e := newEnv(t, Config{})
	other := effect(model.ExternalRef{Channel: model.ChannelRef{Tenant: testChan.Tenant, ID: "other"}}, "a1")
	_, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{
		Effect: other, Action: model.Extension{Name: string(ExtensionMailboxCreate), Version: 1, Data: json.RawMessage(`{"name":"a"}`)},
	})
	_ = wantCode(t, err, model.Permanent)
}

// A thread's state report names its labels from the account's list, reads
// it once for several reports, and keeps the ID as the name for a label
// the list does not know or when the list cannot be read.
func TestStateReportsNameTheirLabels(t *testing.T) {
	e := newEnv(t, Config{})
	l := e.g.addUserLabel(e.box, "Clients/Acme")
	st := model.ProviderConversationState{Labels: []model.ProviderLabel{{ID: l.ID, Name: l.ID}, {ID: "Label_gone", Name: "Label_gone"}}}
	e.inst.nameLabels(t.Context(), e.inst.c.session(), &st)
	if st.Labels[0].Name != "Clients/Acme" || st.Labels[0].ID != l.ID {
		t.Errorf("label = %+v, want the list's name", st.Labels[0])
	}
	if st.Labels[1].Name != "Label_gone" {
		t.Errorf("unknown label = %+v, want its ID kept as its name", st.Labels[1])
	}
	st2 := model.ProviderConversationState{Labels: []model.ProviderLabel{{ID: l.ID, Name: l.ID}}}
	e.inst.nameLabels(t.Context(), e.inst.c.session(), &st2)
	if st2.Labels[0].Name != "Clients/Acme" {
		t.Errorf("second report = %+v", st2.Labels)
	}
	// One read served both reports: an unknown ID does not refresh at once.
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/labels"); n != 1 {
		t.Errorf("label reads = %d, want 1", n)
	}
	// A label made since is found once the last read is old enough.
	l2 := e.g.addUserLabel(e.box, "Later")
	st4 := model.ProviderConversationState{Labels: []model.ProviderLabel{{ID: l2.ID, Name: l2.ID}}}
	e.inst.nameLabels(t.Context(), e.inst.c.session(), &st4)
	if st4.Labels[0].Name != l2.ID {
		t.Errorf("a refresh within %v: %+v", labelNamesRefresh, st4.Labels)
	}
	e.clock.Advance(labelNamesRefresh + time.Second)
	e.inst.nameLabels(t.Context(), e.inst.c.session(), &st4)
	if st4.Labels[0].Name != "Later" {
		t.Errorf("label made since = %+v, want it named after the refresh", st4.Labels)
	}
	e.clock.Advance(labelNamesTTL + time.Second)
	e.inst.nameLabels(t.Context(), e.inst.c.session(), &st2)
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/labels"); n != 3 {
		t.Errorf("label reads = %d, want 3 (first, refresh, expiry)", n)
	}

	down := newEnv(t, Config{})
	down.g.mu.Lock()
	down.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/labels") {
			googleError(w, http.StatusForbidden, "insufficientPermissions", "PERMISSION_DENIED")
			return true
		}
		return false
	}
	down.g.mu.Unlock()
	st3 := model.ProviderConversationState{Labels: []model.ProviderLabel{{ID: "Label_9", Name: "Label_9"}}}
	down.inst.nameLabels(t.Context(), down.inst.c.session(), &st3)
	if st3.Labels[0].Name != "Label_9" {
		t.Errorf("unreadable list: %+v, want the ID kept", st3.Labels)
	}
}
