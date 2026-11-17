package staging_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/staging"
	"hop.top/uma/model"
)

// sink answers every staging request from fail and records the request and
// the bytes it read.
type sink struct {
	req  adapter.StageRequest
	body string
	fail error
}

func (s *sink) StageAttachment(_ context.Context, r adapter.StageRequest, body io.Reader) (model.AttachmentRef, error) {
	s.req = r
	b, _ := io.ReadAll(body)
	s.body = string(b)
	if s.fail != nil {
		return model.AttachmentRef{}, s.fail
	}
	return model.AttachmentRef{ID: "att-1"}, nil
}

func TestStageStreamsTheBodyUnderTheRequest(t *testing.T) {
	s := &sink{}
	req := adapter.StageRequest{Key: "k", Destination: "support", Metadata: model.AttachmentMetadata{Name: "a.txt", MediaType: "text/plain"}}
	ref, outcome, err := staging.Stage(t.Context(), s, req, strings.NewReader("bytes"))
	if err != nil || outcome != staging.Staged || ref.ID != "att-1" {
		t.Fatalf("ref %+v, outcome %v, err %v", ref, outcome, err)
	}
	if s.req != req || s.body != "bytes" {
		t.Errorf("sink saw %+v %q", s.req, s.body)
	}
}

func TestStageClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		outcome staging.Outcome
		retry   bool
	}{
		{"transient", model.NewError(model.Transient, "busy"), staging.Retry, true},
		{"unavailable", model.NewError(model.Unavailable, "down"), staging.Retry, true},
		{"rate limited", model.NewError(model.RateLimited, "slow"), staging.Retry, true},
		{"canceled", context.Canceled, staging.Retry, true},
		{"deadline", context.DeadlineExceeded, staging.Retry, true},
		{"wrapped", fmt.Errorf("stage: %w", model.NewError(model.Transient, "busy")), staging.Retry, true},
		{"disabled", model.NewError(model.Disabled, "no blob store"), staging.Disabled, false},
		{"validation", model.NewError(model.Validation, "refused"), staging.Skipped, false},
		{"conflict", model.NewError(model.Conflict, "other bytes"), staging.Skipped, false},
		{"unauthorized", model.NewError(model.Unauthorized, "audience"), staging.Skipped, false},
		{"permanent", model.NewError(model.Permanent, "gone"), staging.Skipped, false},
		{"plain", errors.New("boom"), staging.Skipped, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, outcome, err := staging.Stage(t.Context(), &sink{fail: tc.err}, adapter.StageRequest{Key: "k"}, strings.NewReader("x"))
			if outcome != tc.outcome {
				t.Errorf("outcome = %v, want %v", outcome, tc.outcome)
			}
			if tc.retry != (err != nil) || (tc.retry && !errors.Is(err, tc.err)) {
				t.Errorf("err = %v, want the sink's error only for a retry", err)
			}
			if got := staging.Retryable(tc.err); got != tc.retry {
				t.Errorf("Retryable = %v, want %v", got, tc.retry)
			}
		})
	}
}

func TestRetryableIsNotRaisedForNil(t *testing.T) {
	if staging.Retryable(nil) {
		t.Error("nil error is retryable")
	}
}

// OpaqueKey is the SHA-256 of its parts joined by NUL, prefixed. The
// derivation is pinned: a different one restages every attachment already
// staged under the old keys.
func TestOpaqueKeyDerivation(t *testing.T) {
	sum := sha256.Sum256([]byte("dest\x00msg\x003"))
	want := "mail:" + hex.EncodeToString(sum[:])
	if got := staging.OpaqueKey("mail:", "dest", "msg", "3"); got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
}

func TestOpaqueKeyIsBoundedStableAndSeparatesParts(t *testing.T) {
	long := strings.Repeat("x", 4096)
	k := staging.OpaqueKey("mail:", long, "1")
	if len(k) > 255 || len(k) != len("mail:")+64 {
		t.Errorf("key length %d", len(k))
	}
	if k != staging.OpaqueKey("mail:", long, "1") {
		t.Error("key is not stable")
	}
	if staging.OpaqueKey("mail:", "ab", "c") == staging.OpaqueKey("mail:", "a", "bc") {
		t.Error("parts are not separated")
	}
	if staging.OpaqueKey("mail:", "m", "1") == staging.OpaqueKey("mail:", "m", "2") {
		t.Error("positions share a key")
	}
}

// The bounds are the documented ones on the mail adapter pages.
func TestBoundsAreTheDocumentedOnes(t *testing.T) {
	if staging.MaxAttachments != 10 || staging.MaxAttachmentBytes != 25<<20 {
		t.Errorf("bounds = %d, %d", staging.MaxAttachments, staging.MaxAttachmentBytes)
	}
}

func TestHistorySinkStagesThroughTheStagerAndAcceptsNothing(t *testing.T) {
	s := &sink{}
	h := staging.HistorySink(s)
	as, ok := h.(adapter.AttachmentSink)
	if !ok {
		t.Fatal("HistorySink with a stager does not stage")
	}
	if _, err := as.StageAttachment(t.Context(), adapter.StageRequest{Key: "k"}, strings.NewReader("x")); err != nil || s.body != "x" {
		t.Fatalf("stage: %v, body %q", err, s.body)
	}
	if _, err := h.Accept(t.Context(), model.Ingress{}); model.CodeOf(err) != model.Unsupported {
		t.Errorf("Accept = %v, want unsupported", err)
	}
	if _, err := h.EvaluateInteraction(t.Context(), model.InteractionInput{}); model.CodeOf(err) != model.Unsupported {
		t.Errorf("EvaluateInteraction = %v, want unsupported", err)
	}
}

func TestHistorySinkWithoutStagerStagesNothing(t *testing.T) {
	h := staging.HistorySink(nil)
	if _, ok := h.(adapter.AttachmentSink); ok {
		t.Fatal("HistorySink(nil) offers staging")
	}
	if _, err := h.Accept(t.Context(), model.Ingress{}); model.CodeOf(err) != model.Unsupported {
		t.Errorf("Accept = %v, want unsupported", err)
	}
}
