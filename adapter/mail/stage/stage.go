// Package stage hands the attachments of a parsed inbound mail to the
// engine's attachment sink, for the mail adapters that read raw RFC 5322
// messages (SMTP, IMAP and Gmail).
//
// The bytes come from mime.Parse with the limits of Keep: at most
// MaxAttachments attachments of at most MaxAttachmentBytes each are kept in
// memory. Stage streams each kept attachment into the sink and reports
// which were staged by setting Attachment.Staged, so a consumer reads from
// the message's own attachment descriptors which attachments have bytes
// behind an attachment part and which are described only.
package stage

import (
	"bytes"
	"context"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/staging"
	"hop.top/uma/model"
)

// Bounds on the attachments one message stages.
const (
	// MaxAttachments is the number of attachments staged per message; the
	// ones after it are described only.
	MaxAttachments = staging.MaxAttachments
	// MaxAttachmentBytes is the decoded size of the largest attachment
	// staged; larger ones are described only. The engine's own attachment
	// size limit and media type checks apply on top.
	MaxAttachmentBytes = staging.MaxAttachmentBytes
)

// Keep returns l with the bounds on the attachment bytes mime.Parse keeps
// for staging.
func Keep(l mime.Limits) mime.Limits {
	l.KeepBytes, l.KeepParts = MaxAttachmentBytes, MaxAttachments
	return l
}

// Request says where the attachments of one message go.
type Request struct {
	// Key identifies the message at the provider, stable across
	// redeliveries and unique per destination: staging is idempotent per
	// channel and key, so a redelivery restages the same attachments.
	Key string
	// Destination is the destination of the ingress the attachments belong
	// to; they are reserved in its inbox.
	Destination string
	Audience    model.Audience
}

// Stage stages the kept bytes of m's attachments through sink and returns
// an attachment part for each, in attachment order. It sets Staged on the
// attachments it staged; the others stay described only.
//
// A sink that cannot stage, or one without a blob store (model.Disabled),
// stages nothing. An attachment the engine refuses for good (its checks,
// or other bytes under the same key) is skipped. Stage returns an error
// only when a redelivery may fix it: the sink was unavailable, or ctx
// ended. Attachments staged before it stay staged, and a redelivery
// restages them under the same keys.
func Stage(ctx context.Context, sink adapter.Sink, r Request, m *mime.Message) ([]model.Part, error) {
	stager, ok := sink.(adapter.AttachmentSink)
	if !ok {
		return nil, nil
	}
	var parts []model.Part
	for n := range m.Attachments {
		if n >= len(m.Data) || len(m.Data[n]) == 0 {
			continue
		}
		a := &m.Attachments[n]
		ref, outcome, err := staging.Stage(ctx, stager, adapter.StageRequest{
			Key: key(r.Key, n), Destination: r.Destination, Metadata: metadata(*a, n, r.Audience),
		}, bytes.NewReader(m.Data[n]))
		switch outcome {
		case staging.Staged:
			a.Staged = true
			parts = append(parts, model.Part{Kind: model.PartAttachment, Attachment: &ref})
		case staging.Retry:
			return parts, err
		case staging.Disabled:
			return parts, nil
		}
	}
	return parts, nil
}

// key is the staging key of the nth attachment of a message: bounded and
// opaque, whatever the provider's message identifier looks like.
func key(message string, n int) string {
	return staging.OpaqueKey("mail:", message, strconv.Itoa(n))
}

func metadata(a mime.Attachment, n int, aud model.Audience) model.AttachmentMetadata {
	md := model.AttachmentMetadata{Name: a.Filename, MediaType: a.MediaType, Size: a.Size, Digest: a.Digest, Audience: aud}
	if md.Name == "" {
		md.Name = "attachment-" + strconv.Itoa(n+1)
	}
	if md.MediaType == "" {
		md.MediaType = "application/octet-stream"
	}
	return md
}
