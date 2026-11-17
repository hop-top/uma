package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta"
	"hop.top/uma/model"
)

// mediaObject is the Graph answer for a media ID: where to download it.
type mediaObject struct {
	URL      string `json:"url"`
	MimeType string `json:"mime_type"`
	SHA256   string `json:"sha256"`
	FileSize int64  `json:"file_size"`
	ID       string `json:"id"`
}

// mediaOf returns the media object of an inbound message and its type.
func mediaOf(m message) (*media, string) {
	for typ, md := range map[string]*media{"image": m.Image, "audio": m.Audio, "video": m.Video, "document": m.Document, "sticker": m.Sticker} {
		if m.Type == typ && md != nil && md.ID != "" {
			return md, typ
		}
	}
	return nil, ""
}

// attachMedia stages the media of an inbound message through the sink and
// adds the attachment to its content, next to the MediaSchema part. It
// reports a failure worth a redelivery; media that cannot be staged for
// good (no staging sink, no blob store, refused by the engine's checks,
// expired at Meta, a foreign download URL) leaves the message with the
// MediaSchema part only.
func (x *Instance) attachMedia(ctx context.Context, sink adapter.Sink, m message, in *model.Ingress) error {
	md, typ := mediaOf(m)
	stager, ok := sink.(adapter.AttachmentSink)
	if md == nil || !ok {
		return nil
	}
	ref, err := x.stageMedia(ctx, stager, md, typ, in.Envelope)
	switch {
	case err == nil:
		in.Envelope.Content.Parts = append(in.Envelope.Content.Parts, model.Part{Kind: model.PartAttachment, Attachment: &ref})
		return nil
	case retryable(err):
		return err
	}
	return nil
}

// stageMedia looks the media ID up, downloads it with the access token and
// stages the bytes under the media ID.
func (x *Instance) stageMedia(ctx context.Context, sink adapter.AttachmentSink, md *media, typ string, env model.Envelope) (model.AttachmentRef, error) {
	token, err := x.secret(ctx, PurposeGraph)
	if err != nil {
		return model.AttachmentRef{}, err
	}
	resp, err := x.graph.Get(ctx, url.PathEscape(md.ID), string(token))
	if err != nil {
		return model.AttachmentRef{}, mediaError(err)
	}
	var obj mediaObject
	if json.Unmarshal(resp.Body, &obj) != nil || obj.URL == "" {
		return model.AttachmentRef{}, model.NewError(model.Permanent, "whatsapp media answer carried no URL")
	}
	body, err := x.graph.Download(ctx, obj.URL, string(token))
	if err != nil {
		return model.AttachmentRef{}, mediaError(err)
	}
	defer func() { _ = body.Close() }()
	desc := model.AttachmentMetadata{
		Name: md.Filename, MediaType: firstNonEmpty(obj.MimeType, md.MimeType), Audience: env.Audience,
	}
	if desc.Name == "" {
		desc.Name = typ
	}
	if obj.FileSize > 0 {
		desc.Size = obj.FileSize
	}
	if d := firstNonEmpty(obj.SHA256, md.SHA256); isHexDigest(d) {
		desc.Digest = "sha256:" + d
	}
	return sink.StageAttachment(ctx, adapter.StageRequest{Key: md.ID, Destination: env.Destination, Metadata: desc}, body)
}

// mediaError maps a failed media lookup or download: no answer, a rate
// limit or a server fault is transient; anything else is permanent.
func mediaError(err error) error {
	var te *meta.TransportError
	var ge *meta.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.As(err, &te):
		return model.NewError(model.Transient, "whatsapp media unreachable")
	case errors.As(err, &ge) && (ge.Status >= 500 || ge.Status == 429 || rateLimitCodes[ge.Code]):
		return model.NewError(model.Transient, "whatsapp media temporarily unavailable")
	}
	return model.NewError(model.Permanent, "whatsapp media unavailable")
}

func retryable(err error) bool {
	return model.CodeOf(err).DefaultRetry() == model.RetrySameOperation ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// isHexDigest reports a lowercase hex SHA-256.
func isHexDigest(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}
