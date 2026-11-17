package imap

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mailboxes"
	"hop.top/uma/model"
)

// The mailbox extensions of the adapter (model.ProviderMailbox). An IMAP
// mailbox is a folder: Kind folder, the ID the mailbox name as the server
// spells it, the name the same with the server's hierarchy delimiter
// replaced by "/" so a path reads one way on every provider.
const (
	// ExtensionMailboxes lists the account's mailboxes (LIST "" "*", with
	// the special-use attributes when the server offers them): INBOX is
	// inbox, \Archive archive, \Trash trash, \Junk junk, \Sent sent,
	// \Drafts drafts. A mailbox that cannot exist (\NonExistent) is left
	// out; one whose name holds "/" under another delimiter cannot be named
	// by a path and is left out too. It is authorized as
	// model.ActionExtensionQuery and is also the capability that gates it.
	ExtensionMailboxes model.Capability = "imap.mail.mailboxes"
	// ExtensionMailboxCreate creates a mailbox (CREATE), its data
	// {"name": "Clients/Acme"} (kind, when given, must be "folder"). The
	// name is a path: the server's delimiter is read with LIST "" "" and
	// each missing parent is created in turn, since a server need not make
	// them; a path segment holding the delimiter is refused. It is
	// idempotent by name: a mailbox that exists (INBOX in any case, others
	// byte for byte), or that the server reports as ALREADYEXISTS when
	// created, is the result. The effect targets the channel, is authorized
	// as model.ActionExtensionExecute (so a plugin actor's create can be
	// held) and is also the capability that gates it. The mailbox is not
	// subscribed (SUBSCRIBE): LIST shows it, a client that lists only
	// subscribed mailboxes does not. A server whose personal namespace has a
	// prefix refuses a name outside it, which is a rejected create.
	ExtensionMailboxCreate model.Capability = "imap.mail.mailbox_create"
)

// mailboxExtensions are the registrations of the two extensions.
func mailboxExtensions() []adapter.ExtensionDefinition {
	return []adapter.ExtensionDefinition{mailboxes.ListDefinition(Type), mailboxes.CreateDefinition(Type)}
}

// Extensions returns the extension definitions the adapter ships, for the
// host to register with the factory.
func (f *Factory) Extensions() []adapter.ExtensionDefinition { return mailboxExtensions() }

// ValidateAction checks the fields of an extension effect without I/O
// (adapter.ActionValidator). An action of another name or version is not
// judged here.
func (f *Factory) ValidateAction(_ model.Audience, action model.Extension) []model.ValidationIssue {
	if action.Name != string(ExtensionMailboxCreate) || action.Version != model.MailboxExtensionVersion {
		return nil
	}
	if _, issue := mailboxes.CheckCreate(action, model.MailboxFolder); issue != nil {
		return []model.ValidationIssue{*issue}
	}
	return nil
}

var (
	_ adapter.ExtensionReader   = (*Instance)(nil)
	_ adapter.ExtensionExecutor = (*Instance)(nil)
	_ adapter.ActionValidator   = (*Factory)(nil)
)

// listed is one mailbox the server listed.
type listed struct {
	name  string // as the server spells it
	delim rune   // 0: no hierarchy
	attrs []imap.MailboxAttr
}

func (l listed) has(a imap.MailboxAttr) bool {
	return slices.ContainsFunc(l.attrs, func(x imap.MailboxAttr) bool { return strings.EqualFold(string(x), string(a)) })
}

// list reads every mailbox of the account.
func (i *Instance) list(ctx context.Context, s *session) ([]listed, error) {
	caps := s.c.Caps()
	opts := &imap.ListOptions{ReturnSpecialUse: caps.Has(imap.CapSpecialUse) || caps.Has(imap.CapIMAP4rev2)}
	var data []*imap.ListData
	if err := s.within(func() error {
		var err error
		data, err = s.c.List("", "*", opts).Collect()
		return err
	}); err != nil {
		return nil, lost(ctx, err)
	}
	out := make([]listed, 0, len(data))
	for _, d := range data {
		out = append(out, listed{name: d.Mailbox, delim: d.Delim, attrs: d.Attrs})
	}
	return out, nil
}

// delimiter reads the hierarchy delimiter of the personal namespace
// (LIST "" ""); 0 is a server without hierarchy.
func (i *Instance) delimiter(ctx context.Context, s *session) (rune, error) {
	var data []*imap.ListData
	if err := s.within(func() error {
		var err error
		data, err = s.c.List("", "", nil).Collect()
		return err
	}); err != nil {
		return 0, lost(ctx, err)
	}
	for _, d := range data {
		if d.Delim != 0 {
			return d.Delim, nil
		}
	}
	return 0, nil
}

// mailboxRole is the role of a listed mailbox.
func (i *Instance) mailboxRole(l listed) string {
	switch {
	case strings.EqualFold(l.name, "INBOX") || l.name == i.cfg.mailbox():
		return model.RoleInbox
	case l.has(imap.MailboxAttrArchive):
		return model.RoleArchive
	case l.has(imap.MailboxAttrTrash):
		return model.RoleTrash
	case l.has(imap.MailboxAttrJunk):
		return model.RoleJunk
	case l.has(imap.MailboxAttrSent):
		return model.MailboxRoleSent
	case l.has(imap.MailboxAttrDrafts):
		return model.MailboxRoleDrafts
	}
	return ""
}

// asMailboxes turns the listed mailboxes into the model's, with paths.
func (i *Instance) asMailboxes(ls []listed) []model.ProviderMailbox {
	var out []model.ProviderMailbox
	for _, l := range ls {
		if l.has(imap.MailboxAttrNonExistent) {
			continue
		}
		name := l.name
		if l.delim != 0 && l.delim != '/' {
			if strings.Contains(name, model.MailboxSeparator) {
				continue // a "/" inside a segment cannot be told from a level
			}
			name = strings.ReplaceAll(name, string(l.delim), model.MailboxSeparator)
		}
		if model.ValidateMailboxName(name) != nil {
			continue
		}
		out = append(out, model.ProviderMailbox{ID: l.name, Name: name, Kind: model.MailboxFolder, Role: i.mailboxRole(l)})
	}
	return out
}

// Query answers the mailbox list (adapter.ExtensionReader).
func (i *Instance) Query(ctx context.Context, q model.QueryExtension) (model.Extension, error) {
	if q.Query.Name != string(ExtensionMailboxes) {
		return model.Extension{}, model.NewError(model.Unsupported, "unknown "+string(Type)+" query")
	}
	if q.Query.Version != model.MailboxExtensionVersion {
		return model.Extension{}, model.NewError(model.SchemaMismatch, "unsupported query version")
	}
	if issue := mailboxes.CheckList(q.Query); issue != nil {
		return model.Extension{}, model.NewError(model.Validation, "query."+issue.Field+": "+issue.Message)
	}
	if i.closed.Load() {
		return model.Extension{}, i.errClosed()
	}
	s, err := i.connect(ctx)
	if err != nil {
		return model.Extension{}, err
	}
	defer s.close()
	ls, err := i.list(ctx, s)
	if err != nil {
		return model.Extension{}, err
	}
	return mailboxes.Result(Type, i.asMailboxes(ls))
}

// Execute runs ExtensionMailboxCreate (adapter.ExtensionExecutor).
func (i *Instance) Execute(ctx context.Context, req adapter.ExecuteRequest) (adapter.Outcome, error) {
	if req.Action.Name != string(ExtensionMailboxCreate) {
		return rejection(model.Unsupported, "unknown "+string(Type)+" action"), nil
	}
	if req.Action.Version != model.MailboxExtensionVersion {
		return rejection(model.SchemaMismatch, "unsupported action version"), nil
	}
	if req.Route.Channel != i.ch.Ref {
		return adapter.Outcome{}, &model.Error{Code: model.Permanent, SafeMessage: "effect is not on a " + string(Type) + " channel", Retry: model.NoRetry}
	}
	c, issue := mailboxes.CheckCreate(req.Action, model.MailboxFolder)
	if issue != nil {
		return rejection(model.Validation, issue.Message), nil
	}
	if i.closed.Load() {
		return adapter.Outcome{}, i.errClosed()
	}
	s, err := i.connect(ctx)
	if err != nil {
		return adapter.Outcome{}, err
	}
	defer s.close()
	delim, err := i.delimiter(ctx, s)
	if err != nil {
		return adapter.Outcome{}, err
	}
	parts := strings.Split(c.Name, model.MailboxSeparator)
	switch {
	case len(parts) > 1 && delim == 0:
		return rejection(model.Unsupported, PurposeIMAP+": the server keeps no mailbox hierarchy"), nil
	case delim != 0 && delim != '/' && slices.ContainsFunc(parts, func(p string) bool { return strings.ContainsRune(p, delim) }):
		return rejection(model.Validation, PurposeIMAP+": a name part holds the server's delimiter "+string(delim)), nil
	}
	ls, err := i.list(ctx, s)
	if err != nil {
		return adapter.Outcome{}, err
	}
	have := func(name string) bool {
		return slices.ContainsFunc(ls, func(l listed) bool {
			return l.name == name || strings.EqualFold(name, "INBOX") && strings.EqualFold(l.name, "INBOX")
		})
	}
	var name string
	for k := range parts {
		name = strings.Join(parts[:k+1], string(delim))
		if have(name) {
			continue
		}
		cerr := s.within(func() error { return s.c.Create(name, nil).Wait() })
		var ie *imap.Error
		switch {
		case cerr == nil:
		case errors.As(cerr, &ie) && ctx.Err() == nil && ie.Code == imap.ResponseCodeAlreadyExists:
			// Another creator won: the mailbox exists, as asked.
		case errors.As(cerr, &ie) && ctx.Err() == nil:
			f := replyFailure(ie)
			if f.Retry != model.NoRetry {
				return adapter.Outcome{}, f
			}
			return adapter.Outcome{State: model.Rejected, Failure: f}, nil
		default:
			// The answer was lost: a repeat finds the mailbox or makes it.
			return adapter.Outcome{}, lost(ctx, cerr)
		}
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMailboxRef(i.ch.Ref, name)}}, nil
}
