package engine_test

import (
	"slices"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

func TestNewAttachmentSizeLimit(t *testing.T) {
	cfg := enginetest.ValidConfig()
	cfg.AttachmentSizeLimit = -1
	_, err := engine.New(cfg)
	enginetest.RequireCode(t, err, model.Validation, "Config.AttachmentSizeLimit: must not be negative")

	cfg.AttachmentSizeLimit = 0
	if got := enginetest.MustNew(t, cfg).AttachmentSizeLimit(); got != engine.DefaultAttachmentSizeLimit {
		t.Fatalf("zero limit = %d, want the default %d", got, engine.DefaultAttachmentSizeLimit)
	}
	cfg.AttachmentSizeLimit = 512
	if got := enginetest.MustNew(t, cfg).AttachmentSizeLimit(); got != 512 {
		t.Fatalf("limit = %d, want 512", got)
	}
}

func TestNewAttachmentMediaTypes(t *testing.T) {
	for _, bad := range [][]string{
		{"image"}, {"image/"}, {"*/*"}, {"*/png"}, {"text/plain; charset=utf-8"}, {"a/b/c"}, {""},
		{"text/plain", "Text/Plain"},
	} {
		cfg := enginetest.ValidConfig()
		cfg.AttachmentMediaTypes = bad
		_, err := engine.New(cfg)
		enginetest.RequireCode(t, err, model.Validation, "Config.AttachmentMediaTypes")
	}
	cfg := enginetest.ValidConfig()
	in := []string{"Text/Plain", "image/*"}
	cfg.AttachmentMediaTypes = in
	e := enginetest.MustNew(t, cfg)
	got := e.AttachmentMediaTypes()
	if want := []string{"text/plain", "image/*"}; !slices.Equal(got, want) {
		t.Fatalf("media types = %q, want %q", got, want)
	}
	in[0], got[1] = "changed", "changed"
	if again := e.AttachmentMediaTypes(); again[0] != "text/plain" || again[1] != "image/*" {
		t.Fatalf("media types alias caller memory: %q", again)
	}
}
