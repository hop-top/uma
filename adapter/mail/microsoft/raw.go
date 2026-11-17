package microsoft

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// maxRawMessageBytes bounds a raw message read: the largest single blob the
// adapter reads from Graph (the bound of one attachment's bytes).
const maxRawMessageBytes = maxStagedBytes

// ReadRaw reads message req.Message in MIME form (GET
// .../messages/{id}/$value): the RFC 5322 message with its headers and every
// part, as Exchange serves the stored message (adapter.RawReader). Exchange
// may have re-encoded the message since it arrived, so the bytes are
// Graph's copy, not necessarily the sender's. A message over
// maxRawMessageBytes is model.Permanent with model.TooLarge, never a
// truncated copy. Nothing is kept. Mail.ReadWrite covers the read.
func (i *instance) ReadRaw(ctx context.Context, req adapter.RawRequest) (adapter.RawMessage, error) {
	ref := req.Message
	if ref.Channel != i.ch.Ref || ref.Namespace != model.ProviderMessageNamespace || ref.Kind != model.RefKindMessage || ref.ID == "" {
		return adapter.RawMessage{}, model.NewError(model.Validation, "message: not a provider message of this channel")
	}
	resp, err := i.c.session().do(ctx, request{
		method: "GET", url: i.messageURL(ref.ID, "/$value"), accept: "message/rfc822, text/plain, */*",
		prefer: []string{preferImmutableID}, limit: maxRawMessageBytes,
	})
	if err != nil {
		return adapter.RawMessage{}, transient(err)
	}
	return adapter.RawMessage{MediaType: model.MediaTypeRawMessage, Bytes: resp.body}, nil
}
