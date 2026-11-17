package model

import (
	"regexp"
	"strings"
)

// Reserved names and namespace grammar.
//
// Three name spaces exist:
//
//   - Core: unqualified (dot-free) tokens, two-segment "<area>.<verb>" names
//     and anything under the "uma." prefix. Only this package defines them.
//   - Maintained adapter contracts: two-segment type keys
//     "<source>.<resource>" whose contracts this module defines and whose
//     adapters only this module's repository implements. MaintainedTypeKeys
//     lists the implemented ones; ReservedTypeKeys lists the planned ones,
//     rejected at registration until implemented. Names owned by such a
//     contract are prefixed by its key, e.g. "github.issues.config".
//   - Third parties: three or more segments, by convention the reverse-DNS
//     name of a domain the author controls followed by "<source>.<resource>",
//     e.g. "com.example.acmechat.messages".
//
// Names never carry versions; Extension.Version, Event.Version and
// Descriptor.ContractMajor do.

// MaxNameLength bounds type keys and namespaced names (DNS subdomain limit).
const MaxNameLength = maxKeyBytes

// CoreNamespace prefixes builtin schema names. External definitions may not use it.
const CoreNamespace = "uma"

// NativeSource is the source segment of host-native adapter contracts.
const NativeSource = "native"

// Maintained adapter type keys implemented in this version.
//
// Each mail key is one channel per mailbox account, bound to one inbox:
// TypeImapMail retrieves over IMAP and sends by SMTP submission with the same
// credential; TypeSmtpMail receives as an MX (no AUTH; sender evidence from
// SPF, DKIM and DMARC, ARC recorded but not verified) and sends through a
// relay; TypeMicrosoftMail and TypeGoogleMail use the Microsoft Graph and
// Gmail APIs; TypeHttpMail takes mail an HTTP relay (an inbound-parse service
// or an MTA hook) posts, authenticated as a webhook. Mail keys always use the
// "mail" medium segment.
const (
	TypeNativeWebWidget TypeKey = "native.webwidget"
	TypeSmtpMail        TypeKey = "smtp.mail"
	TypeImapMail        TypeKey = "imap.mail"
	TypeMicrosoftMail   TypeKey = "microsoft.mail"
	TypeGoogleMail      TypeKey = "google.mail"
	TypeTwilioSms       TypeKey = "twilio.sms"
	TypeTwilioWhatsapp  TypeKey = "twilio.whatsapp"
	TypeMetaWhatsapp    TypeKey = "meta.whatsapp"
	TypeGithubIssues    TypeKey = "github.issues"
	TypeZendeskTickets  TypeKey = "zendesk.tickets"
	// TypeA2aMessages is the A2A server endpoint: remote agents send messages
	// and read their tasks over JSON-RPC; one channel per endpoint.
	TypeA2aMessages TypeKey = "a2a.messages"
	// TypeRelayMessages is the generic JSON message relay: an operator's own
	// SMS, WhatsApp or iMessage gateway posts messages in and takes replies at a
	// configured callback; one channel per relay and medium.
	TypeRelayMessages TypeKey = "relay.messages"
	// TypeHttpMail is the mail HTTP ingress: a relay POSTs each message to the
	// webhook the host mounts. It receives only.
	TypeHttpMail TypeKey = "http.mail"
	// TypeSlackChat is the Slack workspace: one channel per workspace install of
	// one app; rooms, threads and direct messages are its conversations.
	TypeSlackChat TypeKey = "slack.chat"
	// TypeTelegramMessages is the Telegram bot: one channel per bot; chats,
	// forum topics and private chats are its conversations.
	TypeTelegramMessages TypeKey = "telegram.messages"
	// TypeMicrosoftTeams is the Microsoft Teams bot, reached through the Bot
	// Framework: one channel per bot registration in one tenant; channels,
	// channel threads, group chats and personal chats are its conversations.
	TypeMicrosoftTeams TypeKey = "microsoft.teams"
	// TypeDiscordChat is the Discord bot: one channel per bot application;
	// guild channels, threads and direct messages are its conversations.
	TypeDiscordChat TypeKey = "discord.chat"
	// TypeGitlabIssues is GitLab issues: one channel per GitLab account (a
	// personal, project or group access token, or an OAuth application); issues
	// are its work items and their notes its messages.
	TypeGitlabIssues TypeKey = "gitlab.issues"
)

// Reserved maintained type keys: contracts planned but not implemented in
// this version. ValidateTypeKey rejects them until they move to
// MaintainedTypeKeys.
const (
	TypeMetaMessenger          TypeKey = "meta.messenger"
	TypeMetaInstagram          TypeKey = "meta.instagram"
	TypeLineMessages           TypeKey = "line.messages"
	TypeIrcChat                TypeKey = "irc.chat"
	TypeJiraIssues             TypeKey = "jira.issues"
	TypeLinearIssues           TypeKey = "linear.issues"
	TypeFreshdeskTickets       TypeKey = "freshdesk.tickets"
	TypeIntercomConversations  TypeKey = "intercom.conversations"
	TypeHelpscoutConversations TypeKey = "helpscout.conversations"
	TypeJiraTickets            TypeKey = "jira.tickets"
)

// MaintainedTypeKeys returns the two-segment keys whose contracts core defines
// and whose adapters ship in this version.
func MaintainedTypeKeys() []TypeKey {
	return []TypeKey{
		TypeNativeWebWidget, TypeSmtpMail, TypeImapMail, TypeMicrosoftMail,
		TypeGoogleMail, TypeTwilioSms, TypeTwilioWhatsapp, TypeMetaWhatsapp,
		TypeGithubIssues, TypeZendeskTickets, TypeA2aMessages, TypeRelayMessages,
		TypeHttpMail, TypeSlackChat, TypeTelegramMessages,
		TypeMicrosoftTeams, TypeDiscordChat,
		TypeGitlabIssues,
		TypeJiraIssues,
		TypeLinearIssues,
	}
}

// ReservedTypeKeys returns the two-segment keys reserved for planned
// maintained adapters that are not implemented in this version. Every other
// two-segment key outside MaintainedTypeKeys is reserved by shape.
func ReservedTypeKeys() []TypeKey {
	return []TypeKey{
		TypeMetaMessenger, TypeMetaInstagram, TypeLineMessages,
		TypeIrcChat,
		TypeFreshdeskTickets,
		TypeIntercomConversations, TypeHelpscoutConversations, TypeJiraTickets,
	}
}

// Builtin capabilities. Each maps to one optional adapter interface. Every
// other dot-free token is reserved for future core capabilities; adapter
// specific capabilities are namespaced names (see ValidateExtensionName).
const (
	CapabilityReceive          Capability = "receive"           // adapter.Receiver
	CapabilityReceiveHTTP      Capability = "receive_http"      // adapter.HTTPReceiver
	CapabilitySend             Capability = "send"              // adapter.Sender
	CapabilityEdit             Capability = "edit"              // adapter.Editor
	CapabilityDelete           Capability = "delete"            // adapter.Deleter
	CapabilityReact            Capability = "react"             // adapter.Reactor
	CapabilityReconcile        Capability = "reconcile"         // adapter.Reconciler
	CapabilitySignal           Capability = "signal"            // adapter.Signaler
	CapabilitySendEphemeral    Capability = "send_ephemeral"    // adapter.EphemeralSender
	CapabilityStream           Capability = "stream"            // adapter.Streamer
	CapabilityExtensionQuery   Capability = "extension_query"   // adapter.ExtensionReader
	CapabilityExtensionExecute Capability = "extension_execute" // adapter.ExtensionExecutor
	CapabilityProbePermissions Capability = "probe_permissions" // adapter.PermissionProber
	CapabilityInternalNote     Capability = "internal_note"     // adapter.NoteSender
	CapabilityRefetch          Capability = "refetch"           // adapter.Refetcher
	// CapabilityStartConversation: the adapter.Sender also reaches a
	// recipient no inbound message named, sending the first message of an
	// outbound-first conversation (Conversations.Start). Send alone answers
	// on routes ingress stored.
	CapabilityStartConversation Capability = "start_conversation" // adapter.Sender
	CapabilityWorkItemAct       Capability = "work_item_act"      // adapter.WorkItemActor
	// CapabilityRespond: the adapter answers interactions it reported
	// (Interactions.Respond), the single initial response included.
	CapabilityRespond Capability = "respond" // adapter.InteractionResponder
	// CapabilityRespondFollowUp: the adapter also takes follow-up
	// responses after the initial one. It requires CapabilityRespond.
	CapabilityRespondFollowUp Capability = "respond_follow_up" // adapter.InteractionResponder
	// CapabilityReceipt: the adapter tells the sender of a received
	// message that it was delivered or read (Messages.Receipt).
	CapabilityReceipt Capability = "receipt" // adapter.ReceiptSender
	// CapabilityMembership: the adapter adds participants to and removes
	// them from a provider conversation (Conversations.AddParticipant,
	// RemoveParticipant).
	CapabilityMembership Capability = "membership" // adapter.MembershipChanger
	// CapabilityHistory: the adapter reads a window of past messages from
	// the provider, for Runtime.Import.
	CapabilityHistory Capability = "history" // adapter.HistoryReader
	// CapabilityRawRead: the adapter reads the provider's whole copy of a
	// message, as the provider holds it, on demand (Messages.Raw). The
	// bytes are never stored.
	CapabilityRawRead Capability = "raw_read" // adapter.RawReader
	// The five provider conversation state capabilities, one per verb
	// pair; each status reports its scope (StateLimitScope).
	// CapabilityArchive archives and unarchives, CapabilityMove changes
	// folder or labels, CapabilityReadState marks read and unread,
	// CapabilityFlag stars or marks important, CapabilityTrash moves
	// to the provider's trash and back.
	CapabilityArchive   Capability = "archive"    // adapter.Archiver
	CapabilityMove      Capability = "move"       // adapter.Mover
	CapabilityReadState Capability = "read_state" // adapter.ReadStateSetter
	CapabilityFlag      Capability = "flag"       // adapter.Flagger
	CapabilityTrash     Capability = "trash"      // adapter.Trasher
	// CapabilityReplyRecipients: the adapter.Sender also addresses a reply
	// beyond its stored route: reply to all and explicit Cc and Bcc
	// (Reply.Recipients). The status's limits (RecipientLimits) say which
	// of them the channel supports.
	CapabilityReplyRecipients Capability = "reply_recipients" // adapter.Sender
)

// Builtin resource kinds (Ref.Kind). Each is also the first segment of the
// builtin event and action names for that resource.
const (
	KindChannel      = resourceChannel
	KindInbox        = resourceInbox
	KindContact      = resourceContact
	KindIdentity     = resourceIdentity
	KindConversation = resourceConversation
	KindMessage      = resourceMessage
	KindOperation    = resourceOperation
	KindTask         = resourceTask
	KindWorkItem     = resourceWorkItem
	KindAttachment   = resourceAttachment
	KindSubscription = resourceSubscription
	KindStream       = resourceStream
	KindExport       = resourceExport
	KindLabel        = resourceLabel
	// KindEvent is the tenant's event log as a whole (see EventLogRef), the
	// resource Events.List authorizes ActionEventList on. No event has it
	// as Event.Resource.
	KindEvent       = resourceEvent
	KindInteraction = resourceInteraction
)

// Non-resource areas: first segments of builtin action or event names that
// are not resource kinds.
const (
	AreaExtension = "extension"
	AreaPrivacy   = "privacy"
	AreaTrust     = "trust"
	AreaDelivery  = "delivery" // events only
)

// Builtin event kinds (Event.Kind).
//
// EventMessageReceived reports inbound provider messages only;
// EventMessageCreated reports messages authored through uma.
// EventTaskUpdated reports the opening and every change of an agent task.
// EventInteractionReceived reports an accepted interaction record and
// EventInteractionUpdated a change of its response state or a redaction.
// The interaction.* kinds are builtin; no extension registers them.
const (
	EventMessageReceived     = "message.received"
	EventMessageCreated      = "message.created"
	EventMessageUpdated      = "message.updated"
	EventConversationChanged = "conversation.changed"
	EventDeliveryUpdated     = "delivery.updated"
	EventWorkItemUpdated     = "work_item.updated"
	EventIdentityLinked      = "identity.linked"
	// EventContactCreated and EventContactUpdated report Contacts.Create
	// and Contacts.Update, for the internal audience. Their payloads carry
	// references and a change summary, never the name or attributes.
	// EventContactMerged reports Contacts.Merge: references only.
	EventContactCreated = "contact.created"
	EventContactUpdated = "contact.updated"
	EventContactMerged  = "contact.merged"
	// EventConversationLabelAdded and EventConversationLabelRemoved report
	// one label added to or removed from a conversation, one event per
	// label that changed. Label definitions emit no events.
	EventConversationLabelAdded   = "conversation.label_added"
	EventConversationLabelRemoved = "conversation.label_removed"
	EventTaskUpdated              = "task.updated"
	EventInteractionReceived      = "interaction.received"
	EventInteractionUpdated       = "interaction.updated"
	// EventConversationAssigned and EventConversationUnassigned report an
	// assignee set or replaced and an assignee cleared;
	// EventConversationSnoozed and EventConversationUnsnoozed report a
	// snooze set and one ended early. Internal audience, references only;
	// a snooze ending by expiry or inbound message emits nothing.
	EventConversationAssigned   = "conversation.assigned"
	EventConversationUnassigned = "conversation.unassigned"
	EventConversationSnoozed    = "conversation.snoozed"
	EventConversationUnsnoozed  = "conversation.unsnoozed"
	// EventConversationLinked and EventConversationUnlinked report a link
	// added or removed, once per conversation end. Read markers emit
	// nothing.
	EventConversationLinked   = "conversation.linked"
	EventConversationUnlinked = "conversation.unlinked"
	// EventOperationHeld reports a plugin actor's operation stored as held
	// for a human decision, once, when it is first held. EventOperationDecided
	// reports its approval, rejection or edit, once per decision, and
	// EventOperationExpired its deadline passing with no decision. Internal
	// audience, references only: never the preview or the edited arguments.
	// A subscriber needs operation.read and the read right of the
	// operation's target.
	EventOperationHeld    = "operation.held"
	EventOperationDecided = "operation.decided"
	EventOperationExpired = "operation.expired"
)

// EventEnvelopeVersion is the current Event.Version.
const EventEnvelopeVersion uint32 = 1

// Builtin payload schema names (Event.Payload.Name) are CoreNamespace + "." + kind.
const (
	SchemaMessageReceived     = "uma.message.received"
	SchemaMessageCreated      = "uma.message.created"
	SchemaMessageUpdated      = "uma.message.updated"
	SchemaConversationChanged = "uma.conversation.changed"
	SchemaDeliveryUpdated     = "uma.delivery.updated"
	SchemaWorkItemUpdated     = "uma.work_item.updated"
	SchemaIdentityLinked      = "uma.identity.linked"
	SchemaContactCreated      = "uma.contact.created"
	SchemaContactUpdated      = "uma.contact.updated"
	SchemaContactMerged       = "uma.contact.merged"

	SchemaConversationLabelAdded   = "uma.conversation.label_added"
	SchemaConversationLabelRemoved = "uma.conversation.label_removed"
	SchemaTaskUpdated              = "uma.task.updated"

	SchemaInteractionReceived = "uma.interaction.received"
	SchemaInteractionUpdated  = "uma.interaction.updated"

	SchemaConversationAssigned   = "uma.conversation.assigned"
	SchemaConversationUnassigned = "uma.conversation.unassigned"
	SchemaConversationSnoozed    = "uma.conversation.snoozed"
	SchemaConversationUnsnoozed  = "uma.conversation.unsnoozed"

	SchemaConversationLinked   = "uma.conversation.linked"
	SchemaConversationUnlinked = "uma.conversation.unlinked"

	SchemaOperationHeld    = "uma.operation.held"
	SchemaOperationDecided = "uma.operation.decided"
	SchemaOperationExpired = "uma.operation.expired"
)

// PayloadVersion1 is the Event.Payload.Version of every builtin payload above
// and the only payload major emitted in this version.
const PayloadVersion1 uint32 = 1

// Builtin policy actions (policy.Authorization.Action).
//
// ActionChannelReceive admits ingress on a channel. It is asked for the
// trusted service principal when a receiver, webhook or refetch binds to
// the channel (Resource the channel) and again for every accepted ingress
// and staged attachment (Resource the channel, Destination the inbox the
// destination resolves to), so revoking it stops ingress on the next
// accept, without rebinding.
const (
	ActionChannelConfigure       = "channel.configure"
	ActionChannelRead            = "channel.read"
	ActionChannelReceive         = "channel.receive"
	ActionChannelImport          = "channel.import"
	ActionInboxConfigure         = "inbox.configure"
	ActionInboxRead              = "inbox.read"
	ActionContactRead            = "contact.read"
	ActionIdentityRead           = "identity.read"
	ActionContactReadAll         = "contact.read_all"
	ActionContactWrite           = "contact.write"
	ActionIdentityLink           = "identity.link"
	ActionIdentityRelink         = "identity.relink"
	ActionIdentityLinkOverride   = "identity.link_override"
	ActionIdentityUnlink         = "identity.unlink"
	ActionContactMerge           = "contact.merge"
	ActionIdentityHistory        = "identity.history"
	ActionConversationRead       = "conversation.read"
	ActionConversationTransition = "conversation.transition"
	ActionConversationSignal     = "conversation.signal"
	ActionConversationStart      = "conversation.start"
	ActionConversationMembership = "conversation.membership"
	// ActionConversationStateAccount authorizes every provider state verb
	// whose reported scope is account; ActionConversationStateShared those
	// whose scope is shared.
	ActionConversationStateAccount = "conversation.state_account"
	ActionConversationStateShared  = "conversation.state_shared"
	ActionMessageRead              = "message.read"
	ActionMessageReply             = "message.reply"
	ActionMessageNote              = "message.note"
	ActionMessageEdit              = "message.edit"
	ActionMessageDelete            = "message.delete"
	ActionMessageReact             = "message.react"
	ActionMessageEphemeral         = "message.ephemeral"
	ActionMessageReceipt           = "message.receipt"
	ActionWorkItemRead             = "work_item.read"
	ActionWorkItemAct              = "work_item.act"
	ActionTaskRead                 = "task.read"
	ActionTaskAct                  = "task.act"
	ActionOperationRead            = "operation.read"
	ActionOperationReplay          = "operation.replay"
	ActionOperationReconcile       = "operation.reconcile"
	// ActionOperationApprove, ActionOperationReject and ActionOperationEdit
	// authorize the decision of a held operation (HeldBlock). Proposing a
	// held operation never grants them.
	ActionOperationApprove = "operation.approve"
	ActionOperationReject  = "operation.reject"
	ActionOperationEdit    = "operation.edit"
	// ActionTrustRead authorizes reading trust counters (TrustEntry) of a
	// channel; ActionTrustReset authorizes every write of them: reset,
	// recording a clean approval and recording a correction; ActionTrustWrite
	// authorizes importing counts (ImportTrust). The resource is the channel
	// the counters are scoped to.
	ActionTrustRead            = "trust.read"
	ActionTrustReset           = "trust.reset"
	ActionTrustWrite           = "trust.write"
	ActionSubscriptionOpen     = "subscription.open"
	ActionSubscriptionPoll     = "subscription.poll"
	ActionSubscriptionAck      = "subscription.ack"
	ActionSubscriptionClose    = "subscription.close"
	ActionAttachmentPrepare    = "attachment.prepare"
	ActionAttachmentUpload     = "attachment.upload"
	ActionAttachmentRead       = "attachment.read"
	ActionStreamRead           = "stream.read"
	ActionStreamWrite          = "stream.write"
	ActionExtensionQuery       = "extension.query"
	ActionExtensionExecute     = "extension.execute"
	ActionPrivacyRedact        = "privacy.redact"
	ActionPrivacyRedactCascade = "privacy.redact_cascade"
	ActionPrivacyExport        = "privacy.export"
	// ActionLabelRead reads a label definition; ActionLabelConfigure
	// creates, replaces and deletes one (resource: the label, or
	// CreationTarget(tenant, KindLabel) on creation). ActionConversationLabel
	// adds labels to and removes labels from a conversation (resource: the
	// conversation); it needs no ActionLabelRead on the labels.
	ActionLabelRead          = "label.read"
	ActionLabelConfigure     = "label.configure"
	ActionConversationLabel  = "conversation.label"
	ActionInteractionRead    = "interaction.read"
	ActionInteractionRespond = "interaction.respond"
	// ActionConversationAssign sets or clears a conversation's assignee
	// (resource: the conversation); the assignee must also hold it on the
	// conversation. ActionConversationSnooze sets or ends its snooze.
	// ActionConversationReadWorkflow reads a conversation's workflow state
	// (Conversation.Assignee and Snooze), filters on it and receives its
	// workflow events; desk-only, never held by a participant limited to
	// public content.
	ActionConversationAssign       = "conversation.assign"
	ActionConversationSnooze       = "conversation.snooze"
	ActionConversationReadWorkflow = "conversation.read_workflow"
	// ActionConversationLink adds and removes a conversation's links; it is
	// asked on both conversation ends, and the far end of a link to a work
	// item needs ActionWorkItemRead. ActionConversationMarkRead moves the
	// caller's own read marker on a conversation.
	ActionConversationLink     = "conversation.link"
	ActionConversationMarkRead = "conversation.mark_read"
)

// ActionEventList admits one Events.List call, on the tenant's event log
// resource (EventLogRef). It grants no event: each listed event is still
// authorized on its own, as for subscriptions.
const ActionEventList = "event.list"

// Extension definition kinds (adapter.ExtensionDefinition.Kind).
const (
	ExtensionKindRead   = "read"
	ExtensionKindEffect = "effect"
	ExtensionKindEvent  = "event"
)

// Content part kinds (Part.Kind).
const (
	PartText       = textPartKind
	PartAttachment = attachmentPartKind
	PartStructured = structuredPartKind
)

// Subscription start modes (OpenSubscription.Start). There is no default.
const (
	StartBeginning  = "beginning"
	StartTail       = "tail"
	StartCheckpoint = "checkpoint"
)

// Payload trigger values: who caused the change an event reports.
const (
	TriggerProvider = "provider" // observed from the provider (ingress, sync, reconcile)
	TriggerCommand  = "command"  // a uma mutation by Event.Actor
	TriggerSystem   = "system"   // engine policy, lifecycle or retention
)

// message.created origin values.
const (
	MessageOriginReply = "reply" // Messages.Reply
	MessageOriginNote  = "note"  // Messages.AddInternalNote
	MessageOriginStart = "start" // Conversations.Start
	// MessageOriginTaskStatus: the status message a Tasks.Act update
	// recorded (Task.StatusMessage).
	MessageOriginTaskStatus = "task_status"
	// MessageOriginResponse: Interactions.Respond in mode message.
	MessageOriginResponse = "response"
)

// interaction.updated change values.
const (
	InteractionChangeResponded = "responded" // the initial response was claimed
	InteractionChangeFollowUp  = "follow_up" // a follow-up response was accepted
	InteractionChangeRedacted  = "redacted"
)

// message.updated change values.
const (
	MessageChangeEdited          = "edited"
	MessageChangeDeleted         = "deleted"
	MessageChangeRedacted        = "redacted"
	MessageChangeReactionAdded   = "reaction_added"
	MessageChangeReactionRemoved = "reaction_removed"
	// MessageChangeAudienceNarrowed: the provider narrowed the message's
	// audience, such as a comment made private, without newer content.
	MessageChangeAudienceNarrowed = "audience_narrowed"
	// MessageChangeEcho: an inbound copy was linked to the uma-sent
	// message it echoes (Message.EchoOf).
	MessageChangeEcho = "echo"
	// MessageChangeEditSuperseded: an outbound edit the provider answered
	// was not applied locally because the message's edit state advanced
	// since the command was accepted. Nothing is written; the provider
	// outcome stays on the operation. It does not prove the provider
	// edit never happened.
	MessageChangeEditSuperseded = "edit_superseded"
)

// conversation.changed field names.
const (
	ConversationFieldCreated      = "created"
	ConversationFieldStatus       = "status"
	ConversationFieldMode         = "mode"
	ConversationFieldParticipants = "participants"
	ConversationFieldExternal     = "external"
	ConversationFieldReplyRoute   = "reply_route"
	// ConversationFieldProviderState: Conversation.ProviderState changed.
	ConversationFieldProviderState = "provider_state"
)

// work_item.updated field names.
const (
	WorkItemFieldCreated   = "created"
	WorkItemFieldStatus    = "status"
	WorkItemFieldSubject   = "subject"
	WorkItemFieldURL       = "url"
	WorkItemFieldRequester = "requester"
	WorkItemFieldAssignees = "assignees"
	WorkItemFieldFields    = "fields"
	WorkItemFieldExternal  = "external"
)

// task.updated field names.
const (
	TaskFieldCreated         = "created"
	TaskFieldState           = "state"
	TaskFieldStatusMessage   = "status_message"
	TaskFieldArtifacts       = "artifacts"
	TaskFieldCancelRequested = "cancel_requested"
	TaskFieldExternal        = "external"
)

var (
	maintainedKeyRE = regexp.MustCompile(`^[a-z][a-z0-9]{0,62}\.[a-z][a-z0-9]{0,62}$`)
	externalKeyRE   = regexp.MustCompile(
		`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?){2,}$`)
	namespacedRE = regexp.MustCompile(
		`^[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?){2,}$`)
	builtinPairRE  = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
	builtinTokenRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	versionSegRE   = regexp.MustCompile(`^v[0-9]+$`)
)

// IsMaintainedTypeKey reports whether k is a maintained adapter contract key
// implemented in this version.
func IsMaintainedTypeKey(k TypeKey) bool {
	return containsKey(MaintainedTypeKeys(), k)
}

// IsReservedTypeKey reports whether k is reserved for a planned maintained
// adapter not implemented in this version.
func IsReservedTypeKey(k TypeKey) bool {
	return containsKey(ReservedTypeKeys(), k)
}

func containsKey(keys []TypeKey, k TypeKey) bool {
	for _, m := range keys {
		if k == m {
			return true
		}
	}
	return false
}

// ValidateTypeKey checks adapter type-key grammar and reservations.
// Maintained keys are valid; reserved keys are rejected. Other keys need three
// or more lowercase DNS-label segments, must not start with a reserved segment
// or a maintained or reserved key, and must not end in a version segment.
func ValidateTypeKey(k TypeKey) error {
	s := string(k)
	switch {
	case IsMaintainedTypeKey(k):
		return nil
	case IsReservedTypeKey(k):
		return typeKeyError("reserved; not implemented in this version")
	case len(s) > MaxNameLength:
		return typeKeyError("exceeds maximum length")
	case maintainedKeyRE.MatchString(s):
		return typeKeyError("two-segment keys are reserved for maintained adapters")
	case !externalKeyRE.MatchString(s):
		return typeKeyError("must be three or more lowercase dot-separated DNS labels")
	}
	segs := strings.Split(s, ".")
	if segs[0] == CoreNamespace || segs[0] == NativeSource {
		return typeKeyError("starts with a reserved segment")
	}
	if p := TypeKey(segs[0] + "." + segs[1]); IsMaintainedTypeKey(p) || IsReservedTypeKey(p) {
		return typeKeyError("starts with a maintained or reserved type key")
	}
	if versionSegRE.MatchString(segs[len(segs)-1]) {
		return typeKeyError("must not carry a version")
	}
	return nil
}

// ValidateExtensionName checks a name declared outside core: an extension
// definition name, schema name, extension capability, policy action or event
// kind. It needs three or more segments, must not use CoreNamespace and must
// not end in a version segment. Shorter names are reserved for core.
func ValidateExtensionName(name string) error {
	switch {
	case len(name) > MaxNameLength:
		return nameError("exceeds maximum length")
	case builtinTokenRE.MatchString(name) || builtinPairRE.MatchString(name):
		return nameError("unqualified and two-segment names are reserved for core")
	case !namespacedRE.MatchString(name):
		return nameError("must be three or more lowercase dot-separated segments")
	case strings.HasPrefix(name, CoreNamespace+"."):
		return nameError("the uma namespace is reserved for core")
	case versionSegRE.MatchString(name[strings.LastIndexByte(name, '.')+1:]):
		return nameError("must not carry a version")
	}
	return nil
}

func typeKeyError(reason string) error { return invalidField("type", reason) }

func nameError(reason string) error { return invalidField("name", reason) }
