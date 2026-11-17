package model

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

var (
	allCapabilities = []Capability{
		CapabilityReceive, CapabilityReceiveHTTP, CapabilitySend, CapabilityEdit,
		CapabilityDelete, CapabilityReact, CapabilityReconcile, CapabilitySignal,
		CapabilitySendEphemeral, CapabilityStream, CapabilityExtensionQuery,
		CapabilityExtensionExecute, CapabilityProbePermissions,
		CapabilityRespond, CapabilityRespondFollowUp,
		CapabilityReceipt, CapabilityMembership, CapabilityHistory,
		CapabilityArchive, CapabilityMove, CapabilityReadState, CapabilityFlag, CapabilityTrash,
	}
	allResourceKinds = []ResourceKind{
		KindChannel, KindInbox, KindContact, KindIdentity, KindConversation, KindMessage,
		KindOperation, KindTask, KindWorkItem, KindAttachment, KindSubscription, KindStream,
		KindExport, KindLabel, KindEvent, KindInteraction,
	}
	allAreas  = []string{AreaExtension, AreaPrivacy, AreaDelivery, AreaTrust}
	allEvents = []string{
		EventMessageReceived, EventMessageCreated, EventMessageUpdated,
		EventConversationChanged, EventDeliveryUpdated, EventWorkItemUpdated,
		EventIdentityLinked, EventConversationLabelAdded, EventConversationLabelRemoved,
		EventTaskUpdated, EventContactCreated, EventContactUpdated, EventContactMerged,
		EventInteractionReceived, EventInteractionUpdated,
		EventConversationAssigned, EventConversationUnassigned, EventConversationSnoozed, EventConversationUnsnoozed,
		EventConversationLinked, EventConversationUnlinked,
		EventOperationHeld, EventOperationDecided, EventOperationExpired,
	}
	allSchemas = []string{
		SchemaMessageReceived, SchemaMessageCreated, SchemaMessageUpdated,
		SchemaConversationChanged, SchemaDeliveryUpdated, SchemaWorkItemUpdated,
		SchemaIdentityLinked, SchemaConversationLabelAdded, SchemaConversationLabelRemoved,
		SchemaTaskUpdated, SchemaContactCreated, SchemaContactUpdated, SchemaContactMerged,
		SchemaInteractionReceived, SchemaInteractionUpdated,
		SchemaConversationAssigned, SchemaConversationUnassigned, SchemaConversationSnoozed, SchemaConversationUnsnoozed,
		SchemaConversationLinked, SchemaConversationUnlinked,
		SchemaOperationHeld, SchemaOperationDecided, SchemaOperationExpired,
	}
	allActions = []string{
		ActionChannelConfigure, ActionChannelRead, ActionChannelReceive, ActionChannelImport, ActionInboxConfigure, ActionInboxRead,
		ActionContactRead, ActionContactReadAll, ActionContactWrite, ActionIdentityRead, ActionIdentityLink,
		ActionIdentityRelink, ActionIdentityLinkOverride, ActionIdentityUnlink,
		ActionIdentityHistory, ActionContactMerge, ActionConversationRead, ActionConversationTransition,
		ActionConversationSignal, ActionConversationStart, ActionConversationMembership, ActionMessageRead, ActionMessageReply, ActionMessageNote,
		ActionMessageEdit, ActionMessageDelete, ActionMessageReact, ActionMessageEphemeral, ActionMessageReceipt,
		ActionWorkItemRead, ActionWorkItemAct, ActionTaskRead, ActionTaskAct,
		ActionOperationRead, ActionOperationReplay, ActionOperationReconcile,
		ActionOperationApprove, ActionOperationReject, ActionOperationEdit, ActionTrustRead, ActionTrustReset, ActionTrustWrite, ActionSubscriptionOpen, ActionSubscriptionPoll,
		ActionSubscriptionAck, ActionSubscriptionClose, ActionAttachmentPrepare,
		ActionAttachmentUpload, ActionAttachmentRead, ActionStreamRead, ActionStreamWrite,
		ActionExtensionQuery, ActionExtensionExecute, ActionPrivacyRedact, ActionPrivacyRedactCascade, ActionPrivacyExport,
		ActionLabelRead, ActionLabelConfigure, ActionConversationLabel,
		ActionEventList,
		ActionInteractionRead, ActionInteractionRespond,
		ActionConversationAssign, ActionConversationSnooze, ActionConversationReadWorkflow,
		ActionConversationLink, ActionConversationMarkRead,
		ActionConversationStateAccount, ActionConversationStateShared,
	}
)

func TestMaintainedTypeKeys(t *testing.T) {
	want := []string{
		"native.webwidget", "smtp.mail", "imap.mail", "microsoft.mail", "google.mail",
		"twilio.sms", "twilio.whatsapp", "meta.whatsapp", "github.issues", "zendesk.tickets", "a2a.messages", "relay.messages",
		"http.mail", "slack.chat", "telegram.messages",
		"microsoft.teams", "discord.chat",
		"gitlab.issues",
		"jira.issues",
		"linear.issues",
	}
	requireKeys(t, MaintainedTypeKeys(), want)
	for _, k := range MaintainedTypeKeys() {
		if !IsMaintainedTypeKey(k) || IsReservedTypeKey(k) {
			t.Errorf("%q: maintained=%v reserved=%v", k, IsMaintainedTypeKey(k), IsReservedTypeKey(k))
		}
		requireOK(t, ValidateTypeKey(k))
	}
}

func TestReservedTypeKeys(t *testing.T) {
	want := []string{
		"meta.messenger", "meta.instagram", "line.messages",
		"irc.chat",
		"freshdesk.tickets", "intercom.conversations",
		"helpscout.conversations", "jira.tickets",
	}
	requireKeys(t, ReservedTypeKeys(), want)
	for _, k := range ReservedTypeKeys() {
		if IsMaintainedTypeKey(k) || !IsReservedTypeKey(k) {
			t.Errorf("%q: maintained=%v reserved=%v", k, IsMaintainedTypeKey(k), IsReservedTypeKey(k))
		}
		requireNameError(t, ValidateTypeKey(k), "type", "not implemented in this version")
	}
}

func TestTypeKeyListsAreCopies(t *testing.T) {
	MaintainedTypeKeys()[0] = "com.example.hijack.key"
	ReservedTypeKeys()[0] = "com.example.hijack.key"
	if MaintainedTypeKeys()[0] != TypeNativeWebWidget || ReservedTypeKeys()[0] != TypeMetaMessenger {
		t.Fatal("key lists share backing storage with callers")
	}
}

func TestIsTypeKeyRejectsOthers(t *testing.T) {
	for _, k := range []TypeKey{"", "smtp.email", "com.example.chat", "Twilio.sms"} {
		if IsMaintainedTypeKey(k) || IsReservedTypeKey(k) {
			t.Errorf("%q reported maintained or reserved", k)
		}
	}
}

func TestValidateTypeKeyAccepts(t *testing.T) {
	for _, k := range []TypeKey{
		"com.example.acmechat.messages",
		"org.example.sample.inbound",
		"com.example.smtp.mail",
		"com.example.twilio.sms",
		"com.example.microsoft.mail",
		"com.my-co.chat",
		"io.0xco.chat",
		"com.example.v",
		"com.example.v2beta",
		"com.example.chat.v2.messages",
		TypeKey(longName(253)),
	} {
		t.Run(string(k), func(t *testing.T) { requireOK(t, ValidateTypeKey(k)) })
	}
}

func TestValidateTypeKeyRejects(t *testing.T) {
	label64 := strings.Repeat("a", 64)
	for _, tc := range []struct {
		key    TypeKey
		reason string
	}{
		// Two segments: maintained allow-list only.
		{"acme.chat", "reserved for maintained adapters"},
		{"vonage.sms", "reserved for maintained adapters"},
		{"smtp.email", "reserved for maintained adapters"},
		{"imap.email", "reserved for maintained adapters"},
		{"google.email", "reserved for maintained adapters"},
		{"mailbox.mail", "reserved for maintained adapters"},
		{"telegram.chat", "reserved for maintained adapters"},
		{"line.chat", "reserved for maintained adapters"},
		{"microsoft.graph", "reserved for maintained adapters"},
		{"native.web_widget", "three or more"},
		{"native.web-widget", "three or more"},
		{"twilio", "three or more"},
		{"Twilio.sms", "three or more"},
		{"", "three or more"},
		// Grammar: lowercase DNS labels, no normalization.
		{"Com.example.chat", "three or more"},
		{"com.example.acme_chat", "three or more"},
		{"com..example.chat", "three or more"},
		{"com.-acme.chat", "three or more"},
		{"com.acme-.chat", "three or more"},
		{".com.acme.chat", "three or more"},
		{"com.acme.chat.", "three or more"},
		{"com/acme/chat", "three or more"},
		{"com.acme.chat ", "three or more"},
		{TypeKey("com." + label64 + ".chat"), "three or more"},
		{TypeKey(longName(254)), "maximum length"},
		// Reserved first segments.
		{"uma.foo.bar", "reserved segment"},
		{"native.foo.bar", "reserved segment"},
		// Maintained or reserved key as prefix.
		{"smtp.mail.relay", "maintained or reserved type key"},
		{"twilio.sms.rcs", "maintained or reserved type key"},
		{"microsoft.mail.shared", "maintained or reserved type key"},
		{"google.mail.v2", "maintained or reserved type key"},
		{"slack.chat.bridge", "maintained or reserved type key"},
		{"jira.tickets.service_desk", "three or more"},
		{"jira.tickets.servicedesk", "maintained or reserved type key"},
		// No version segment.
		{"com.acme.chat.v2", "version"},
		{"com.acme.chat.v10", "version"},
		{"com.acme.v0", "version"},
	} {
		t.Run(string(tc.key), func(t *testing.T) {
			requireNameError(t, ValidateTypeKey(tc.key), "type", tc.reason)
		})
	}
}

func TestValidateExtensionNameAccepts(t *testing.T) {
	for _, n := range []string{
		"github.issues.config",
		"zendesk.tickets.macro_apply",
		"twilio.sms.message.flagged",
		"org.example.sample.inbound.config",
		"com.example.acmechat.messages.config",
		"com.my-co.chat.thread_pin",
		"com.example.native.x",
		"com.example.v",
		longName(253),
	} {
		t.Run(n, func(t *testing.T) { requireOK(t, ValidateExtensionName(n)) })
	}
}

func TestValidateExtensionNameRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
	}{
		{"", "three or more"},
		{"receive", "reserved for core"},
		{"flag_spam", "reserved for core"},
		{"message.received", "reserved for core"},
		{"message.flag_spam", "reserved for core"},
		{"uma", "reserved for core"},
		{"uma.message.received", "uma namespace"},
		{"uma.foo.bar", "uma namespace"},
		{"com.example.foo.v2", "version"},
		{"github.issues.config.v1", "version"},
		{"Com.example.foo", "three or more"},
		{"com..example.foo", "three or more"},
		{"com.example.foo_", "three or more"},
		{"com.example._foo", "three or more"},
		{"com.example.-foo", "three or more"},
		{"com/example/foo", "three or more"},
		{"com.example." + strings.Repeat("a", 64), "three or more"},
		{longName(254), "maximum length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireNameError(t, ValidateExtensionName(tc.name), "name", tc.reason)
		})
	}
}

func TestBuiltinCapabilities(t *testing.T) {
	want := []string{
		"receive", "receive_http", "send", "edit", "delete", "react", "reconcile", "signal",
		"send_ephemeral", "stream", "extension_query", "extension_execute", "probe_permissions",
		"respond", "respond_follow_up",
		"receipt", "membership", "history",
		"archive", "move", "read_state", "flag", "trash",
	}
	if len(allCapabilities) != len(want) {
		t.Fatalf("%d capabilities, want %d", len(allCapabilities), len(want))
	}
	for i, c := range allCapabilities {
		if string(c) != want[i] {
			t.Errorf("capability %d = %q, want %q", i, c, want[i])
		}
		if strings.Contains(string(c), ".") {
			t.Errorf("capability %q is not dot-free", c)
		}
	}
}

func TestBuiltinResourceKindsAndAreas(t *testing.T) {
	want := []string{
		"channel", "inbox", "contact", "identity", "conversation", "message",
		"operation", "task", "work_item", "attachment", "subscription", "stream", "export", "label",
		"event",
		"interaction",
	}
	if len(allResourceKinds) != len(want) {
		t.Fatalf("%d kinds, want %d", len(allResourceKinds), len(want))
	}
	for i, k := range allResourceKinds {
		if string(k) != want[i] {
			t.Errorf("kind %d = %q, want %q", i, k, want[i])
		}
		requireOK(t, k.Validate())
	}
	for i, a := range []string{"extension", "privacy", "delivery", "trust"} {
		if allAreas[i] != a {
			t.Errorf("area %d = %q, want %q", i, allAreas[i], a)
		}
		if ResourceKind(a).Validate() == nil {
			t.Errorf("area %q is also a resource kind", a)
		}
	}
}

func TestBuiltinEventKinds(t *testing.T) {
	want := []string{
		"message.received", "message.created", "message.updated", "conversation.changed",
		"delivery.updated", "work_item.updated", "identity.linked",
		"conversation.label_added", "conversation.label_removed", "task.updated",
		"contact.created", "contact.updated", "contact.merged", "interaction.received", "interaction.updated",
		"conversation.assigned", "conversation.unassigned", "conversation.snoozed", "conversation.unsnoozed",
		"conversation.linked", "conversation.unlinked",
		"operation.held", "operation.decided", "operation.expired",
	}
	if len(allEvents) != len(want) {
		t.Fatalf("%d events, want %d", len(allEvents), len(want))
	}
	for i, e := range allEvents {
		if e != want[i] {
			t.Errorf("event %d = %q, want %q", i, e, want[i])
		}
		area, verb, ok := strings.Cut(e, ".")
		if !ok || strings.Contains(verb, ".") || (!strings.HasSuffix(verb, "ed") && verb != "held") {
			t.Errorf("event %q is not <area>.<past-tense verb> (held is the one irregular)", e)
		}
		if !isBuiltinArea(area) {
			t.Errorf("event %q: area %q is neither a resource kind nor an area", e, area)
		}
	}
}

func TestBuiltinActions(t *testing.T) {
	if len(allActions) != 69 {
		t.Fatalf("%d actions, want 69", len(allActions))
	}
	for _, a := range allActions {
		area, verb, ok := strings.Cut(a, ".")
		if !ok || verb == "" || strings.Contains(verb, ".") {
			t.Errorf("action %q is not <area>.<verb>", a)
		}
		if !isBuiltinArea(area) || area == AreaDelivery {
			t.Errorf("action %q: area %q is not an action area", a, area)
		}
	}
	for _, a := range []string{
		"contact.read_all", "identity.link", "identity.relink", "identity.link_override",
		"identity.unlink", "identity.history", "message.ephemeral", "privacy.export",
		"privacy.redact_cascade", "conversation.start", "label.read", "label.configure",
		"conversation.label", "event.list",
		"interaction.read", "interaction.respond",
		"conversation.assign", "conversation.snooze", "conversation.read_workflow",
		"conversation.link", "conversation.mark_read",
	} {
		if !contains(allActions, a) {
			t.Errorf("missing action %q", a)
		}
	}
}

func TestPayloadSchemas(t *testing.T) {
	if len(allSchemas) != len(allEvents) {
		t.Fatalf("%d schemas for %d events", len(allSchemas), len(allEvents))
	}
	for i, s := range allSchemas {
		if want := CoreNamespace + "." + allEvents[i]; s != want {
			t.Errorf("schema for %q = %q, want %q", allEvents[i], s, want)
		}
	}
	if CoreNamespace != "uma" {
		t.Errorf("CoreNamespace = %q", CoreNamespace)
	}
	// v1 is the only payload major and the only envelope version emitted.
	if PayloadVersion1 != 1 || EventEnvelopeVersion != 1 {
		t.Errorf("PayloadVersion1 = %d, EventEnvelopeVersion = %d", PayloadVersion1, EventEnvelopeVersion)
	}
	if MaxNameLength != 253 {
		t.Errorf("MaxNameLength = %d", MaxNameLength)
	}
}

// Core names cannot be declared outside core: every builtin name fails
// ValidateExtensionName, so an extension cannot shadow one.
func TestBuiltinNamesCannotBeShadowed(t *testing.T) {
	var names []string
	for _, c := range allCapabilities {
		names = append(names, string(c))
	}
	for _, k := range allResourceKinds {
		names = append(names, string(k))
	}
	names = append(names, allAreas...)
	names = append(names, allEvents...)
	names = append(names, allSchemas...)
	names = append(names, allActions...)
	names = append(names, ExtensionKindRead, ExtensionKindEffect, ExtensionKindEvent,
		PartText, PartAttachment, PartStructured, StartBeginning, StartTail, StartCheckpoint,
		CoreNamespace, NativeSource)
	for _, n := range names {
		if err := ValidateExtensionName(n); err == nil {
			t.Errorf("builtin name %q accepted as an extension name", n)
		}
	}
	for _, k := range append(MaintainedTypeKeys(), ReservedTypeKeys()...) {
		if err := ValidateExtensionName(string(k)); err == nil {
			t.Errorf("type key %q accepted as an extension name", k)
		}
	}
}

func TestEnumerationValues(t *testing.T) {
	for _, tc := range []struct {
		got, want []string
	}{
		{[]string{ExtensionKindRead, ExtensionKindEffect, ExtensionKindEvent}, []string{"read", "effect", "event"}},
		{[]string{PartText, PartAttachment, PartStructured}, []string{"text", "attachment", "structured"}},
		{[]string{StartBeginning, StartTail, StartCheckpoint}, []string{"beginning", "tail", "checkpoint"}},
		{[]string{TriggerProvider, TriggerCommand, TriggerSystem}, []string{"provider", "command", "system"}},
		{
			[]string{MessageOriginReply, MessageOriginNote, MessageOriginStart, MessageOriginTaskStatus, MessageOriginResponse},
			[]string{"reply", "note", "start", "task_status", "response"},
		},
		{
			[]string{InteractionChangeResponded, InteractionChangeFollowUp, InteractionChangeRedacted},
			[]string{"responded", "follow_up", "redacted"},
		},
		{
			[]string{MessageChangeEdited, MessageChangeDeleted, MessageChangeRedacted,
				MessageChangeReactionAdded, MessageChangeReactionRemoved, MessageChangeAudienceNarrowed,
				MessageChangeEcho, MessageChangeEditSuperseded},
			[]string{"edited", "deleted", "redacted", "reaction_added", "reaction_removed", "audience_narrowed",
				"echo", "edit_superseded"},
		},
		{
			[]string{ConversationFieldCreated, ConversationFieldStatus, ConversationFieldMode,
				ConversationFieldParticipants, ConversationFieldExternal, ConversationFieldReplyRoute},
			[]string{"created", "status", "mode", "participants", "external", "reply_route"},
		},
		{
			[]string{WorkItemFieldCreated, WorkItemFieldStatus, WorkItemFieldSubject, WorkItemFieldURL,
				WorkItemFieldRequester, WorkItemFieldAssignees, WorkItemFieldFields, WorkItemFieldExternal},
			[]string{"created", "status", "subject", "url", "requester", "assignees", "fields", "external"},
		},
		{
			[]string{TaskFieldCreated, TaskFieldState, TaskFieldStatusMessage, TaskFieldArtifacts,
				TaskFieldCancelRequested, TaskFieldExternal},
			[]string{"created", "state", "status_message", "artifacts", "cancel_requested", "external"},
		},
	} {
		if strings.Join(tc.got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// Every name the registry, policy and event log key on is distinct across
// spaces: a type key, capability, action, event kind or schema name never
// shares a value with another.
func TestRegistryNamesUnique(t *testing.T) {
	seen := map[string]string{}
	add := func(space, v string) {
		if prev, ok := seen[v]; ok {
			t.Errorf("%q is both %s and %s", v, prev, space)
		}
		seen[v] = space
	}
	for _, k := range append(MaintainedTypeKeys(), ReservedTypeKeys()...) {
		add("type key", string(k))
	}
	for _, c := range allCapabilities {
		add("capability", string(c))
	}
	for _, a := range allActions {
		add("action", a)
	}
	for _, e := range allEvents {
		add("event", e)
	}
	for _, s := range allSchemas {
		add("schema", s)
	}
}

// Within each const block of names.go no two constants share a value, and
// every Type constant is listed in exactly one key list.
func TestNamesConstBlocksUnique(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "names.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[TypeKey]int{}
	for _, k := range append(MaintainedTypeKeys(), ReservedTypeKeys()...) {
		listed[k]++
	}
	typeConsts := 0
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		values := map[string]string{}
		for _, s := range g.Specs {
			vs := s.(*ast.ValueSpec)
			for i, name := range vs.Names {
				v := constValue(vs.Values[i])
				if prev, dup := values[v]; dup {
					t.Errorf("%s and %s share value %s", prev, name.Name, v)
				}
				values[v] = name.Name
				if strings.HasPrefix(name.Name, "Type") {
					typeConsts++
					k, err := strconv.Unquote(v)
					if err != nil || listed[TypeKey(k)] != 1 {
						t.Errorf("%s = %s is listed %d times", name.Name, v, listed[TypeKey(k)])
					}
				}
			}
		}
	}
	if typeConsts != len(listed) {
		t.Errorf("%d Type constants, %d listed keys", typeConsts, len(listed))
	}
}

func constValue(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Value
	case *ast.Ident:
		return v.Name
	}
	return ""
}

func isBuiltinArea(area string) bool {
	return contains(allAreas, area) || ResourceKind(area).Validate() == nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func requireKeys(t *testing.T, got []TypeKey, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d keys, want %d: %q", len(got), len(want), got)
	}
	for i := range got {
		if string(got[i]) != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// requireNameError asserts err is a validation *Error for field whose reason
// contains reason.
func requireNameError(t *testing.T, err error, field, reason string) {
	t.Helper()
	requireError(t, err, Validation, field)
	var e *Error
	if errors.As(err, &e) && (e.Retry != NoRetry || !strings.Contains(e.SafeMessage, reason)) {
		t.Fatalf("error = %q retry %q, want reason %q, retry none", e.SafeMessage, e.Retry, reason)
	}
}

// longName returns a valid three-label name of exactly n bytes.
func longName(n int) string {
	head := "com.example."
	rest := n - len(head)
	var labels []string
	for rest > 0 {
		l := min(rest, 63)
		if rest-l == 1 { // avoid a trailing empty label
			l--
		}
		labels = append(labels, strings.Repeat("a", l))
		rest -= l + 1
	}
	return head + strings.Join(labels, ".")
}
