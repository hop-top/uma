package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"hop.top/uma/model"
)

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return digestPrefix + hex.EncodeToString(s[:])
}

func requireCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	var me *model.Error
	if !errors.As(err, &me) || me.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func TestStagerAcceptsDeclaredBytes(t *testing.T) {
	data := []byte("declared bytes")
	s := newStager(t.Context(), bytes.NewReader(data), int64(len(data)), 1024, digestOf(data))
	got, err := io.ReadAll(s)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %q, %v", got, err)
	}
	if err := s.complete(); err != nil {
		t.Fatal(err)
	}
	if d, n := s.sum(); d != digestOf(data) || n != int64(len(data)) {
		t.Fatalf("sum = %s %d", d, n)
	}
}

func TestStagerRejects(t *testing.T) {
	data := []byte("0123456789")
	for _, tc := range []struct {
		name   string
		size   int64
		limit  int64
		digest string
	}{
		{"PastLimit", 0, 9, ""},
		{"LongerThanDeclared", 9, 1024, ""},
		{"ShorterThanDeclared", 11, 1024, ""},
		{"DigestMismatch", 0, 1024, digestOf([]byte("other"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStager(t.Context(), bytes.NewReader(data), tc.size, tc.limit, tc.digest)
			_, err := io.ReadAll(s)
			requireCode(t, err, model.Validation)
			// The failure sticks.
			_, again := s.Read(make([]byte, 4))
			if !errors.Is(again, err) {
				t.Fatalf("second read = %v, want %v", again, err)
			}
			requireCode(t, s.complete(), model.Validation)
		})
	}
}

// A store reading in large chunks never receives more than one byte past
// the limit before the stager fails.
func TestStagerBoundsLargeReads(t *testing.T) {
	src := &countingSource{r: bytes.NewReader(make([]byte, 1<<20))}
	s := newStager(t.Context(), src, 0, 100, "")
	_, err := s.Read(make([]byte, 1<<20))
	requireCode(t, err, model.Validation)
	if src.n > 101 {
		t.Fatalf("read %d bytes from the body with limit 100", src.n)
	}
}

type countingSource struct {
	r io.Reader
	n int
}

func (c *countingSource) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// A store that stops at the declared length without reading EOF still
// completes; one that stops early kept a partial body.
func TestStagerComplete(t *testing.T) {
	data := []byte("exact")
	s := newStager(t.Context(), bytes.NewReader(data), int64(len(data)), 1024, "")
	if _, err := io.CopyN(io.Discard, s, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := s.complete(); err != nil {
		t.Fatalf("complete after the declared length: %v", err)
	}
	s = newStager(t.Context(), strings.NewReader("partial body"), 0, 1024, "")
	if _, err := io.CopyN(io.Discard, s, 3); err != nil {
		t.Fatal(err)
	}
	requireCode(t, s.complete(), model.Unavailable)
}

func TestStagerCancellationAndReaderFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s := newStager(ctx, strings.NewReader("x"), 0, 10, "")
	if _, err := s.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read = %v, want context.Canceled", err)
	}
	s = newStager(t.Context(), iotestErr{}, 0, 10, "")
	_, err := s.Read(make([]byte, 1))
	requireCode(t, err, model.Transient)
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error %q carries the body's error text", err)
	}
}

type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, errors.New("secret detail") }

func TestVerifiedReader(t *testing.T) {
	data := []byte("stored bytes")
	ok := newVerifiedReader(io.NopCloser(bytes.NewReader(data)), int64(len(data)), digestOf(data))
	if got, err := io.ReadAll(ok); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %q, %v", got, err)
	}
	for name, body := range map[string][]byte{
		"Longer":  append(bytes.Clone(data), '!'),
		"Shorter": data[:4],
		"Changed": bytes.ToUpper(data),
	} {
		t.Run(name, func(t *testing.T) {
			v := newVerifiedReader(io.NopCloser(bytes.NewReader(body)), int64(len(data)), digestOf(data))
			_, err := io.ReadAll(v)
			requireCode(t, err, model.Permanent)
		})
	}
}

func TestCheckMetadata(t *testing.T) {
	sum := digestOf([]byte("x"))
	for _, d := range []string{"", sum} {
		if err := checkDigest(d); err != nil {
			t.Errorf("checkDigest(%q) = %v", d, err)
		}
	}
	for _, d := range []string{"sha256:" + strings.ToUpper(sum[7:]), sum[:70], "sha512:" + sum[7:], sum[7:], sum + "0"} {
		requireCode(t, checkDigest(d), model.Validation)
	}
	requireCode(t, checkSize(11, 10), model.Validation)
	if err := checkSize(10, 10); err != nil {
		t.Fatal(err)
	}
	accepted := []string{"text/plain", "image/*"}
	for _, mt := range []string{"text/plain", "TEXT/plain; charset=utf-8", "image/png"} {
		if err := checkMediaType(mt, accepted); err != nil {
			t.Errorf("checkMediaType(%q) = %v", mt, err)
		}
	}
	for _, mt := range []string{"text/html", "", "image", "*/*", "image/*"} {
		requireCode(t, checkMediaType(mt, accepted), model.Validation)
	}
	if err := checkMediaType("application/zip", nil); err != nil {
		t.Fatalf("no accepted list: %v", err)
	}
}
