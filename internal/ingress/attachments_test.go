package ingress_test

import (
	"io"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

const pngDigest = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" // sha256("hello")

func stager(t *testing.T, h *harness) adapter.AttachmentSink {
	t.Helper()
	var s adapter.Sink = h.Sink
	st, ok := s.(adapter.AttachmentSink)
	if !ok {
		t.Fatal("engine sink does not implement adapter.AttachmentSink")
	}
	return st
}

func public() model.Audience { return model.Audience{Visibility: model.Public} }

func image(size int64, digest string, a model.Audience) model.AttachmentMetadata {
	return model.AttachmentMetadata{Name: "photo.png", MediaType: "image/png", Size: size, Digest: digest, Audience: a}
}

func withAttachment(env model.Envelope, ref model.AttachmentRef) model.Envelope {
	env.Content.Parts = append(env.Content.Parts, model.Part{Kind: model.PartAttachment, Attachment: &ref})
	return env
}

// Inbound bytes staged through the sink land in the destination's inbox
// under the attachment checks; a redelivery restages to the same
// attachment, and the message referencing it is stored.
func TestSinkStagesInboundAttachments(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	h := newHarness(t, chatType, func(c *engine.Config) {
		c.Blobs = blobs
		c.AttachmentSizeLimit = 8
		c.AttachmentMediaTypes = []string{"image/*"}
	})
	in := h.Configure(perContact(false), dest)
	st := stager(t, h)
	req := adapter.StageRequest{Key: "media-1", Destination: dest, Metadata: image(5, pngDigest, public())}
	ref, err := st.StageAttachment(h.Ctx, req, strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("StageAttachment: %v", err)
	}
	if !ref.In(in.Ref) {
		t.Fatalf("attachment %+v, want it in the destination inbox %+v", ref, in.Ref)
	}
	again, err := st.StageAttachment(h.Ctx, req, strings.NewReader("hello"))
	if err != nil || again != ref {
		t.Fatalf("restage = %+v, %v; want the same attachment %+v", again, err, ref)
	}
	if n := len(blobs.Keys()); n != 1 {
		t.Fatalf("blobs kept = %d, want 1", n)
	}
	_, err = st.StageAttachment(h.Ctx, adapter.StageRequest{Key: "media-1", Destination: dest, Metadata: image(0, "", public())}, strings.NewReader("other"))
	requireCode(t, err, model.Conflict, "")

	for name, c := range map[string]struct {
		req  adapter.StageRequest
		body io.Reader
		code model.ErrorCode
	}{
		"over the size limit": {adapter.StageRequest{Key: "m2", Destination: dest, Metadata: image(0, "", public())}, strings.NewReader("123456789"), model.Validation},
		"refused media type":  {adapter.StageRequest{Key: "m3", Destination: dest, Metadata: model.AttachmentMetadata{Name: "a.txt", MediaType: "text/plain", Audience: public()}}, strings.NewReader("hi"), model.Validation},
		"digest mismatch":     {adapter.StageRequest{Key: "m4", Destination: dest, Metadata: image(0, pngDigest, public())}, strings.NewReader("bye"), model.Validation},
		"unbound destination": {adapter.StageRequest{Key: "m5", Destination: "nowhere", Metadata: image(0, "", public())}, strings.NewReader("hi"), model.NotFound},
		"no key":              {adapter.StageRequest{Destination: dest, Metadata: image(0, "", public())}, strings.NewReader("hi"), model.Validation},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := st.StageAttachment(h.Ctx, c.req, c.body)
			requireCode(t, err, c.code, "")
		})
	}
	if n := len(blobs.Keys()); n != 1 {
		t.Fatalf("blobs kept = %d after refused stagings, want 1", n)
	}

	h.MustAccept(withAttachment(h.Inbound("e1", "m1", "u1", dest), ref))
	msg := h.MessageByProviderID(in.Ref, "m1")
	if p := msg.Content.Parts[len(msg.Content.Parts)-1]; p.Attachment == nil || *p.Attachment != ref {
		t.Fatalf("message parts %+v, want the staged attachment", msg.Content.Parts)
	}
}

// An attachment part must name a staged attachment of the envelope's
// inbox under the envelope's audience.
func TestIngressRefusesUnstagedAttachments(t *testing.T) {
	h := newHarness(t, chatType, func(c *engine.Config) { c.Blobs = &portstest.MemoryBlobs{} })
	in := h.Configure(perContact(false), dest)
	other := h.Configure(perContact(false), "sales")
	st := stager(t, h)
	inOther, err := st.StageAttachment(h.Ctx, adapter.StageRequest{Key: "k1", Destination: "sales", Metadata: image(0, "", public())}, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	internal, err := st.StageAttachment(h.Ctx, adapter.StageRequest{Key: "k2", Destination: dest, Metadata: image(0, "", model.Audience{Visibility: model.Internal})}, strings.NewReader("y"))
	if err != nil {
		t.Fatal(err)
	}
	if !inOther.In(other.Ref) || !internal.In(in.Ref) {
		t.Fatal("staged in the wrong inbox")
	}
	unstaged := model.AttachmentRef{Inbox: in.Ref, ID: h.ID()}
	for name, ref := range map[string]model.AttachmentRef{
		"another inbox's":    inOther,
		"a wider audience":   internal,
		"a never staged one": unstaged,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.Accept(withAttachment(h.Inbound("e-"+name, "m-"+name, "u1", dest), ref))
			requireCode(t, err, model.Validation, "attachment")
		})
	}
	if n := len(h.Messages(in.Ref)); n != 0 {
		t.Fatalf("messages = %d, want none", n)
	}
}
