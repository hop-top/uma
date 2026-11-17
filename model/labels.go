package model

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Conversation labels.
//
// A label is tenant vocabulary: one definition (Label) reusable across every
// inbox of the tenant, never per inbox. A conversation holds labels as
// references in Conversation.Labels, so adding or removing one is a change
// of the conversation and moves its revision. Labels are canonical in uma
// for every channel, including channels whose provider has labels of its
// own: provider labels stay provider data and are never matched to uma
// labels by name.

// Label limits.
const (
	// MaxLabelNameRunes bounds Label.Name in runes; the name also follows
	// the 255-byte token rule.
	MaxLabelNameRunes = 64
	// MaxLabelDescriptionBytes bounds Label.Description.
	MaxLabelDescriptionBytes = 1024
	// MaxConversationLabels bounds the labels one conversation holds and
	// the labels one ConversationLabels request names.
	MaxConversationLabels = 64
	// MaxLabelSelectorPart bounds each of LabelSelector.Any, All and None.
	MaxLabelSelectorPart = 32
	// MaxLabelSelectorLabels bounds the distinct labels one LabelSelector
	// names across its parts.
	MaxLabelSelectorLabels = 64
)

// LabelRef is a tenant-scoped label reference.
type LabelRef struct {
	Tenant TenantID `json:"tenant"`
	ID     ID       `json:"id"`
}

// Label is a tenant-wide conversation label definition. Name is unique in
// the tenant under simple case folding (LabelNameKey): "Urgent" and
// "urgent" cannot both exist. It is stored as given. Description is
// optional raw text; Color is optional "#rrggbb" in lowercase hex. A
// configuration record, it carries no timestamps. Label names are tenant
// vocabulary shown to agents: they should carry no personal data.
type Label struct {
	Ref         LabelRef `json:"ref"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Color       string   `json:"color"`
	Revision    Revision `json:"revision"`
}

// LabelSelector narrows a query by conversation labels: the conjunction of
// its non-empty parts. Any matches a conversation holding at least one of
// its labels, All one holding every one of them, None one holding none of
// them. Labels are compared as a literal set: a label of the tenant that
// does not exist is held by no conversation, so Any ignores it, All
// matches nothing and None is unaffected.
//
// A set selector names at least one label; each part is unique and at most
// MaxLabelSelectorPart long, the parts name at most MaxLabelSelectorLabels
// distinct labels, and a label in None may not also be in Any or All.
type LabelSelector struct {
	Any  []LabelRef `json:"any,omitempty"`
	All  []LabelRef `json:"all,omitempty"`
	None []LabelRef `json:"none,omitempty"`
}

// ConfigureLabel creates a label (empty Label.Ref.ID) or replaces one at
// ExpectedRevision. Renaming is a replace: assignments reference the ID and
// are untouched. Label.Revision is ignored.
type ConfigureLabel struct {
	Command
	Label Label `json:"label"`
}

// DeleteLabel deletes a label definition at ExpectedRevision.
type DeleteLabel struct {
	Command
	Label LabelRef `json:"label"`
}

// ConversationLabels names the labels to add to or remove from a
// conversation (Conversations.AddLabels, RemoveLabels): 1 to
// MaxConversationLabels distinct labels of the conversation's tenant.
// ExpectedRevision is optional and, when set, is the conversation's
// revision.
type ConversationLabels struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Labels       []LabelRef      `json:"labels,omitempty"`
}

// NewLabelRef returns a validated label ref.
func NewLabelRef(t TenantID, id ID) (LabelRef, error) {
	return newValid(LabelRef{Tenant: t, ID: id})
}

// Validate checks tenant and ID.
func (r LabelRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the tenant; an empty ID (creation) is allowed.
func (r LabelRef) ValidateScope() error { return r.validate("", true) }

func (r LabelRef) validate(path string, leafOptional bool) error {
	return checkTenantScoped(path, r.Tenant, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r LabelRef) Scope() TenantID { return r.Tenant }

// IsZero reports whether r is unset.
func (r LabelRef) IsZero() bool { return r == LabelRef{} }

// Ref returns the generic form.
func (r LabelRef) Ref() Ref { return Ref{Tenant: r.Tenant, Kind: resourceLabel, ID: r.ID} }

// AsLabel converts a label Ref after checking kind, scope and ID.
func (r Ref) AsLabel() (LabelRef, error) {
	if err := r.expect(resourceLabel); err != nil {
		return LabelRef{}, err
	}
	return LabelRef{Tenant: r.Tenant, ID: r.ID}, nil
}

// Validate checks a stored label: a full ref, the name, description and
// color rules.
func (l Label) Validate() error { return l.validate("", false) }

func (l Label) validate(path string, leafOptional bool) error {
	return firstError(
		l.Ref.validate(fieldPath(path, "ref"), leafOptional),
		checkLabelName(fieldPath(path, "name"), l.Name),
		checkLabelDescription(fieldPath(path, "description"), l.Description),
		checkLabelColor(fieldPath(path, "color"), l.Color),
	)
}

func checkLabelName(field, name string) error {
	if err := checkToken(field, name); err != nil {
		return err
	}
	if utf8.RuneCountInString(name) > MaxLabelNameRunes {
		return invalidField(field, "exceeds "+strconv.Itoa(MaxLabelNameRunes)+" characters")
	}
	return nil
}

func checkLabelDescription(field, d string) error {
	if d == "" {
		return nil
	}
	if p := rawProblem(d, MaxLabelDescriptionBytes); p != "" {
		return invalidField(field, p)
	}
	return nil
}

func checkLabelColor(field, c string) error {
	if c == "" {
		return nil
	}
	if len(c) != 7 || c[0] != '#' {
		return invalidField(field, "must be #rrggbb lowercase hex")
	}
	for i := 1; i < len(c); i++ {
		if b := c[i]; (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return invalidField(field, "must be #rrggbb lowercase hex")
		}
	}
	return nil
}

// LabelNameKey returns the key label names are unique under: each rune
// replaced by the smallest rune of its unicode.SimpleFold orbit, so every
// case variant of a name shares one key. There is no normalization:
// precomposed and decomposed forms of a character are different names.
func LabelNameKey(name string) string { return strings.Map(minFold, name) }

// minFold returns the smallest rune of r's simple case folding orbit.
func minFold(r rune) rune {
	m := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		m = min(m, f)
	}
	return m
}

// ValidateLabels checks Conversation.Labels: at most
// MaxConversationLabels valid refs of the conversation's tenant, sorted by
// ID with no duplicates.
func (c Conversation) ValidateLabels() error {
	if len(c.Labels) > MaxConversationLabels {
		return invalidField("labels", "exceeds "+strconv.Itoa(MaxConversationLabels)+" labels")
	}
	tenant := c.Ref.Inbox.Tenant
	for i, l := range c.Labels {
		field := "labels[" + strconv.Itoa(i) + "]"
		if err := l.validate(field, false); err != nil {
			return err
		}
		if l.Tenant != tenant {
			return invalidField(field, "must belong to the conversation's tenant")
		}
		if i > 0 && l.ID <= c.Labels[i-1].ID {
			return invalidField(field, "must be sorted by ID without duplicates")
		}
	}
	return nil
}

// Validate checks the selector as Filter.Labels does.
func (s LabelSelector) Validate() error { return s.validate("labels") }

func (s LabelSelector) validate(path string) error {
	if len(s.Any)+len(s.All)+len(s.None) == 0 {
		return invalidField(path, "must name at least one label")
	}
	held := map[LabelRef]bool{}
	for _, part := range []struct {
		name   string
		labels []LabelRef
	}{{"any", s.Any}, {"all", s.All}, {"none", s.None}} {
		field := fieldPath(path, part.name)
		if len(part.labels) > MaxLabelSelectorPart {
			return invalidField(field, "exceeds "+strconv.Itoa(MaxLabelSelectorPart)+" labels")
		}
		seen := make(map[LabelRef]bool, len(part.labels))
		for i, l := range part.labels {
			item := field + "[" + strconv.Itoa(i) + "]"
			if err := l.validate(item, false); err != nil {
				return err
			}
			if seen[l] {
				return invalidField(item, "duplicate label")
			}
			seen[l] = true
			if part.name == "none" && held[l] {
				return invalidField(item, "also required by any or all")
			}
		}
		for l := range seen {
			held[l] = true
		}
	}
	if len(held) > MaxLabelSelectorLabels {
		return invalidField(path, "exceeds "+strconv.Itoa(MaxLabelSelectorLabels)+" distinct labels")
	}
	return nil
}

// CheckScope returns a not_found error when the selector names a label of
// a tenant other than t, as CheckScope does for refs.
func (s LabelSelector) CheckScope(t TenantID) error {
	for _, part := range [][]LabelRef{s.Any, s.All, s.None} {
		for _, l := range part {
			if err := CheckScope(t, l); err != nil {
				return err
			}
		}
	}
	return nil
}

// RevisionRule is RevisionAbsent for creation (empty label ID) and
// RevisionRequired for updates.
func (r ConfigureLabel) RevisionRule() RevisionRule { return createOrUpdate(r.Label.Ref.ID) }

// Validate checks a label create or update request.
func (r ConfigureLabel) Validate() error {
	return firstError(r.Command.Validate(r.RevisionRule()), r.Label.validate("label", true))
}

// RevisionRule is RevisionRequired: deletes.
func (DeleteLabel) RevisionRule() RevisionRule { return RevisionRequired }

// Validate checks the label ref.
func (r DeleteLabel) Validate() error {
	return firstError(r.Command.Validate(r.RevisionRule()), r.Label.validate("label", false))
}

// RevisionRule is RevisionOptional: adding a held label or removing an
// absent one is a no-op, so a caller may act without reading first.
func (ConversationLabels) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the conversation and 1 to MaxConversationLabels distinct
// labels of its tenant.
func (r ConversationLabels) Validate() error {
	if err := firstError(r.Command.Validate(r.RevisionRule()), r.Conversation.validate("conversation", false)); err != nil {
		return err
	}
	switch {
	case len(r.Labels) == 0:
		return invalidField("labels", "required")
	case len(r.Labels) > MaxConversationLabels:
		return invalidField("labels", "exceeds "+strconv.Itoa(MaxConversationLabels)+" labels")
	}
	seen := make(map[LabelRef]bool, len(r.Labels))
	for i, l := range r.Labels {
		field := "labels[" + strconv.Itoa(i) + "]"
		if err := l.validate(field, false); err != nil {
			return err
		}
		if l.Tenant != r.Conversation.Inbox.Tenant {
			return invalidField(field, "must belong to the conversation's tenant")
		}
		if seen[l] {
			return invalidField(field, "duplicate label")
		}
		seen[l] = true
	}
	return nil
}

// Clone returns a deep copy.
func (s LabelSelector) Clone() LabelSelector {
	s.Any = cloneEach(s.Any, same)
	s.All = cloneEach(s.All, same)
	s.None = cloneEach(s.None, same)
	return s
}
