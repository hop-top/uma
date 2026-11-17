package services

import "hop.top/uma/internal/operations"

// RegisterHeld registers the appliers and edit validators of the local
// writes this package holds for approval (conversation label changes,
// internal notes that stay in uma, the label and transition writes
// Config.Held opts in, and starts). uma.New calls it, the only public
// constructor of an engine, so every engine that can hold or decide a
// held operation has them; calling it again changes nothing. It is
// registered by call, not at load, so the core declares no init.
func RegisterHeld() {
	operations.RegisterApplier(intentConversationLabels, applyHeldLabels)
	operations.RegisterValidator(intentConversationLabels, validateHeldLabelsEdit)
	operations.RegisterApplier(intentLocalNote, applyHeldLocalNote)
	operations.RegisterValidator(intentLocalNote, validateHeldLocalNoteEdit)
	operations.RegisterOpeningApplier(operations.IntentStart, applyHeldStart)
	operations.RegisterValidator(operations.IntentStart, validateHeldStartEdit)
	operations.RegisterApplier(intentLabelConfigure, applyHeldLabelConfigure)
	operations.RegisterValidator(intentLabelConfigure, validateHeldLabelConfigureEdit)
	operations.RegisterApplier(intentLabelDelete, applyHeldLabelDelete)
	operations.RegisterValidator(intentLabelDelete, validateHeldLabelDeleteEdit)
	operations.RegisterApplier(intentConversationTransition, applyHeldTransition)
	operations.RegisterValidator(intentConversationTransition, validateHeldTransitionEdit)
}
