package ingress_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
)

// publicReader allows message and attachment reads only under a public
// audience; every other action is allowed.
type publicReader struct{}

func (publicReader) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	read := a.Action == model.ActionMessageRead || a.Action == model.ActionAttachmentRead
	return policy.Decision{Allow: !read || a.Audience.Visibility == model.Public}, nil
}

const attachmentBytes = "attached bytes"

// openAs opens ref as c and returns its bytes or the error.
func openAs(t *testing.T, h *harness, c enginetest.Client, ref model.AttachmentRef) (string, error) {
	t.Helper()
	_, rc, err := c.Attachments().Open(h.Ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: ref})
	if err != nil {
		if rc != nil {
			t.Fatal("a refused Open returned a reader")
		}
		return "", err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), nil
}

func stagePublic(t *testing.T, h *harness, key string) model.AttachmentRef {
	t.Helper()
	ref, err := stager(t, h).StageAttachment(h.Ctx, adapter.StageRequest{Key: key, Destination: dest, Metadata: image(0, "", public())}, strings.NewReader(attachmentBytes))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func narrowingEdit(h *harness, event, msgID string, at time.Time, content bool) model.Envelope {
	env := h.Inbound(event, msgID, "u1", dest)
	env.Kind, env.SourceRevision, env.OccurredAt = model.EnvelopeMessageEdited, event, at
	env.Audience = model.Audience{Visibility: model.Internal}
	if !content {
		env.Content = model.Content{}
	}
	return env
}

// An attachment is read under the current audience of the messages that
// hold it: once an edit narrows its message, a caller who may read only
// public content can open neither the message nor the attachment, whether
// the edit carries no content or is older than the applied content.
func TestNarrowedMessageRestrictsItsAttachment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		at      time.Duration
		content bool
	}{
		{"audience-only edit", 2 * time.Minute, false},
		{"stale edit", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, chatType, func(c *engine.Config) { c.Blobs = &portstest.MemoryBlobs{}; c.Authorizer = publicReader{} })
			in := h.Configure(perContact(false), dest)
			ref := stagePublic(t, h, "media")
			orig := withAttachment(h.Inbound("e-new", "m", "u1", dest), ref)
			orig.OccurredAt = t0.Add(time.Minute)
			h.MustAccept(orig)
			if got, err := openAs(t, h, h.Client, ref); err != nil || got != attachmentBytes {
				t.Fatalf("before narrowing: %q, %v", got, err)
			}
			h.MustAccept(narrowingEdit(h, "e-edit", "m", t0.Add(tc.at), tc.content))
			m := h.MessageByProviderID(in.Ref, "m")
			_, err := h.Client.Messages().Get(h.Ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "get"}, Ref: m.Ref})
			requireCode(t, err, model.Unauthorized, "")
			_, err = openAs(t, h, h.Client, ref)
			requireCode(t, err, model.Unauthorized, "")
		})
	}
}

// An attachment several messages hold stays readable while any of them is
// readable, and is refused once every holder is narrowed.
func TestSharedAttachmentFollowsEveryHolder(t *testing.T) {
	h := newHarness(t, chatType, func(c *engine.Config) { c.Blobs = &portstest.MemoryBlobs{}; c.Authorizer = publicReader{} })
	h.Configure(perContact(false), dest)
	ref := stagePublic(t, h, "shared")
	h.MustAccept(withAttachment(h.Inbound("e-1", "m1", "u1", dest), ref))
	h.MustAccept(withAttachment(h.Inbound("e-2", "m2", "u1", dest), ref))

	h.MustAccept(narrowingEdit(h, "e-1-edit", "m1", t0.Add(time.Minute), false))
	if got, err := openAs(t, h, h.Client, ref); err != nil || got != attachmentBytes {
		t.Fatalf("with one public holder left: %q, %v", got, err)
	}
	h.MustAccept(narrowingEdit(h, "e-2-edit", "m2", t0.Add(time.Minute), false))
	_, err := openAs(t, h, h.Client, ref)
	requireCode(t, err, model.Unauthorized, "")
}

// An attachment no message holds, staged and not yet referenced or left
// behind by a deleted message, is readable by the principal that staged it
// only.
func TestUnheldAttachmentReadableByItsStagerOnly(t *testing.T) {
	h := newHarness(t, chatType, func(c *engine.Config) { c.Blobs = &portstest.MemoryBlobs{} })
	h.Configure(perContact(false), dest)
	svc, err := enginetest.For(h.E, h.Service())
	if err != nil {
		t.Fatal(err)
	}
	ref := stagePublic(t, h, "media")
	_, err = openAs(t, h, h.Client, ref)
	requireCode(t, err, model.Unauthorized, "")
	if got, err := openAs(t, h, svc, ref); err != nil || got != attachmentBytes {
		t.Fatalf("stager before the message: %q, %v", got, err)
	}

	h.MustAccept(withAttachment(h.Inbound("e-new", "m", "u1", dest), ref))
	if got, err := openAs(t, h, h.Client, ref); err != nil || got != attachmentBytes {
		t.Fatalf("held by a message: %q, %v", got, err)
	}

	del := h.Inbound("e-del", "m", "u1", dest)
	del.Kind, del.Content, del.OccurredAt = model.EnvelopeMessageDeleted, model.Content{}, t0.Add(time.Minute)
	h.MustAccept(del)
	_, err = openAs(t, h, h.Client, ref)
	requireCode(t, err, model.Unauthorized, "")
	if got, err := openAs(t, h, svc, ref); err != nil || got != attachmentBytes {
		t.Fatalf("stager after the delete: %q, %v", got, err)
	}
}
