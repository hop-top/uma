package google

import (
	"context"
	"slices"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mailboxes"
	"hop.top/uma/model"
)

// The mailbox extensions of the adapter (model.ProviderMailbox). A Gmail
// mailbox is a label: Kind label, the ID the label's, the name Gmail's,
// with "/" nesting a label under another by name alone. Gmail does not make
// the parent: a label "Clients/Acme" can exist without "Clients".
const (
	// ExtensionMailboxes lists the account's labels with users.labels.list:
	// the system labels and the user's own. A system label of a role carries
	// it (INBOX inbox, TRASH trash, SPAM junk, SENT sent, DRAFT drafts).
	// It is authorized as model.ActionExtensionQuery, needs ScopeReadonly,
	// and is also the capability that gates it.
	ExtensionMailboxes model.Capability = "google.mail.mailboxes"
	// ExtensionMailboxCreate creates a label with users.labels.create, its
	// data {"name": "Clients/Acme"} (kind, when given, must be "label").
	// The effect targets the channel. It is idempotent by name: a label of
	// that name, compared without regard to case as Gmail does, is the
	// result, and a create that loses a race to another creator reads the
	// winner back; the label's reference is in the operation's provider
	// references. It is authorized as model.ActionExtensionExecute (so a
	// plugin actor's create can be held), needs ScopeModify, and is also the
	// capability that gates it. A name Gmail refuses is rejected as
	// permanent.
	ExtensionMailboxCreate model.Capability = "google.mail.mailbox_create"
)

// mailboxExtensions are the registrations of the two extensions.
func mailboxExtensions() []adapter.ExtensionDefinition {
	return []adapter.ExtensionDefinition{mailboxes.ListDefinition(Type), mailboxes.CreateDefinition(Type)}
}

// gLabel is a users.labels resource.
type gLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// gLabelList is the answer of users.labels.list.
type gLabelList struct {
	Labels []gLabel `json:"labels"`
}

// gLabelCreate is the body of users.labels.create.
type gLabelCreate struct {
	Name                  string `json:"name"`
	LabelListVisibility   string `json:"labelListVisibility"`
	MessageListVisibility string `json:"messageListVisibility"`
}

// systemRoles are the roles of the system labels that have one.
var systemRoles = map[string]string{
	labelInbox: model.RoleInbox, labelTrash: model.RoleTrash, labelSpam: model.RoleJunk,
	labelSent: model.MailboxRoleSent, labelDraft: model.MailboxRoleDrafts,
}

// listLabels reads the account's labels.
func (i *instance) listLabels(ctx context.Context, s *session) ([]model.ProviderMailbox, error) {
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/labels", nil)})
	if err != nil {
		return nil, transient(err)
	}
	var l gLabelList
	if err := decodeAnswer(resp.body, &l); err != nil {
		return nil, err
	}
	out := make([]model.ProviderMailbox, 0, len(l.Labels))
	for _, g := range l.Labels {
		if g.ID == "" || model.ValidateMailboxName(g.Name) != nil {
			continue // a label the model cannot name is left out, not guessed at
		}
		out = append(out, model.ProviderMailbox{ID: g.ID, Name: g.Name, Kind: model.MailboxLabel, Role: systemRoles[g.ID]})
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
	boxes, err := i.listLabels(ctx, i.c.session())
	if err != nil {
		return model.Extension{}, err
	}
	return mailboxes.Result(Type, boxes)
}

// createMailbox runs ExtensionMailboxCreate.
func (i *instance) createMailbox(ctx context.Context, req adapter.ExecuteRequest) (adapter.Outcome, error) {
	if req.Action.Version != ExtensionVersion {
		return rejected(model.SchemaMismatch, "unsupported action version"), nil
	}
	if req.Route.Channel != i.ch.Ref {
		return adapter.Outcome{}, &model.Error{Code: model.Permanent, SafeMessage: "effect is not on a " + string(Type) + " channel", Retry: model.NoRetry}
	}
	c, issue := mailboxes.CheckCreate(req.Action, model.MailboxLabel)
	if issue != nil {
		return rejected(model.Validation, issue.Message), nil
	}
	s := i.c.session()
	boxes, err := i.listLabels(ctx, s)
	if err != nil {
		return rejectedOr(err)
	}
	if b, ok := mailboxes.Find(boxes, c.Name, model.MailboxLabel); ok {
		return i.mailboxOutcome(b), nil
	}
	resp, err := s.do(ctx, request{
		method: "POST", url: i.c.api("/labels", nil),
		body: encode(gLabelCreate{Name: c.Name, LabelListVisibility: "labelShow", MessageListVisibility: "show"}),
	})
	if model.IsCode(err, model.Conflict) {
		// Another creator won: the label exists now, which is what was asked.
		if boxes, lerr := i.listLabels(ctx, s); lerr == nil {
			if b, ok := mailboxes.Find(boxes, c.Name, model.MailboxLabel); ok {
				return i.mailboxOutcome(b), nil
			}
		}
		return rejectedOr(err)
	}
	if err != nil {
		return rejectedOr(err)
	}
	var g gLabel
	if err := decode(resp.body, &g); err != nil || g.ID == "" {
		return adapter.Outcome{}, errUnexpectedAnswer()
	}
	return i.mailboxOutcome(model.ProviderMailbox{ID: g.ID, Name: g.Name, Kind: model.MailboxLabel}), nil
}

func (i *instance) mailboxOutcome(b model.ProviderMailbox) adapter.Outcome {
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMailboxRef(i.ch.Ref, b.ID)}}
}

// labelNames is the instance's memory of label names by ID, so a state
// report names its labels without a read per report.
type labelNames struct {
	mu    sync.Mutex
	at    time.Time
	names map[string]string
}

// labelNamesTTL is how long a read of the label list serves state reports,
// and labelNamesRefresh how soon a label the list did not name, such as one
// made since, is looked for again.
const (
	labelNamesTTL     = 5 * time.Minute
	labelNamesRefresh = 30 * time.Second
)

// nameLabels sets Name of each provider label of st to the name Gmail gives
// its ID, reading the label list when the last read is older than
// labelNamesTTL, or older than labelNamesRefresh and names no such ID. A
// list it cannot read leaves the names as the IDs, and the report goes on.
func (i *instance) nameLabels(ctx context.Context, s *session, st *model.ProviderConversationState) {
	if len(st.Labels) == 0 {
		return
	}
	names, age := i.names.get(i.now())
	if names == nil || age > labelNamesTTL ||
		age > labelNamesRefresh && slices.ContainsFunc(st.Labels, func(l model.ProviderLabel) bool { _, ok := names[l.ID]; return !ok }) {
		boxes, err := i.listLabels(ctx, s)
		if err != nil {
			return
		}
		names = make(map[string]string, len(boxes))
		for _, b := range boxes {
			names[b.ID] = b.Name
		}
		i.names.put(i.now(), names)
	}
	for k, l := range st.Labels {
		if n, ok := names[l.ID]; ok {
			st.Labels[k].Name = n
		}
	}
}

// get returns the remembered names and how old they are; nil when none.
func (l *labelNames) get(now time.Time) (map[string]string, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.names, now.Sub(l.at)
}

func (l *labelNames) put(now time.Time, names map[string]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at, l.names = now, names
}

// validateMailboxCreate checks the create action without I/O.
func validateMailboxCreate(action model.Extension) []model.ValidationIssue {
	if action.Version != ExtensionVersion {
		return nil
	}
	if _, issue := mailboxes.CheckCreate(action, model.MailboxLabel); issue != nil {
		return []model.ValidationIssue{*issue}
	}
	return nil
}

var _ adapter.ExtensionReader = (*instance)(nil)
