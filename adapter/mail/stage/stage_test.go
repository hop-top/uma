package stage_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/stage"
	"hop.top/uma/model"
)

// fakeSink is a sink without an engine: it records each staging request
// and answers from fail.
type fakeSink struct {
	mu   sync.Mutex
	reqs []adapter.StageRequest
	body []string
	fail func(n int) error
}

func (f *fakeSink) Accept(context.Context, model.Ingress) (model.IngressReceipt, error) {
	return model.IngressReceipt{}, nil
}

func (f *fakeSink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, nil
}

func (f *fakeSink) StageAttachment(_ context.Context, r adapter.StageRequest, body io.Reader) (model.AttachmentRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.reqs)
	f.reqs = append(f.reqs, r)
	b, _ := io.ReadAll(body)
	f.body = append(f.body, string(b))
	if f.fail != nil {
		if err := f.fail(n); err != nil {
			return model.AttachmentRef{}, err
		}
	}
	return model.AttachmentRef{ID: model.ID("att-" + r.Key)}, nil
}

// plainSink is a sink that cannot stage.
type plainSink struct{}

func (plainSink) Accept(context.Context, model.Ingress) (model.IngressReceipt, error) {
	return model.IngressReceipt{}, nil
}

func (plainSink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, nil
}

func message(n int) *mime.Message {
	m := &mime.Message{}
	for i := range n {
		name := "f" + string(rune('a'+i)) + ".txt"
		data := []byte("bytes-" + name)
		m.Attachments = append(m.Attachments, mime.Attachment{
			Filename: name, MediaType: "text/plain", Size: int64(len(data)), Digest: "sha256:" + strings.Repeat("0", 64),
		})
		m.Data = append(m.Data, data)
	}
	return m
}

var req = stage.Request{Key: "msg-1", Destination: "support", Audience: model.Audience{Visibility: model.Public}}

func TestStageStagesKeptBytes(t *testing.T) {
	s, m := &fakeSink{}, message(2)
	m.Data[1] = nil // over the bound: described only
	parts, err := stage.Stage(t.Context(), s, req, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Kind != model.PartAttachment || parts[0].Attachment == nil {
		t.Fatalf("parts = %+v", parts)
	}
	if !m.Attachments[0].Staged || m.Attachments[1].Staged {
		t.Errorf("staged = %v %v, want true false", m.Attachments[0].Staged, m.Attachments[1].Staged)
	}
	r := s.reqs[0]
	if r.Destination != "support" || r.Metadata.Name != "fa.txt" || r.Metadata.MediaType != "text/plain" ||
		r.Metadata.Size != m.Attachments[0].Size || r.Metadata.Digest != m.Attachments[0].Digest || r.Metadata.Audience != req.Audience {
		t.Errorf("request = %+v", r)
	}
	if s.body[0] != "bytes-fa.txt" || len(r.Key) == 0 || len(r.Key) > 255 {
		t.Errorf("body %q key %q", s.body[0], r.Key)
	}
}

func TestStageKeysAreStablePerMessageAndPosition(t *testing.T) {
	a, b := &fakeSink{}, &fakeSink{}
	if _, err := stage.Stage(t.Context(), a, req, message(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Stage(t.Context(), b, req, message(2)); err != nil {
		t.Fatal(err)
	}
	if a.reqs[0].Key != b.reqs[0].Key || a.reqs[0].Key == a.reqs[1].Key {
		t.Errorf("keys %q %q vs %q", a.reqs[0].Key, a.reqs[1].Key, b.reqs[0].Key)
	}
	other := req
	other.Key = "msg-2"
	c := &fakeSink{}
	if _, err := stage.Stage(t.Context(), c, other, message(1)); err != nil {
		t.Fatal(err)
	}
	if c.reqs[0].Key == a.reqs[0].Key {
		t.Error("two messages share a staging key")
	}
}

// A key is "mail:" and the SHA-256 of the message key and the position,
// NUL-separated. Changing it restages every attachment already staged.
func TestStageKeyIsPinned(t *testing.T) {
	s := &fakeSink{}
	if _, err := stage.Stage(t.Context(), s, req, message(2)); err != nil {
		t.Fatal(err)
	}
	for n, r := range s.reqs {
		sum := sha256.Sum256([]byte("msg-1\x00" + string(rune('0'+n))))
		if want := "mail:" + hex.EncodeToString(sum[:]); r.Key != want {
			t.Errorf("key %d = %q, want %q", n, r.Key, want)
		}
	}
}

func TestStageNamesAnonymousAttachment(t *testing.T) {
	s, m := &fakeSink{}, message(1)
	m.Attachments[0].Filename, m.Attachments[0].MediaType = "", ""
	if _, err := stage.Stage(t.Context(), s, req, m); err != nil {
		t.Fatal(err)
	}
	if got := s.reqs[0].Metadata; got.Name != "attachment-1" || got.MediaType != "application/octet-stream" {
		t.Errorf("metadata = %+v", got)
	}
}

func TestStageWithoutStagingSink(t *testing.T) {
	m := message(1)
	parts, err := stage.Stage(t.Context(), plainSink{}, req, m)
	if err != nil || len(parts) != 0 || m.Attachments[0].Staged {
		t.Fatalf("parts %+v, staged %v, err %v", parts, m.Attachments[0].Staged, err)
	}
}

func TestStageDisabledStopsTrying(t *testing.T) {
	s := &fakeSink{fail: func(int) error { return model.NewError(model.Disabled, "no blob store") }}
	m := message(3)
	parts, err := stage.Stage(t.Context(), s, req, m)
	if err != nil || len(parts) != 0 {
		t.Fatalf("parts %+v, err %v: a message without a blob store is delivered as described", parts, err)
	}
	if len(s.reqs) != 1 {
		t.Errorf("requests = %d, want 1: Disabled holds for every attachment", len(s.reqs))
	}
	for _, a := range m.Attachments {
		if a.Staged {
			t.Error("marked staged")
		}
	}
}

func TestStageRefusedAttachmentIsSkipped(t *testing.T) {
	for _, code := range []model.ErrorCode{model.Validation, model.Conflict, model.Unauthorized, model.Permanent} {
		s := &fakeSink{fail: func(n int) error {
			if n == 0 {
				return model.NewError(code, "refused")
			}
			return nil
		}}
		m := message(2)
		parts, err := stage.Stage(t.Context(), s, req, m)
		if err != nil || len(parts) != 1 || m.Attachments[0].Staged || !m.Attachments[1].Staged {
			t.Errorf("%s: parts %d, staged %v %v, err %v", code, len(parts), m.Attachments[0].Staged, m.Attachments[1].Staged, err)
		}
	}
}

func TestStageTransientFailureRetriesTheMessage(t *testing.T) {
	for _, err := range []error{
		model.NewError(model.Transient, "blob store busy"), model.NewError(model.Unavailable, "down"),
		model.NewError(model.RateLimited, "slow"), context.Canceled, context.DeadlineExceeded,
	} {
		s := &fakeSink{fail: func(int) error { return err }}
		if _, got := stage.Stage(t.Context(), s, req, message(2)); !errors.Is(got, err) {
			t.Errorf("%v: err = %v", err, got)
		}
	}
}

// The bounds are documented on the adapter pages: 10 attachments of 25 MiB.
func TestStageBoundsAreTheDocumentedOnes(t *testing.T) {
	l := stage.Keep(mime.Limits{MaxTextBytes: 7})
	if l.MaxTextBytes != 7 || l.KeepParts != 10 || l.KeepBytes != 25<<20 {
		t.Errorf("limits = %+v", l)
	}
}
