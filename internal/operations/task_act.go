package operations

import "hop.top/uma/model"

// TaskActTarget is the operation key target of a Tasks.Act request: the
// conversation for model.SchemaTaskOpen, whose task does not exist yet, so
// a retried opening replays instead of allocating again; the task for
// every other action.
func TaskActTarget(req model.TaskAction) model.Ref {
	if req.Action.Name == model.SchemaTaskOpen {
		return req.Target.Conversation.Ref()
	}
	return req.Target.Ref()
}

// TaskActFingerprint is the version 1 fingerprint of a Tasks.Act request:
// kind, target (TaskActTarget), action and expected revision. The action
// covers the whole opening or update, status message, audience and
// artifacts included; correlation is excluded.
func TaskActFingerprint(req model.TaskAction) []byte {
	f := NewFingerprint()
	f.Set("kind", Field().Str(model.ActionTaskAct))
	f.Set("target", Field().Ref(TaskActTarget(req)))
	f.Set("action", Field().Extension(req.Action))
	f.Set("expected_revision", Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}
