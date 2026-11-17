package model

// MediaTypeRawMessage is RawMessage.MediaType of a mail message: the whole
// RFC 5322 message.
const MediaTypeRawMessage = "message/rfc822"

// RawMessage is the provider's whole copy of a stored message, read on
// demand through Messages.Raw: for mail the RFC 5322 bytes with every
// header and part, as the provider serves them. It is never stored, so the
// caller owns what it holds; Size is len(Bytes).
type RawMessage struct {
	Ref       MessageRef `json:"ref"`
	MediaType string     `json:"media_type"`
	Size      int64      `json:"size"`
	Bytes     []byte     `json:"bytes,omitempty"`
}
