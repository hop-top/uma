package google

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// ReadRaw reads Gmail message req.Message in raw form
// (users.messages.get, format=raw): the whole RFC 5322 message with its
// headers and every part, as Gmail holds it (adapter.RawReader). It is the
// read the receiver and the unsubscribe effect make, under the same size
// bound: a message whose raw answer is over it is model.Permanent with
// model.TooLarge, never a truncated copy. Nothing is kept. It needs
// ScopeReadonly; a missing scope is model.Unauthorized from the call.
func (i *instance) ReadRaw(ctx context.Context, req adapter.RawRequest) (adapter.RawMessage, error) {
	ref := req.Message
	if ref.Channel != i.ch.Ref || ref.Namespace != model.ProviderMessageNamespace || ref.Kind != model.RefKindMessage || ref.ID == "" {
		return adapter.RawMessage{}, model.NewError(model.Validation, "message: not a provider message of this channel")
	}
	raw, err := i.rawMessage(ctx, i.c.session(), ref.ID)
	switch {
	case tooLarge(err):
		return adapter.RawMessage{}, model.NewReasonError(model.Permanent, model.TooLarge, "message is over the size limit of a raw read")
	case err != nil:
		return adapter.RawMessage{}, transient(err)
	}
	return adapter.RawMessage{MediaType: model.MediaTypeRawMessage, Bytes: raw}, nil
}
