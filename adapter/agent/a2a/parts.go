package a2a

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Limits of an inbound message.
const (
	maxMessageIDBytes = 128
	maxReferenceTasks = 64
	maxFilenameBytes  = 255
)

// digest is a length-prefixed hash of fields, so no two field lists share
// one.
func digest(fields ...string) string {
	h := sha256.New()
	for _, s := range fields {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// inboundID is the engine message ID of a client message: scoped to its
// authenticated agent, so equal client IDs of two agents never collide in
// deduplication, and stable across the agent's retries.
func inboundID(principal model.ExternalRef, clientID string) string {
	return "a" + digest("message", principal.Namespace, principal.ID, clientID)
}

// validClientID reports whether id can be a client-chosen message ID.
func validClientID(id string) bool {
	if id == "" || len(id) > maxMessageIDBytes || !utf8.ValidString(id) {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c <= ' ' || c == 0x7f {
			return false
		}
	}
	return true
}

// inboundContent converts the parts of a client message to uma content.
// Text is text; data is a structured SchemaData part; a url is a
// structured SchemaFileURL part, never fetched; raw bytes are staged
// through the sink first, so Accept can name the attachment. Part
// metadata is ignored, a part naming none or several of its content
// fields is a violation.
func (c *call) inboundContent(ctx context.Context, clientID, destination string, parts []wirePart) (model.Content, *rpcError) {
	if len(parts) == 0 {
		return model.Content{}, invalidParams("message.parts", "at least one part is required")
	}
	var out model.Content
	for i, p := range parts {
		field := "message.parts[" + strconv.Itoa(i) + "]"
		set := 0
		for _, present := range []bool{p.Text != nil, p.Raw != nil, p.URL != nil, len(p.Data) > 0} {
			if present {
				set++
			}
		}
		if set != 1 {
			return model.Content{}, invalidParams(field, "must hold exactly one of text, raw, url and data")
		}
		switch {
		case p.Text != nil:
			if *p.Text == "" {
				return model.Content{}, invalidParams(field+".text", "must not be empty")
			}
			out.Parts = append(out.Parts, model.Part{Kind: "text", Text: *p.Text})
		case len(p.Data) > 0:
			ext := model.Extension{Name: SchemaData, Version: 1, Data: bytes.Clone(p.Data)}
			out.Parts = append(out.Parts, model.Part{Kind: "structured", Structured: &ext})
		case p.URL != nil:
			data, err := json.Marshal(fileURL{URL: *p.URL, MediaType: p.MediaType, Filename: p.Filename})
			if err != nil || *p.URL == "" {
				return model.Content{}, invalidParams(field+".url", "must not be empty")
			}
			ext := model.Extension{Name: SchemaFileURL, Version: 1, Data: data}
			out.Parts = append(out.Parts, model.Part{Kind: "structured", Structured: &ext})
		default:
			ref, rerr := c.stage(ctx, field, clientID, destination, i, p)
			if rerr != nil {
				return model.Content{}, rerr
			}
			out.Parts = append(out.Parts, model.Part{Kind: "attachment", Attachment: &ref})
		}
	}
	return out, nil
}

// stage stores the bytes of a raw part. Its key derives from the agent,
// the message and the part, so a retried message stages the same bytes
// under the same key.
func (c *call) stage(ctx context.Context, field, clientID, destination string, i int, p wirePart) (model.AttachmentRef, *rpcError) {
	data, err := base64.StdEncoding.DecodeString(*p.Raw)
	if err != nil || len(data) == 0 {
		return model.AttachmentRef{}, invalidParams(field+".raw", "must be non-empty base64")
	}
	if len(p.Filename) > maxFilenameBytes || !utf8.ValidString(p.Filename) {
		return model.AttachmentRef{}, invalidParams(field+".filename", "is not a valid file name")
	}
	name, media := p.Filename, p.MediaType
	if name == "" {
		name = "part-" + strconv.Itoa(i)
	}
	if media == "" {
		media = "application/octet-stream"
	}
	ref, err := c.s.files.StageAttachment(ctx, adapter.StageRequest{
		Key:         "a2a." + digest("attachment", c.principal.ID, clientID, strconv.Itoa(i)),
		Destination: destination,
		Metadata: model.AttachmentMetadata{
			Name: name, MediaType: media, Size: int64(len(data)),
			Audience: model.Audience{Visibility: model.Public},
		},
	}, bytes.NewReader(data))
	if err != nil {
		if model.IsCode(err, model.Validation) || model.IsCode(err, model.Unsupported) {
			return model.AttachmentRef{}, invalidParams(field, "cannot be stored: "+string(model.CodeOf(err)))
		}
		return model.AttachmentRef{}, internalError(c.x.f.domain, err)
	}
	return ref, nil
}
