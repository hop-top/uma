package engine

import (
	"maps"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Builtin core names the registry checks declarations against. They are
// functions, not package variables, so no engine shares mutable state.

// builtinCapabilities returns the core capabilities, each backed by exactly
// one optional adapter interface (see capabilityImplemented).
func builtinCapabilities() []model.Capability {
	return []model.Capability{
		model.CapabilityReceive, model.CapabilityReceiveHTTP, model.CapabilitySend,
		model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact,
		model.CapabilityReconcile, model.CapabilitySignal, model.CapabilitySendEphemeral,
		model.CapabilityStream, model.CapabilityExtensionQuery,
		model.CapabilityExtensionExecute, model.CapabilityProbePermissions,
		model.CapabilityInternalNote, model.CapabilityRefetch,
		model.CapabilityStartConversation,
		model.CapabilityWorkItemAct,
		model.CapabilityRespond, model.CapabilityRespondFollowUp,
		model.CapabilityReceipt, model.CapabilityMembership, model.CapabilityHistory,
		model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState,
		model.CapabilityFlag, model.CapabilityTrash,
		model.CapabilityReplyRecipients, model.CapabilityRawRead,
	}
}

// capabilityRequires returns the builtin capability c requires in the same
// descriptor, if any: follow-up responses extend the initial response.
func capabilityRequires(c model.Capability) (model.Capability, bool) {
	if c == model.CapabilityRespondFollowUp {
		return model.CapabilityRespond, true
	}
	return "", false
}

// capabilityImplemented reports whether inst implements the adapter
// interface behind builtin capability c. ok is false for any other name.
func capabilityImplemented(c model.Capability, inst adapter.Instance) (implemented, ok bool) {
	switch c {
	case model.CapabilityReceive:
		return is[adapter.Receiver](inst), true
	case model.CapabilityReceiveHTTP:
		return is[adapter.HTTPReceiver](inst), true
	case model.CapabilitySend:
		return is[adapter.Sender](inst), true
	case model.CapabilityEdit:
		return is[adapter.Editor](inst), true
	case model.CapabilityDelete:
		return is[adapter.Deleter](inst), true
	case model.CapabilityReact:
		return is[adapter.Reactor](inst), true
	case model.CapabilityReconcile:
		return is[adapter.Reconciler](inst), true
	case model.CapabilitySignal:
		return is[adapter.Signaler](inst), true
	case model.CapabilitySendEphemeral:
		return is[adapter.EphemeralSender](inst), true
	case model.CapabilityStream:
		return is[adapter.Streamer](inst), true
	case model.CapabilityExtensionQuery:
		return is[adapter.ExtensionReader](inst), true
	case model.CapabilityExtensionExecute:
		return is[adapter.ExtensionExecutor](inst), true
	case model.CapabilityProbePermissions:
		return is[adapter.PermissionProber](inst), true
	case model.CapabilityInternalNote:
		return is[adapter.NoteSender](inst), true
	case model.CapabilityRefetch:
		return is[adapter.Refetcher](inst), true
	case model.CapabilityStartConversation, model.CapabilityReplyRecipients:
		return is[adapter.Sender](inst), true
	case model.CapabilityWorkItemAct:
		return is[adapter.WorkItemActor](inst), true
	case model.CapabilityRespond, model.CapabilityRespondFollowUp:
		return is[adapter.InteractionResponder](inst), true
	case model.CapabilityReceipt:
		return is[adapter.ReceiptSender](inst), true
	case model.CapabilityMembership:
		return is[adapter.MembershipChanger](inst), true
	case model.CapabilityHistory:
		return is[adapter.HistoryReader](inst), true
	case model.CapabilityRawRead:
		return is[adapter.RawReader](inst), true
	case model.CapabilityArchive:
		return is[adapter.Archiver](inst), true
	case model.CapabilityMove:
		return is[adapter.Mover](inst), true
	case model.CapabilityReadState:
		return is[adapter.ReadStateSetter](inst), true
	case model.CapabilityFlag:
		return is[adapter.Flagger](inst), true
	case model.CapabilityTrash:
		return is[adapter.Trasher](inst), true
	}
	return false, false
}

func is[T any](v any) bool {
	_, ok := v.(T)
	return ok
}

// builtinEvents returns the core event kinds.
func builtinEvents() []string {
	return []string{
		model.EventMessageReceived, model.EventMessageCreated, model.EventMessageUpdated,
		model.EventConversationChanged, model.EventDeliveryUpdated,
		model.EventWorkItemUpdated, model.EventIdentityLinked, model.EventContactMerged,
		model.EventContactCreated, model.EventContactUpdated,
		model.EventConversationLabelAdded, model.EventConversationLabelRemoved,
		model.EventTaskUpdated,
		model.EventInteractionReceived, model.EventInteractionUpdated,
		model.EventConversationAssigned, model.EventConversationUnassigned,
		model.EventConversationSnoozed, model.EventConversationUnsnoozed,
		model.EventConversationLinked, model.EventConversationUnlinked,
		model.EventConversationReadMarked, model.EventConversationUnreadMarked,
		model.EventOperationHeld, model.EventOperationDecided, model.EventOperationExpired,
	}
}

// actionClass says what a builtin policy action grants, which decides
// whether an extension definition may authorize as it.
type actionClass uint8

const (
	// classRead grants reading only. An event kind may authorize as it
	// (delivery is a read); an effect may not.
	classRead actionClass = iota + 1
	// classEffect writes through a provider-side resource. An effect may
	// authorize as it, as the ticket adapter's internal note and work item
	// update do; an event kind may not.
	classEffect
	// classCore is bound to a core flow (configuration, ingress, identity
	// and contact data, operation recovery, subscriptions, attachment
	// staging, streams, privacy, labels). Neither an effect nor an event
	// kind may borrow it.
	classCore
)

// builtinActionClasses classifies every core policy action. The table is
// the one place a builtin action is declared: builtinActions derives from
// it and the registry rules read it, never the action's name, so a new
// action must be classified to exist (see TestBuiltinSetsMatchModel and
// TestEveryModelActionClassified).
func builtinActionClasses() map[string]actionClass {
	m := map[string]actionClass{}
	for class, actions := range map[actionClass][]string{
		classRead: {
			model.ActionChannelRead, model.ActionInboxRead,
			model.ActionContactRead, model.ActionIdentityRead, model.ActionContactReadAll,
			model.ActionIdentityHistory,
			model.ActionConversationRead, model.ActionMessageRead,
			model.ActionWorkItemRead, model.ActionTaskRead,
			model.ActionOperationRead, model.ActionEventList,
			model.ActionAttachmentRead, model.ActionStreamRead,
			model.ActionExtensionQuery, model.ActionLabelRead, model.ActionInteractionRead,
			model.ActionConversationReadWorkflow,
		},
		classEffect: {
			model.ActionConversationTransition, model.ActionConversationSignal,
			model.ActionConversationStart, model.ActionConversationLabel, model.ActionConversationMembership,
			model.ActionConversationStateAccount, model.ActionConversationStateShared,
			model.ActionMessageReply, model.ActionMessageNote, model.ActionMessageEdit,
			model.ActionMessageDelete, model.ActionMessageReact, model.ActionMessageEphemeral,
			model.ActionMessageReceipt,
			model.ActionWorkItemAct, model.ActionTaskAct,
			model.ActionInteractionRespond, model.ActionExtensionExecute,
		},
		classCore: {
			model.ActionChannelConfigure, model.ActionChannelReceive, model.ActionChannelImport, model.ActionInboxConfigure,
			model.ActionContactWrite, model.ActionContactMerge,
			model.ActionIdentityLink, model.ActionIdentityRelink, model.ActionIdentityLinkOverride,
			model.ActionIdentityUnlink,
			model.ActionOperationReplay, model.ActionOperationReconcile,
			model.ActionOperationApprove, model.ActionOperationReject, model.ActionOperationEdit,
			model.ActionTrustRead, model.ActionTrustReset, model.ActionTrustWrite,
			model.ActionSubscriptionOpen, model.ActionSubscriptionPoll,
			model.ActionSubscriptionAck, model.ActionSubscriptionClose,
			model.ActionAttachmentPrepare, model.ActionAttachmentUpload, model.ActionStreamWrite,
			model.ActionPrivacyRedact, model.ActionPrivacyRedactCascade, model.ActionPrivacyExport,
			model.ActionLabelConfigure,
			model.ActionConversationAssign, model.ActionConversationSnooze,
			model.ActionConversationLink, model.ActionConversationMarkRead,
		},
	} {
		for _, a := range actions {
			m[a] = class
		}
	}
	return m
}

// builtinActions returns the core policy actions, sorted.
func builtinActions() []string {
	return slices.Sorted(maps.Keys(builtinActionClasses()))
}

// notExecuted holds the builtin capabilities an adapter may declare but
// no core operation of this version executes. Every builtin capability is
// executed: edit by edit-mode streams and Messages.Edit, delete and react
// by Messages.Delete and Messages.React, work_item_act by WorkItems.Act,
// respond and respond_follow_up by Interactions.Respond and
// Interactions.Ephemeral.
var notExecuted = map[model.Capability]bool{}

// Executes reports whether a core operation of this version executes
// capability c: false for a builtin capability in notExecuted, which
// discovery then reports unavailable however the adapter reports it, and
// true for every other builtin and for a namespaced capability, which the
// Extensions service executes.
func Executes(c model.Capability) bool { return !notExecuted[c] }
