package bot_test

import (
	"context"
	"regexp"
	"testing"

	"hop.top/uma/companions/bot"
	"hop.top/uma/model"
)

var regexpAll = regexp.MustCompile(``)

func TestTextContentIsOneTextPart(t *testing.T) {
	c := bot.TextContent("hi")
	if len(c.Parts) != 1 || c.Parts[0].Kind != model.PartText || c.Parts[0].Text != "hi" {
		t.Fatalf("content = %+v, want one text part", c)
	}
	if got := bot.Text(c); got != "hi" {
		t.Fatalf("Text round trip = %q", got)
	}
}

// ReplyText is Reply(TextContent): same content, same operation ID.
func TestReplyTextMatchesReply(t *testing.T) {
	c := newFake()
	cfg := config(c)
	var want model.OperationID
	cfg.Patterns = []bot.Pattern{{Name: "all", Match: regexpAll, Handle: func(ctx context.Context, tn *bot.Turn) error {
		want = tn.OperationID("reply/0")
		_, err := tn.ReplyText(ctx, "pong")
		return err
	}}}
	b := mustNew(t, cfg)
	c.ev.push(c.message(t, 1, conv(1), "ping"))
	step(t, b)
	sent := c.msgs.sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d replies, want 1", len(sent))
	}
	if sent[0].Operation != want {
		t.Fatalf("operation = %q, want %q", sent[0].Operation, want)
	}
	if got := bot.Text(sent[0].Content); got != "pong" {
		t.Fatalf("content = %q", got)
	}
}
