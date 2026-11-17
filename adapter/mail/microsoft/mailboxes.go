package microsoft

import (
	"context"
	"net/url"
	"slices"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mailboxes"
	"hop.top/uma/model"
)

// The mailbox extensions of the adapter (model.ProviderMailbox). Two kinds
// of Exchange mailbox exist: mail folders (kind folder, the ID the folder's
// immutable ID, the name its path from the mailbox root with "/") and
// master categories (kind category, referenced by display name, so the name
// is the ID; no nesting).
const (
	// ExtensionMailboxes lists the mailbox's mail folders, every level
	// (GET .../mailFolders and .../childFolders, to 8 levels and 2000
	// folders; a deeper or larger tree is a permanent error, not a cut
	// list), and its master categories (GET .../outlook/masterCategories).
	// A well-known folder carries its role (inbox, archive, deleteditems as
	// trash, junkemail as junk, sentitems as sent, drafts). It is
	// authorized as model.ActionExtensionQuery, needs Mail.Read or
	// MailboxSettings.Read for the categories, and is also the capability
	// that gates it.
	ExtensionMailboxes model.Capability = "microsoft.mail.mailboxes"
	// ExtensionMailboxCreate creates a mail folder (the default kind) or a
	// master category, its data {"name": "Clients/Acme", "kind": "folder"}.
	// A folder name is a path: each missing parent is created, so
	// "Clients/Acme" under no "Clients" makes both, and a path under a
	// well-known folder ("Inbox/Clients") makes the child there. A category
	// name is taken whole, "/" included, with the first color preset. It is
	// idempotent by name: the mailbox object of that name and kind, compared
	// without regard to case as Exchange does, is the result, and a create
	// that loses a race reads the winner back; a create that stops midway
	// leaves the parents it made and finishes when repeated. The effect
	// targets the channel, is authorized as model.ActionExtensionExecute (so
	// a plugin actor's create can be held) and is also the capability that
	// gates it. Mail.ReadWrite covers folders, MailboxSettings.ReadWrite
	// categories.
	ExtensionMailboxCreate model.Capability = "microsoft.mail.mailbox_create"
)

// Bounds of a folder walk.
const (
	maxFolderDepth = 8
	maxFolders     = 2000
	maxFolderPages = 100
)

// mailboxExtensions are the registrations of the two extensions.
func mailboxExtensions() []adapter.ExtensionDefinition {
	return []adapter.ExtensionDefinition{mailboxes.ListDefinition(Type), mailboxes.CreateDefinition(Type)}
}

// wellKnownRoles are the mailbox roles of the well-known folders the list
// resolves by name.
var wellKnownRoles = map[string]string{
	folderInbox: model.RoleInbox, folderArchive: model.RoleArchive, folderTrash: model.RoleTrash, folderJunk: model.RoleJunk,
	"sentitems": model.MailboxRoleSent, "drafts": model.MailboxRoleDrafts,
}

// gFolder is a mailFolder resource.
type gFolder struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ChildFolderCount int    `json:"childFolderCount"`
}

type gFolderPage struct {
	Value    []gFolder `json:"value"`
	NextLink string    `json:"@odata.nextLink"`
}

// gCategory is an outlookCategory resource.
type gCategory struct {
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"displayName"`
	Color       string `json:"color,omitempty"`
}

type gCategoryPage struct {
	Value []gCategory `json:"value"`
}

// gNewFolder is the body of a folder create.
type gNewFolder struct {
	DisplayName string `json:"displayName"`
}

// children lists the child folders of parent ("" for the mailbox root's).
func (i *instance) children(ctx context.Context, s *session, parent string) ([]gFolder, error) {
	path := "/mailFolders"
	if parent != "" {
		path += "/" + url.PathEscape(parent) + "/childFolders"
	}
	link := i.c.api(i.cfg.mailboxPath()+path, url.Values{"$top": {"100"}, "$select": {"id,displayName,childFolderCount"}})
	var out []gFolder
	for range maxFolderPages {
		resp, err := s.do(ctx, request{method: "GET", url: link, prefer: []string{preferImmutableID}})
		if err != nil {
			return nil, transient(err)
		}
		var page gFolderPage
		if err := decode(resp.body, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Value...)
		if page.NextLink == "" {
			return out, nil
		}
		if !i.c.sameOrigin(page.NextLink) {
			return nil, model.NewError(model.Permanent, "graph paged to another origin")
		}
		link = page.NextLink
	}
	return nil, model.NewError(model.Permanent, "mail folder list has too many pages")
}

// listFolders reads every mail folder of the mailbox as mailboxes, with the
// roles of the well-known ones.
func (i *instance) listFolders(ctx context.Context, s *session) ([]model.ProviderMailbox, error) {
	roles := map[string]string{} // folder ID -> role
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range wellKnownRoles {
			if !yield(n) {
				return
			}
		}
	}) {
		f, err := i.folder(ctx, s, name)
		if model.IsCode(err, model.NotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		roles[f.ID] = wellKnownRoles[name]
	}
	var out []model.ProviderMailbox
	var walk func(parent, prefix string, depth int) error
	walk = func(parent, prefix string, depth int) error {
		fs, err := i.children(ctx, s, parent)
		if err != nil {
			return err
		}
		for _, f := range fs {
			name := prefix + f.DisplayName
			if len(out) >= maxFolders {
				return model.NewError(model.Permanent, "mail folder tree is over the list limit")
			}
			if f.ID != "" && model.ValidateMailboxName(name) == nil {
				out = append(out, model.ProviderMailbox{ID: f.ID, Name: name, Kind: model.MailboxFolder, Role: roles[f.ID]})
			}
			if f.ChildFolderCount > 0 && f.ID != "" {
				if depth+1 >= maxFolderDepth {
					return model.NewError(model.Permanent, "mail folder tree is over the depth limit")
				}
				if err := walk(f.ID, name+model.MailboxSeparator, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk("", "", 0); err != nil {
		return nil, err
	}
	return out, nil
}

// listCategories reads the master categories; the display name is the ID.
func (i *instance) listCategories(ctx context.Context, s *session) ([]model.ProviderMailbox, error) {
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api(i.cfg.mailboxPath()+"/outlook/masterCategories", nil)})
	if err != nil {
		return nil, transient(err)
	}
	var page gCategoryPage
	if err := decode(resp.body, &page); err != nil {
		return nil, err
	}
	var out []model.ProviderMailbox
	for _, c := range page.Value {
		if model.ValidateMailboxName(c.DisplayName) == nil {
			out = append(out, model.ProviderMailbox{ID: c.DisplayName, Name: c.DisplayName, Kind: model.MailboxCategory})
		}
	}
	return out, nil
}

// Query answers the mailbox list (adapter.ExtensionReader).
func (i *instance) Query(ctx context.Context, q model.QueryExtension) (model.Extension, error) {
	if q.Query.Name != string(ExtensionMailboxes) {
		return model.Extension{}, model.NewError(model.Unsupported, "unknown "+string(Type)+" query")
	}
	if q.Query.Version != ExtensionVersion {
		return model.Extension{}, model.NewError(model.SchemaMismatch, "unsupported query version")
	}
	if issue := mailboxes.CheckList(q.Query); issue != nil {
		return model.Extension{}, model.NewError(model.Validation, "query."+issue.Field+": "+issue.Message)
	}
	s := i.c.session()
	folders, err := i.listFolders(ctx, s)
	if err != nil {
		return model.Extension{}, err
	}
	cats, err := i.listCategories(ctx, s)
	if err != nil {
		return model.Extension{}, err
	}
	return mailboxes.Result(Type, append(folders, cats...))
}

// createMailbox runs ExtensionMailboxCreate.
func (i *instance) createMailbox(ctx context.Context, req adapter.ExecuteRequest) (adapter.Outcome, error) {
	if req.Action.Version != ExtensionVersion {
		return rejected(model.SchemaMismatch, "unsupported action version"), nil
	}
	if req.Route.Channel != i.ch.Ref {
		return adapter.Outcome{}, &model.Error{Code: model.Permanent, SafeMessage: "effect is not on a " + string(Type) + " channel", Retry: model.NoRetry}
	}
	c, issue := mailboxes.CheckCreate(req.Action, model.MailboxFolder, model.MailboxCategory)
	if issue != nil {
		return rejected(model.Validation, issue.Message), nil
	}
	s := i.c.session()
	var id string
	var err error
	if c.Kind == model.MailboxCategory {
		id, err = i.ensureCategory(ctx, s, c.Name)
	} else {
		id, err = i.ensureFolder(ctx, s, c.Name)
	}
	if err != nil {
		return rejectedOr(err)
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMailboxRef(i.ch.Ref, id)}}, nil
}

func findFolder(fs []gFolder, name string) (gFolder, bool) {
	for _, f := range fs {
		if strings.EqualFold(f.DisplayName, name) {
			return f, true
		}
	}
	return gFolder{}, false
}

// ensureFolder returns the ID of the folder at path, creating it and each
// missing parent.
func (i *instance) ensureFolder(ctx context.Context, s *session, path string) (string, error) {
	parent := ""
	for _, name := range strings.Split(path, model.MailboxSeparator) {
		fs, err := i.children(ctx, s, parent)
		if err != nil {
			return "", err
		}
		f, ok := findFolder(fs, name)
		if !ok {
			created := i.c.api(i.cfg.mailboxPath()+"/mailFolders", nil)
			if parent != "" {
				created = i.c.api(i.cfg.mailboxPath()+"/mailFolders/"+url.PathEscape(parent)+"/childFolders", nil)
			}
			resp, err := s.do(ctx, request{method: "POST", url: created, body: encode(gNewFolder{DisplayName: name}), prefer: []string{preferImmutableID}})
			switch {
			case model.IsCode(err, model.Conflict):
				// Another creator won: the folder exists now.
				fs, lerr := i.children(ctx, s, parent)
				if lerr != nil {
					return "", lerr
				}
				if f, ok = findFolder(fs, name); !ok {
					return "", err
				}
			case err != nil:
				return "", err
			default:
				if derr := decode(resp.body, &f); derr != nil || f.ID == "" {
					return "", model.NewError(model.Transient, "graph answered an unexpected body")
				}
			}
		}
		parent = f.ID
	}
	return parent, nil
}

// ensureCategory returns the display name of the master category called
// name, creating it.
func (i *instance) ensureCategory(ctx context.Context, s *session, name string) (string, error) {
	cats, err := i.listCategories(ctx, s)
	if err != nil {
		return "", err
	}
	if b, ok := mailboxes.Find(cats, name, model.MailboxCategory); ok {
		return b.ID, nil
	}
	resp, err := s.do(ctx, request{
		method: "POST", url: i.c.api(i.cfg.mailboxPath()+"/outlook/masterCategories", nil),
		body: encode(gCategory{DisplayName: name, Color: "preset0"}),
	})
	if model.IsCode(err, model.Conflict) {
		if cats, lerr := i.listCategories(ctx, s); lerr == nil {
			if b, ok := mailboxes.Find(cats, name, model.MailboxCategory); ok {
				return b.ID, nil
			}
		}
		return "", err
	}
	if err != nil {
		return "", err
	}
	var c gCategory
	if err := decode(resp.body, &c); err != nil || c.DisplayName == "" {
		return "", model.NewError(model.Transient, "graph answered an unexpected body")
	}
	return c.DisplayName, nil
}

// validateMailboxCreate checks the create action without I/O.
func validateMailboxCreate(action model.Extension) []model.ValidationIssue {
	if action.Version != ExtensionVersion {
		return nil
	}
	if _, issue := mailboxes.CheckCreate(action, model.MailboxFolder, model.MailboxCategory); issue != nil {
		return []model.ValidationIssue{*issue}
	}
	return nil
}

var _ adapter.ExtensionReader = (*instance)(nil)
