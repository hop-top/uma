package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func stateCmd() Command { return Command{Call: Call{Correlation: "corr-1"}, Operation: "op-1"} }

// stateRequest is the shared shape of the five state requests.
type stateRequest interface{ Validate() error }

func TestStateRequestsValidate(t *testing.T) {
	type tc struct {
		name, field string
		req         func() stateRequest
	}
	archive := func(f func(*ArchiveConversation)) func() stateRequest {
		return func() stateRequest {
			r := ArchiveConversation{Command: stateCmd(), Conversation: convA}
			if f != nil {
				f(&r)
			}
			return r
		}
	}
	move := func(f func(*MoveConversation)) func() stateRequest {
		return func() stateRequest {
			r := MoveConversation{Command: stateCmd(), Conversation: convA, Destination: MoveDestination{Role: RoleArchive}}
			if f != nil {
				f(&r)
			}
			return r
		}
	}
	read := func(f func(*SetConversationRead)) func() stateRequest {
		return func() stateRequest {
			r := SetConversationRead{Command: stateCmd(), Conversation: convA, Read: true}
			if f != nil {
				f(&r)
			}
			return r
		}
	}
	flag := func(f func(*FlagConversation)) func() stateRequest {
		return func() stateRequest {
			r := FlagConversation{Command: stateCmd(), Conversation: convA, Kind: FlagStar}
			if f != nil {
				f(&r)
			}
			return r
		}
	}
	trash := func(f func(*TrashConversation)) func() stateRequest {
		return func() stateRequest {
			r := TrashConversation{Command: stateCmd(), Conversation: convA}
			if f != nil {
				f(&r)
			}
			return r
		}
	}
	for _, v := range []func() stateRequest{
		archive(nil), archive(func(r *ArchiveConversation) { r.Undo = true; r.ExpectedRevision = "r1" }),
		move(nil), move(func(r *MoveConversation) { r.Destination = MoveDestination{ID: "Label_1"} }),
		move(func(r *MoveConversation) {
			r.Destination = MoveDestination{}
			r.Add = []string{"a"}
			r.Remove = []string{"b"}
		}),
		read(nil), read(func(r *SetConversationRead) { r.Read = false }),
		flag(nil), flag(func(r *FlagConversation) { r.Kind = FlagImportant; r.Undo = true }),
		trash(nil), trash(func(r *TrashConversation) { r.Undo = true }),
	} {
		requireOK(t, v().Validate())
	}
	for _, c := range []tc{
		{"archive no operation", "operation", archive(func(r *ArchiveConversation) { r.Operation = "" })},
		{"archive no conversation", "conversation.inbox.tenant", archive(func(r *ArchiveConversation) { r.Conversation = ConversationRef{} })},
		{"move no operation", "operation", move(func(r *MoveConversation) { r.Operation = "" })},
		{"move no conversation", "conversation.inbox.tenant", move(func(r *MoveConversation) { r.Conversation = ConversationRef{} })},
		{"move nothing", "destination", move(func(r *MoveConversation) { r.Destination = MoveDestination{} })},
		{"move id and role", "destination", move(func(r *MoveConversation) { r.Destination.ID = "x" })},
		{"move unknown role", "destination.role", move(func(r *MoveConversation) { r.Destination.Role = "spam" })},
		{"move destination and add", "destination", move(func(r *MoveConversation) { r.Add = []string{"a"} })},
		{"move empty label", "add", move(func(r *MoveConversation) { r.Destination = MoveDestination{}; r.Add = []string{""} })},
		{"move repeated label", "add", move(func(r *MoveConversation) { r.Destination = MoveDestination{}; r.Add = []string{"a", "a"} })},
		{"move label added and removed", "remove", move(func(r *MoveConversation) {
			r.Destination = MoveDestination{}
			r.Add, r.Remove = []string{"a"}, []string{"a"}
		})},
		{"read no operation", "operation", read(func(r *SetConversationRead) { r.Operation = "" })},
		{"read no conversation", "conversation.inbox.tenant", read(func(r *SetConversationRead) { r.Conversation = ConversationRef{} })},
		{"flag no operation", "operation", flag(func(r *FlagConversation) { r.Operation = "" })},
		{"flag no conversation", "conversation.inbox.tenant", flag(func(r *FlagConversation) { r.Conversation = ConversationRef{} })},
		{"flag no kind", "kind", flag(func(r *FlagConversation) { r.Kind = "" })},
		{"flag unknown kind", "kind", flag(func(r *FlagConversation) { r.Kind = "bookmark" })},
		{"flag pin kind", "kind", flag(func(r *FlagConversation) { r.Kind = "pin" })},
		{"trash no operation", "operation", trash(func(r *TrashConversation) { r.Operation = "" })},
		{"trash no conversation", "conversation.inbox.tenant", trash(func(r *TrashConversation) { r.Conversation = ConversationRef{} })},
	} {
		t.Run(c.name, func(t *testing.T) { requireError(t, c.req().Validate(), Validation, c.field) })
	}
}

func TestStateVerbCapabilityAndScope(t *testing.T) {
	for v, want := range map[StateVerb]Capability{
		StateVerbArchive: CapabilityArchive, StateVerbUnarchive: CapabilityArchive,
		StateVerbMove: CapabilityMove, StateVerbRead: CapabilityReadState, StateVerbUnread: CapabilityReadState,
		StateVerbFlag: CapabilityFlag, StateVerbUnflag: CapabilityFlag,
		StateVerbTrash: CapabilityTrash, StateVerbUntrash: CapabilityTrash,
	} {
		if got, ok := v.Capability(); !ok || got != want {
			t.Errorf("%s.Capability() = %q, %v; want %q", v, got, ok, want)
		}
	}
	if _, ok := StateVerb("purge").Capability(); ok {
		t.Error("undeclared verb has a capability")
	}
	for s, want := range map[StateScope]string{ScopeAccount: ActionConversationStateAccount, ScopeShared: ActionConversationStateShared} {
		if got, ok := s.Action(); !ok || got != want || !s.Valid() {
			t.Errorf("%s.Action() = %q, %v; want %q", s, got, ok, want)
		}
	}
	if _, ok := StateScope("both").Action(); ok || StateScope("").Valid() {
		t.Error("undeclared scope accepted")
	}
	if !MoveShapeLabels.Valid() || !MoveShapeFolder.Valid() || MoveShape("tree").Valid() {
		t.Error("MoveShape.Valid")
	}
}

func TestProviderConversationStateValidate(t *testing.T) {
	yes := true
	valid := func() ProviderConversationState {
		return ProviderConversationState{
			Channel: ChannelRef{Tenant: tenantA, ID: idA}, Archived: true, Read: &yes,
			Labels: []ProviderLabel{{ID: "INBOX"}, {ID: "STARRED"}}, Flags: []string{FlagStar},
			ObservedAt: time.Unix(1, 0).UTC(), Complete: true,
		}
	}
	requireOK(t, valid().Validate())
	folder := valid()
	folder.Labels = nil
	folder.Folder = &Folder{ID: "f1", Role: RoleArchive}
	requireOK(t, folder.Validate())
	for _, c := range []struct {
		name, field string
		mut         func(*ProviderConversationState)
	}{
		{"no channel", "channel.tenant", func(s *ProviderConversationState) { s.Channel = ChannelRef{} }},
		{"folder and labels", "labels", func(s *ProviderConversationState) { s.Folder = &Folder{ID: "f1"} }},
		{"folder without id", "folder.id", func(s *ProviderConversationState) { s.Labels = nil; s.Folder = &Folder{} }},
		{"folder role", "folder.role", func(s *ProviderConversationState) { s.Labels = nil; s.Folder = &Folder{ID: "f", Role: "x"} }},
		{"label without id", "labels.id", func(s *ProviderConversationState) { s.Labels = []ProviderLabel{{}} }},
		{"repeated label", "labels", func(s *ProviderConversationState) { s.Labels = []ProviderLabel{{ID: "a"}, {ID: "a"}} }},
		{"unknown flag", "flags", func(s *ProviderConversationState) { s.Flags = []string{"x"} }},
		{"repeated flag", "flags", func(s *ProviderConversationState) { s.Flags = []string{FlagImportant, FlagImportant} }},
		{"no observation time", "observed_at", func(s *ProviderConversationState) { s.ObservedAt = time.Time{} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := valid()
			c.mut(&s)
			requireError(t, s.Validate(), Validation, c.field)
		})
	}
}

func TestProviderStateReportValidate(t *testing.T) {
	ref := ExternalRef{Channel: ChannelRef{Tenant: tenantA, ID: idA}, Namespace: "test.provider", Kind: RefKindRoom, ID: "r1"}
	r := ProviderStateReport{Conversation: ref, State: ProviderConversationState{ObservedAt: time.Unix(1, 0).UTC()}}
	requireOK(t, r.Validate())
	bad := r
	bad.Conversation = ExternalRef{}
	if bad.Validate() == nil {
		t.Error("report without a conversation reference accepted")
	}
	bad = r
	bad.State.ObservedAt = time.Time{}
	requireError(t, bad.Validate(), Validation, "state.observed_at")
}

func TestStateLimitsRoundTrip(t *testing.T) {
	in := StateLimits{Scope: ScopeAccount, Shape: MoveShapeFolder, Roles: []string{RoleArchive}, Kinds: []string{FlagStar}}
	got, err := ParseStateLimits(in.Extension())
	if err != nil || got.Scope != in.Scope || got.Shape != in.Shape || len(got.Roles) != 1 || len(got.Kinds) != 1 {
		t.Fatalf("ParseStateLimits = %+v, %v, want %+v", got, err, in)
	}
}

func TestParseStateLimitsFailsClosed(t *testing.T) {
	ext := func(data string) Extension { return Extension{Name: StateLimitsName, Version: 1, Data: []byte(data)} }
	for name, e := range map[string]Extension{
		"zero":         {},
		"other name":   {Name: "acme.limits", Version: 1, Data: []byte(`{"scope":"account"}`)},
		"unreadable":   ext(`{`),
		"no scope":     ext(`{}`),
		"bad scope":    ext(`{"scope":"world"}`),
		"bad shape":    ext(`{"scope":"account","shape":"tree"}`),
		"bad kind":     ext(`{"scope":"account","kinds":["love"]}`),
		"bad role":     ext(`{"scope":"account","roles":["spam"]}`),
		"wrong schema": {Name: StateLimitsName, Version: 2, Data: []byte(`{"scope":"account"}`)},
	} {
		if _, err := ParseStateLimits(e); !IsCode(err, Validation) {
			t.Errorf("%s: ParseStateLimits = %v, want validation", name, err)
		}
	}
}

func TestPendingStateValidate(t *testing.T) {
	op := OperationRef{Tenant: "t1", ID: "00000001-0000-7000-8000-000000000001"}
	ok := PendingState{Operation: op, Conversation: convA, Verb: StateVerbArchive}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid pending state: %v", err)
	}
	bad := ok
	bad.Verb = "shred"
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown verb accepted")
	}
	bad = ok
	bad.Operation = OperationRef{}
	if err := bad.Validate(); err == nil {
		t.Fatal("zero operation accepted")
	}
}

// A flag request carries a kind and no message reference: a body with a
// message field is refused by a strict decode, and pin is no kind.
func TestFlagConversationHasNoMessage(t *testing.T) {
	body := `{"operation":"op-1","conversation":{},"kind":"star","message":{}}`
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var r FlagConversation
	if err := dec.Decode(&r); err == nil || !strings.Contains(err.Error(), `"message"`) {
		t.Fatalf("decode = %v, want unknown field message", err)
	}
	if IsFlagKind("pin") {
		t.Fatal(`IsFlagKind("pin") = true, want false`)
	}
}

// The messages a state request names are tokens, none repeated, on every
// verb.
func TestStateRequestsValidateMessages(t *testing.T) {
	for name, msgs := range map[string][]ID{"empty": {""}, "repeated": {"m1", "m1"}} {
		for verb, r := range map[string]stateRequest{
			"archive": ArchiveConversation{Command: stateCmd(), Conversation: convA, Messages: msgs},
			"move":    MoveConversation{Command: stateCmd(), Conversation: convA, Destination: MoveDestination{Role: RoleArchive}, Messages: msgs},
			"read":    SetConversationRead{Command: stateCmd(), Conversation: convA, Messages: msgs},
			"flag":    FlagConversation{Command: stateCmd(), Conversation: convA, Kind: FlagStar, Messages: msgs},
			"trash":   TrashConversation{Command: stateCmd(), Conversation: convA, Messages: msgs},
		} {
			if err := r.Validate(); !IsCode(err, Validation) {
				t.Errorf("%s with %s messages: Validate = %v, want validation", verb, name, err)
			}
		}
	}
	ok := ArchiveConversation{Command: stateCmd(), Conversation: convA, Messages: []ID{"m1", "m2"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
}
