package attachments

import (
	"mime"
	"strconv"
	"strings"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
)

// digestPrefix names the one digest algorithm: a digest is
// sha256:<64 lowercase hex>.
const digestPrefix = "sha256:"

// checkDigest accepts an empty (undeclared) digest or a sha256 one.
func checkDigest(d string) error {
	if d == "" {
		return nil
	}
	h, ok := strings.CutPrefix(d, digestPrefix)
	if !ok || len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		return model.NewError(model.Validation, "metadata.digest: not a sha256:<64 lowercase hex> digest")
	}
	return nil
}

// checkSize rejects a declared size above the limit.
func checkSize(size, limit int64) error {
	if size > limit {
		return model.NewError(model.Validation, "metadata.size: exceeds the attachment size limit")
	}
	return nil
}

// checkMediaType requires a well-formed media type and, when accepted is
// not empty, one of its entries: an exact type/subtype or a type/* match
// on the type without parameters.
func checkMediaType(v string, accepted []string) error {
	mt, _, err := mime.ParseMediaType(v)
	typ, sub, ok := strings.Cut(mt, "/")
	if err != nil || !ok || typ == "" || sub == "" || typ == "*" || sub == "*" {
		return model.NewError(model.Validation, "metadata.media_type: not a media type")
	}
	if len(accepted) == 0 {
		return nil
	}
	for _, a := range accepted {
		if a == mt || a == typ+"/*" {
			return nil
		}
	}
	return model.NewError(model.Validation, "metadata.media_type: not an accepted media type")
}

// prepareFingerprint returns the version 1 fingerprint of a preparation:
// kind, inbox and the declared metadata.
func prepareFingerprint(req model.PrepareAttachment) []byte {
	m := req.Metadata
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionAttachmentPrepare))
	f.Set("target", operations.Field().Ref(req.Inbox.Ref()))
	f.Set("metadata", operations.Field().Str(m.Name).Str(m.MediaType).Str(strconv.FormatInt(m.Size, 10)).Str(m.Digest))
	f.Set("audience", operations.Field().Audience(m.Audience))
	return f.Sum()
}

// uploadFingerprint returns the version 1 fingerprint of an upload: kind,
// attachment, the computed digest and length of the bytes, and the
// expected revision. Payload identity is the bytes, never the reader.
func uploadFingerprint(ref model.AttachmentRef, digest string, n int64, expected model.Revision) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionAttachmentUpload))
	f.Set("target", operations.Field().Ref(ref.Ref()))
	f.Set("content", operations.Field().Str(digest).Str(strconv.FormatInt(n, 10)))
	f.Set("expected_revision", operations.Field().Str(string(expected)))
	return f.Sum()
}
