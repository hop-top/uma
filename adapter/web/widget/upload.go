package widget

import (
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Visitor uploads. POST .../uploads?id=<client ID>&name=<file name> takes
// the file as the raw body under its own Content-Type, with the session
// token as a bearer token, like messages. The bytes are staged through the
// engine's attachment sink (adapter.AttachmentSink) under the attachments
// configuration: the engine bounds the body by Config.AttachmentSizeLimit
// (the request is an upload, adapter.UploadReceiver) and checks the
// accepted media types, a declared Content-Length and the audience. The
// answer is an UploadResponse whose handle a later MessageRequest lists in
// Attachments. A handle is signed with the session key and bound to the
// visitor, so no visitor can attach another's upload. Staging is
// idempotent per visitor and client ID: retrying an upload with the same
// bytes returns the same handle.

// maxAttachments bounds the attachments of one visitor message.
const maxAttachments = 10

// maxNameBytes bounds an upload's file name.
const maxNameBytes = 255

// handlePrefix versions the upload handle format; it is part of the MAC
// input.
const handlePrefix = "u1."

// UploadResponse is the JSON answer to a staged upload: the handle to list
// in MessageRequest.Attachments.
type UploadResponse struct {
	Attachment string `json:"attachment"`
}

// Upload reports a visitor upload, which the engine bounds by the
// attachment size limit.
func (x *Instance) Upload(r *http.Request) bool {
	return r.Method == http.MethodPost && path.Base(r.URL.Path) == "uploads"
}

func (x *Instance) serveUpload(w http.ResponseWriter, r *http.Request, sink adapter.Sink, c claims, key []byte) {
	q := r.URL.Query()
	id, name := q.Get("id"), q.Get("name")
	if !validClientID(id) || len(name) > maxNameBytes {
		status(w, http.StatusBadRequest)
		return
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		status(w, http.StatusUnsupportedMediaType)
		return
	}
	stager, ok := sink.(adapter.AttachmentSink)
	if !ok {
		status(w, http.StatusNotImplemented)
		return
	}
	if name == "" {
		name = "upload"
	}
	ref, err := stager.StageAttachment(r.Context(), adapter.StageRequest{
		Key: inboundID("upload", c, id), Destination: c.Site,
		Metadata: model.AttachmentMetadata{Name: name, MediaType: mt, Size: max(r.ContentLength, 0), Audience: model.Audience{Visibility: model.Public}},
	}, r.Body)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		status(w, http.StatusRequestEntityTooLarge)
		return
	case model.IsCode(err, model.Disabled):
		status(w, http.StatusNotImplemented)
		return
	case err != nil:
		sinkError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, UploadResponse{Attachment: uploadHandle(key, x.ref(), c, ref)})
}

// uploadHandle is the visitor's handle on a staged attachment: its inbox
// and ID, with a MAC binding them to the channel and visitor.
func uploadHandle(key []byte, ch model.ChannelRef, c claims, ref model.AttachmentRef) string {
	body := handlePrefix + string(ref.Inbox.ID) + "." + string(ref.ID)
	return body + "." + base64.RawURLEncoding.EncodeToString(mac(key, handleInput(ch, c, body)))
}

func handleInput(ch model.ChannelRef, c claims, body string) string {
	var b strings.Builder
	for _, s := range []string{"upload", string(ch.Tenant), string(ch.ID), c.namespace(), c.Visitor, body} {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	return b.String()
}

// attachmentOf verifies a handle for the visitor and returns its
// attachment.
func attachmentOf(key []byte, ch model.ChannelRef, c claims, handle string) (model.AttachmentRef, bool) {
	if len(handle) > maxTokenBytes || !strings.HasPrefix(handle, handlePrefix) {
		return model.AttachmentRef{}, false
	}
	dot := strings.LastIndexByte(handle, '.')
	if dot <= len(handlePrefix) {
		return model.AttachmentRef{}, false
	}
	body, sig := handle[:dot], handle[dot+1:]
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(key, handleInput(ch, c, body))) {
		return model.AttachmentRef{}, false
	}
	inbox, id, ok := strings.Cut(body[len(handlePrefix):], ".")
	ref := model.AttachmentRef{Inbox: model.InboxRef{Tenant: ch.Tenant, ID: model.ID(inbox)}, ID: model.ID(id)}
	if !ok || ref.Validate() != nil {
		return model.AttachmentRef{}, false
	}
	return ref, true
}
