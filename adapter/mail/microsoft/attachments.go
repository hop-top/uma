package microsoft

import (
	"bytes"
	"context"
	"net/url"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/staging"
	"hop.top/uma/model"
)

// Bounds on the attachments one message stages.
const (
	// maxStagedAttachments is the number of attachments downloaded and
	// staged per message; the ones after it are described only.
	maxStagedAttachments = staging.MaxAttachments
	// maxStagedBytes is the size of the largest attachment staged; larger
	// ones are described only and never downloaded.
	maxStagedBytes = staging.MaxAttachmentBytes
)

// fileAttachmentType is the @odata.type of an attachment with raw bytes.
// An item attachment (an Outlook item) has a MIME form that is not a file,
// and a reference attachment (a link) has no raw form: Graph answers its
// $value with 405.
const fileAttachmentType = "#microsoft.graph.fileAttachment"

// staged is the outcome of staging a message's attachments: the attachment
// parts, and per attachment whether its bytes were staged.
type staged struct {
	parts []model.Part
	ok    []bool
}

func (s staged) has(n int) bool { return n < len(s.ok) && s.ok[n] }

// stageAttachments downloads the raw bytes of the message's file
// attachments one at a time and stages each through sink, up to
// maxStagedAttachments attachments of maxStagedBytes each, among the
// attachments that are described. The bytes are the attachment's $value
// (https://learn.microsoft.com/en-us/graph/api/attachment-get, "Get the raw
// contents of a file or item attachment"), which does not base64 them as
// contentBytes does.
//
// A sink that cannot stage downloads nothing. An attachment that cannot be
// fetched for good (gone, no raw form, over the bound, or refused by the
// engine's checks) stays described only. Without a blob store
// (model.Disabled) the first attempt ends staging. An error is returned only
// when repeating the fetch may fix it: Graph or the sink was unavailable, or
// ctx ended.
func (i *instance) stageAttachments(ctx context.Context, s *session, sink adapter.Sink, msgID, dest string, atts []gAttachment) (staged, error) {
	st := staged{ok: make([]bool, len(atts))}
	stager, ok := sink.(adapter.AttachmentSink)
	if !ok {
		return st, nil
	}
	attempts := 0
	for n, a := range atts[:min(len(atts), maxAttachmentParts)] {
		if attempts == maxStagedAttachments {
			break
		}
		// A response without @odata.type is tried: a reference
		// attachment's 405 is final below.
		if (a.ODataType != "" && a.ODataType != fileAttachmentType) || a.Size > maxStagedBytes {
			continue
		}
		attempts++
		body, err := i.attachmentBytes(ctx, s, msgID, a.ID)
		if err != nil {
			if staging.Retryable(err) {
				return st, err
			}
			continue
		}
		if len(body) == 0 {
			continue
		}
		md := model.AttachmentMetadata{
			Name: mime.SanitizeFilename(a.Name), MediaType: a.ContentType, Size: int64(len(body)), Audience: model.Audience{Visibility: model.Public},
		}
		if md.Name == "" {
			md.Name = "attachment"
		}
		if md.MediaType == "" {
			md.MediaType = "application/octet-stream"
		}
		ref, outcome, err := staging.Stage(ctx, stager, adapter.StageRequest{Key: stagingKey(dest, msgID, a.ID), Destination: dest, Metadata: md}, bytes.NewReader(body))
		switch outcome {
		case staging.Staged:
			st.ok[n] = true
			st.parts = append(st.parts, model.Part{Kind: model.PartAttachment, Attachment: &ref})
		case staging.Retry:
			return st, err
		case staging.Disabled:
			return st, nil
		}
	}
	return st, nil
}

// attachmentBytes downloads the raw content of one attachment, read whole
// up to maxStagedBytes.
func (i *instance) attachmentBytes(ctx context.Context, s *session, msgID, attID string) ([]byte, error) {
	resp, err := s.do(ctx, request{
		method: "GET",
		url:    i.c.api(i.cfg.mailboxPath()+"/messages/"+url.PathEscape(msgID)+"/attachments/"+url.PathEscape(attID)+"/$value", nil),
		prefer: []string{preferImmutableID}, accept: "*/*", limit: maxStagedBytes,
	})
	if err != nil {
		return nil, transient(err)
	}
	return resp.body, nil
}

// stagingKey scopes the staging of one attachment to its destination and
// message: bounded and opaque whatever Graph's identifiers look like.
func stagingKey(dest, msgID, attID string) string {
	return staging.OpaqueKey("mail:", dest, msgID, attID)
}
