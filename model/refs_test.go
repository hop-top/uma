package model

import (
	"strings"
	"testing"
)

type typedRef interface {
	Scoped
	Validate() error
	ValidateScope() error
	IsZero() bool
	Ref() Ref
}

func wrapNew[P any, T typedRef](f func(P, ID) (T, error)) func(P, ID) (typedRef, error) {
	return func(p P, id ID) (typedRef, error) { return f(p, id) }
}

func wrapAs[T typedRef](f func(Ref) (T, error)) func(Ref) (typedRef, error) {
	return func(r Ref) (typedRef, error) { return f(r) }
}

type tenantRefCase struct {
	kind ResourceKind
	new  func(TenantID, ID) (typedRef, error)
	lit  func(TenantID, ID) typedRef
	as   func(Ref) (typedRef, error)
}

var tenantRefCases = []tenantRefCase{
	{resourceChannel, wrapNew(NewChannelRef),
		func(t TenantID, id ID) typedRef { return ChannelRef{Tenant: t, ID: id} }, wrapAs(Ref.AsChannel)},
	{resourceInbox, wrapNew(NewInboxRef),
		func(t TenantID, id ID) typedRef { return InboxRef{Tenant: t, ID: id} }, wrapAs(Ref.AsInbox)},
	{resourceContact, wrapNew(NewContactRef),
		func(t TenantID, id ID) typedRef { return ContactRef{Tenant: t, ID: id} }, wrapAs(Ref.AsContact)},
	{resourceOperation, wrapNew(NewOperationRef),
		func(t TenantID, id ID) typedRef { return OperationRef{Tenant: t, ID: id} }, wrapAs(Ref.AsOperation)},
	{resourceSubscription, wrapNew(NewSubscriptionRef),
		func(t TenantID, id ID) typedRef { return SubscriptionRef{Tenant: t, ID: id} }, wrapAs(Ref.AsSubscription)},
	{resourceExport, wrapNew(NewExportRef),
		func(t TenantID, id ID) typedRef { return ExportRef{Tenant: t, ID: id} }, wrapAs(Ref.AsExport)},
	{resourceLabel, wrapNew(NewLabelRef),
		func(t TenantID, id ID) typedRef { return LabelRef{Tenant: t, ID: id} }, wrapAs(Ref.AsLabel)},
}

func TestTenantScopedRefs(t *testing.T) {
	for _, tc := range tenantRefCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			r, err := tc.new(tenantA, idB)
			requireOK(t, err)
			if r.Scope() != tenantA || r.IsZero() {
				t.Fatalf("scope = %q, zero = %v", r.Scope(), r.IsZero())
			}
			want := Ref{Tenant: tenantA, Kind: tc.kind, ID: idB}
			if got := r.Ref(); got != want {
				t.Fatalf("Ref() = %+v, want %+v", got, want)
			}
			back, err := tc.as(r.Ref())
			requireOK(t, err)
			if back != r {
				t.Fatalf("round trip = %+v, want %+v", back, r)
			}

			for name, in := range map[string]struct {
				tenant TenantID
				id     ID
				field  string
			}{
				"empty tenant": {"", idB, "tenant"},
				"bad tenant":   {" x", idB, "tenant"},
				"empty id":     {tenantA, "", "id"},
				"bad id":       {tenantA, badID, "id"},
			} {
				t.Run(name, func(t *testing.T) {
					got, err := tc.new(in.tenant, in.id)
					requireError(t, err, Validation, in.field)
					if !got.IsZero() {
						t.Fatalf("constructor returned %+v with error", got)
					}
					requireError(t, tc.lit(in.tenant, in.id).Validate(), Validation, in.field)
				})
			}

			requireOK(t, tc.lit(tenantA, "").ValidateScope())
			requireError(t, tc.lit(tenantA, badID).ValidateScope(), Validation, "id")
			requireError(t, tc.lit("", "").ValidateScope(), Validation, "tenant")
			if !tc.lit("", "").IsZero() {
				t.Fatal("zero literal not IsZero")
			}
		})
	}
}

type inboxRefCase struct {
	kind ResourceKind
	new  func(InboxRef, ID) (typedRef, error)
	lit  func(InboxRef, ID) typedRef
	in   func(InboxRef, ID, InboxRef) bool
	as   func(Ref) (typedRef, error)
}

var inboxRefCases = []inboxRefCase{
	{resourceIdentity, wrapNew(NewIdentityRef),
		func(p InboxRef, id ID) typedRef { return IdentityRef{Inbox: p, ID: id} },
		func(p InboxRef, id ID, q InboxRef) bool { return IdentityRef{Inbox: p, ID: id}.In(q) },
		wrapAs(Ref.AsIdentity)},
	{resourceConversation, wrapNew(NewConversationRef),
		func(p InboxRef, id ID) typedRef { return ConversationRef{Inbox: p, ID: id} },
		func(p InboxRef, id ID, q InboxRef) bool { return ConversationRef{Inbox: p, ID: id}.In(q) },
		wrapAs(Ref.AsConversation)},
	{resourceAttachment, wrapNew(NewAttachmentRef),
		func(p InboxRef, id ID) typedRef { return AttachmentRef{Inbox: p, ID: id} },
		func(p InboxRef, id ID, q InboxRef) bool { return AttachmentRef{Inbox: p, ID: id}.In(q) },
		wrapAs(Ref.AsAttachment)},
}

func TestInboxScopedRefs(t *testing.T) {
	for _, tc := range inboxRefCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			r, err := tc.new(inboxA, idB)
			requireOK(t, err)
			if r.Scope() != tenantA || r.IsZero() {
				t.Fatalf("scope = %q, zero = %v", r.Scope(), r.IsZero())
			}
			want := Ref{Tenant: tenantA, Kind: tc.kind, Inbox: inboxA.ID, ID: idB}
			if got := r.Ref(); got != want {
				t.Fatalf("Ref() = %+v, want %+v", got, want)
			}
			back, err := tc.as(r.Ref())
			requireOK(t, err)
			if back != r {
				t.Fatalf("round trip = %+v, want %+v", back, r)
			}

			for name, in := range map[string]struct {
				parent InboxRef
				id     ID
				field  string
			}{
				"parent tenant": {InboxRef{ID: idA}, idB, "inbox.tenant"},
				"parent id":     {InboxRef{Tenant: tenantA}, idB, "inbox.id"},
				"parent bad id": {InboxRef{Tenant: tenantA, ID: badID}, idB, "inbox.id"},
				"empty id":      {inboxA, "", "id"},
				"bad id":        {inboxA, badID, "id"},
			} {
				t.Run(name, func(t *testing.T) {
					got, err := tc.new(in.parent, in.id)
					requireError(t, err, Validation, in.field)
					if !got.IsZero() {
						t.Fatalf("constructor returned %+v with error", got)
					}
					requireError(t, tc.lit(in.parent, in.id).Validate(), Validation, in.field)
				})
			}

			requireOK(t, tc.lit(inboxA, "").ValidateScope())
			requireError(t, tc.lit(inboxA, badID).ValidateScope(), Validation, "id")
			requireError(t, tc.lit(InboxRef{Tenant: tenantA}, "").ValidateScope(), Validation, "inbox.id")

			if !tc.in(inboxA, idB, inboxA) {
				t.Fatal("In(own inbox) = false")
			}
			if tc.in(inboxA, idB, inboxB) {
				t.Fatal("In(other inbox) = true")
			}
			if tc.in(inboxA, idB, InboxRef{Tenant: tenantB, ID: inboxA.ID}) {
				t.Fatal("In(same inbox ID, other tenant) = true")
			}
		})
	}
}

type convRefCase struct {
	kind ResourceKind
	new  func(ConversationRef, ID) (typedRef, error)
	lit  func(ConversationRef, ID) typedRef
	in   func(ConversationRef, ID, ConversationRef) bool
}

var convRefCases = []convRefCase{
	{resourceMessage, wrapNew(NewMessageRef),
		func(p ConversationRef, id ID) typedRef { return MessageRef{Conversation: p, ID: id} },
		func(p ConversationRef, id ID, q ConversationRef) bool {
			return MessageRef{Conversation: p, ID: id}.In(q)
		}},
	{resourceTask, wrapNew(NewTaskRef),
		func(p ConversationRef, id ID) typedRef { return TaskRef{Conversation: p, ID: id} },
		func(p ConversationRef, id ID, q ConversationRef) bool { return TaskRef{Conversation: p, ID: id}.In(q) }},
	{resourceWorkItem, wrapNew(NewWorkItemRef),
		func(p ConversationRef, id ID) typedRef { return WorkItemRef{Conversation: p, ID: id} },
		func(p ConversationRef, id ID, q ConversationRef) bool {
			return WorkItemRef{Conversation: p, ID: id}.In(q)
		}},
	{resourceStream, wrapNew(NewStreamRef),
		func(p ConversationRef, id ID) typedRef { return StreamRef{Conversation: p, ID: id} },
		func(p ConversationRef, id ID, q ConversationRef) bool {
			return StreamRef{Conversation: p, ID: id}.In(q)
		}},
}

func TestConversationScopedRefs(t *testing.T) {
	for _, tc := range convRefCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			r, err := tc.new(convA, idC)
			requireOK(t, err)
			if r.Scope() != tenantA || r.IsZero() {
				t.Fatalf("scope = %q, zero = %v", r.Scope(), r.IsZero())
			}
			// The generic form keeps tenant, kind, inbox and ID; the conversation
			// lives on the typed ref only.
			want := Ref{Tenant: tenantA, Kind: tc.kind, Inbox: inboxA.ID, ID: idC}
			if got := r.Ref(); got != want {
				t.Fatalf("Ref() = %+v, want %+v", got, want)
			}
			requireOK(t, r.Ref().Validate())

			for name, in := range map[string]struct {
				parent ConversationRef
				id     ID
				field  string
			}{
				"chain tenant": {ConversationRef{Inbox: InboxRef{ID: idA}, ID: idB}, idC, "conversation.inbox.tenant"},
				"chain inbox":  {ConversationRef{Inbox: InboxRef{Tenant: tenantA}, ID: idB}, idC, "conversation.inbox.id"},
				"parent id":    {ConversationRef{Inbox: inboxA}, idC, "conversation.id"},
				"parent bad":   {ConversationRef{Inbox: inboxA, ID: badID}, idC, "conversation.id"},
				"empty id":     {convA, "", "id"},
				"bad id":       {convA, badID, "id"},
			} {
				t.Run(name, func(t *testing.T) {
					got, err := tc.new(in.parent, in.id)
					requireError(t, err, Validation, in.field)
					if !got.IsZero() {
						t.Fatalf("constructor returned %+v with error", got)
					}
					requireError(t, tc.lit(in.parent, in.id).Validate(), Validation, in.field)
				})
			}

			requireOK(t, tc.lit(convA, "").ValidateScope())
			requireError(t, tc.lit(convA, badID).ValidateScope(), Validation, "id")
			requireError(t, tc.lit(ConversationRef{Inbox: inboxA}, "").ValidateScope(), Validation, "conversation.id")

			if !tc.in(convA, idC, convA) {
				t.Fatal("In(own conversation) = false")
			}
			if tc.in(convA, idC, convB) {
				t.Fatal("In(sibling conversation) = true")
			}
			if tc.in(convA, idC, ConversationRef{Inbox: inboxB, ID: convA.ID}) {
				t.Fatal("In(same conversation ID, other inbox) = true")
			}
		})
	}
}

func TestRefAsRejects(t *testing.T) {
	channel := Ref{Tenant: tenantA, Kind: resourceChannel, ID: idA}
	identity := Ref{Tenant: tenantA, Kind: resourceIdentity, Inbox: idA, ID: idB}
	cases := map[string]struct {
		as    func() error
		field string
	}{
		"kind mismatch": {func() error { _, err := channel.AsInbox(); return err }, "kind"},
		"identity as conversation": {func() error {
			_, err := identity.AsConversation()
			return err
		}, "kind"},
		"missing scope": {func() error {
			r := identity
			r.Inbox = ""
			_, err := r.AsIdentity()
			return err
		}, "inbox"},
		"forbidden scope": {func() error {
			r := channel
			r.Inbox = idB
			_, err := r.AsChannel()
			return err
		}, "inbox"},
		"bad inbox": {func() error {
			r := identity
			r.Inbox = badID
			_, err := r.AsIdentity()
			return err
		}, "inbox"},
		"bad id": {func() error {
			r := channel
			r.ID = badID
			_, err := r.AsChannel()
			return err
		}, "id"},
		"empty id": {func() error {
			r := identity
			r.ID = ""
			_, err := r.AsIdentity()
			return err
		}, "id"},
		"bad tenant": {func() error {
			r := channel
			r.Tenant = ""
			_, err := r.AsChannel()
			return err
		}, "tenant"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) { requireError(t, tc.as(), Validation, tc.field) })
	}
}

func TestRefAsRejectsEveryWrongKind(t *testing.T) {
	as := map[ResourceKind]func(Ref) (typedRef, error){}
	for _, tc := range tenantRefCases {
		as[tc.kind] = tc.as
	}
	for _, tc := range inboxRefCases {
		as[tc.kind] = tc.as
	}
	stream := Ref{Tenant: tenantA, Kind: resourceStream, Inbox: idA, ID: idB}
	for kind, f := range as {
		t.Run(string(kind), func(t *testing.T) {
			got, err := f(stream)
			requireError(t, err, Validation, "kind")
			if !got.IsZero() {
				t.Fatalf("returned %+v with error", got)
			}
		})
	}
}

func TestRefValidate(t *testing.T) {
	cases := map[string]struct {
		ref   Ref
		field string // empty means valid
	}{
		"channel":              {Ref{Tenant: tenantA, Kind: resourceChannel, ID: idA}, ""},
		"message":              {Ref{Tenant: tenantA, Kind: resourceMessage, Inbox: idA, ID: idB}, ""},
		"extension":            {Ref{Tenant: tenantA, Kind: "com.example.widget", ID: idA}, ""},
		"extension with inbox": {Ref{Tenant: tenantA, Kind: "com.example.widget", Inbox: idB, ID: idA}, ""},
		"empty tenant":         {Ref{Kind: resourceChannel, ID: idA}, "tenant"},
		"empty kind":           {Ref{Tenant: tenantA, ID: idA}, "kind"},
		"unknown kind":         {Ref{Tenant: tenantA, Kind: "widget", ID: idA}, "kind"},
		"two segment kind":     {Ref{Tenant: tenantA, Kind: "example.widget", ID: idA}, "kind"},
		"uppercase kind":       {Ref{Tenant: tenantA, Kind: "com.Example.widget", ID: idA}, "kind"},
		"extension bad inbox":  {Ref{Tenant: tenantA, Kind: "com.example.widget", Inbox: badID, ID: idA}, "inbox"},
		"message no inbox":     {Ref{Tenant: tenantA, Kind: resourceMessage, ID: idB}, "inbox"},
		"tenant kind inbox":    {Ref{Tenant: tenantA, Kind: resourceContact, Inbox: idA, ID: idB}, "inbox"},
		"empty id":             {Ref{Tenant: tenantA, Kind: resourceChannel}, "id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.ref.Validate()
			if tc.field == "" {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, tc.field)
		})
	}
	scopeOnly := Ref{Tenant: tenantA, Kind: resourceConversation, Inbox: idA}
	requireOK(t, scopeOnly.ValidateScope())
	requireError(t, scopeOnly.Validate(), Validation, "id")
	if scopeOnly.Scope() != tenantA || scopeOnly.IsZero() || !(Ref{}).IsZero() {
		t.Fatal("Ref Scope/IsZero mismatch")
	}
}

func TestResourceKindValidate(t *testing.T) {
	for _, k := range []ResourceKind{
		resourceChannel, resourceInbox, resourceContact, resourceIdentity, resourceConversation,
		resourceMessage, resourceOperation, resourceTask, resourceWorkItem, resourceAttachment,
		resourceSubscription, resourceStream, resourceExport, resourceEvent, "org.example.sample.thing",
	} {
		requireOK(t, k.Validate())
	}
	long := ResourceKind("a.b." + strings.Repeat("c", 250))
	for _, k := range []ResourceKind{"", "Message", "messages", "a..b.c", ".a.b.c", "a.b.c.", "a.b c.d", long} {
		requireError(t, k.Validate(), Validation, "kind")
	}
}

func TestCheckScope(t *testing.T) {
	foreign := ContactRef{Tenant: tenantB, ID: idA}
	requireOK(t, CheckScope(tenantA))
	requireOK(t, CheckScope(tenantA, inboxA, convA, msgA, msgA.Ref()))
	requireError(t, CheckScope(tenantA, inboxA, foreign), NotFound, "")
	requireError(t, CheckScope(tenantA, MessageRef{}), NotFound, "")

	err := CheckScope(tenantA, foreign)
	if e, _ := err.(*Error); e.Retry != NoRetry || e.SafeMessage == "" {
		t.Fatalf("not_found error = %+v", e)
	}
}

func TestSameTenant(t *testing.T) {
	if !SameTenant(inboxA, ChannelRef{Tenant: tenantA, ID: idB}) {
		t.Fatal("same tenant reported different")
	}
	if SameTenant(inboxA, ContactRef{Tenant: tenantB, ID: idB}) {
		t.Fatal("different tenants reported same")
	}
	if SameTenant(InboxRef{}, ContactRef{}) {
		t.Fatal("unscoped refs reported same tenant")
	}
}

// Filter.Identity selects link history by identity: validated as a full
// identity ref and deep-copied by Clone.
func TestFilterIdentityValidateClone(t *testing.T) {
	id := IdentityRef{Inbox: inboxA, ID: idC}
	requireOK(t, Filter{Identity: &id}.Validate())
	requireOK(t, Filter{Identity: &id, Contact: &ContactRef{Tenant: tenantA, ID: idD}}.Validate())
	requireError(t, Filter{Identity: &IdentityRef{Inbox: inboxA}}.Validate(), Validation, "identity.id")
	requireError(t, Filter{Identity: &IdentityRef{ID: idC}}.Validate(), Validation, "identity.inbox.tenant")
	requireError(t, ListRequest{Call: call, Filter: Filter{Identity: &IdentityRef{Inbox: inboxA}}}.Validate(),
		Validation, "filter.identity.id")

	f := Filter{Identity: &id}
	c := f.Clone()
	if c.Identity == nil || *c.Identity != id {
		t.Fatalf("Clone dropped identity: %+v", c.Identity)
	}
	if c.Identity == f.Identity {
		t.Fatal("Clone aliases the identity ref")
	}
	c.Identity.ID = idD
	if f.Identity.ID != idC {
		t.Fatal("mutating the clone changed the original")
	}
}
