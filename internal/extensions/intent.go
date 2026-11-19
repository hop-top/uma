package extensions

import (
	"encoding/json"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
)

// intentName names the stored intent of an extension effect, version
// intentVersion1.
const (
	intentName     = "uma.intent.extension"
	intentVersion1 = 1
)

// intent is the durable intent of an extension effect: everything a worker
// needs to execute it after a restart. The definition is the one Action
// names; the channel and route are resolved again from Target at
// execution.
type intent struct {
	Target   model.Ref       `json:"target"`
	Audience model.Audience  `json:"audience"`
	Action   model.Extension `json:"action"`
	// Message is the conversation message the effect acts on, when named.
	Message *model.MessageRef `json:"message,omitempty"`
}

// encode returns the stored form of in.
func (in intent) encode() (model.Extension, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return model.Extension{}, model.NewError(model.Validation, "extension action cannot be stored")
	}
	return model.Extension{Name: intentName, Version: intentVersion1, Data: data}, nil
}

// decodeIntent reads a stored extension intent.
func decodeIntent(e model.Extension) (intent, error) {
	var in intent
	if e.Name != intentName || e.Version != intentVersion1 {
		return in, mismatch("unknown operation intent")
	}
	if err := json.Unmarshal(e.Data, &in); err != nil {
		return in, &model.Error{Code: model.Permanent, SafeMessage: "stored operation intent unreadable", Retry: model.NoRetry}
	}
	return in, nil
}

// fingerprint returns the version 1 fingerprint of an extension effect:
// policy action, target, audience, action and expected revision.
// Correlation is excluded.
func fingerprint(kind string, req model.ExecuteExtension) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(kind))
	f.Set("target", operations.Field().Ref(req.Target))
	f.Set("audience", operations.Field().Audience(req.Audience))
	f.Set("action", operations.Field().Extension(req.Action))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	if req.Message != nil {
		f.Set("message", operations.Field().Str(string(req.Message.ID)))
	}
	return f.Sum()
}
