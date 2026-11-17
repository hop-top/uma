package telegram

import (
	"testing"

	"hop.top/uma/model"
)

func TestReactionEmojiAreTheDocumentedSet(t *testing.T) {
	// "Currently, it can be one of" 73 emoji (ReactionTypeEmoji).
	if len(reactionEmoji) != 73 {
		t.Fatalf("%d emoji, want the 73 the Bot API lists", len(reactionEmoji))
	}
	seen := map[string]bool{}
	for _, e := range reactionEmoji {
		if seen[e] {
			t.Errorf("%q listed twice", e)
		}
		seen[e] = true
		if err := model.ValidateEmoji(e); err != nil {
			t.Errorf("%q is no emoji core accepts: %v", e, err)
		}
		if got, ok := telegramEmoji(e); !ok || got != e {
			t.Errorf("telegramEmoji(%q) = %q, %v", e, got, ok)
		}
	}
}

// The Bot API prints several emoji without the variation selector; the heart
// most people type has it. Only that selector is forgiven.
func TestTelegramEmojiForgivesTheVariationSelectorOnly(t *testing.T) {
	for in, want := range map[string]string{
		"❤️":         "❤",
		"❤":          "❤",
		"⚡️":         "⚡",
		"✍️":         "✍",
		"\U0001f44d": "\U0001f44d",
	} {
		if got, ok := telegramEmoji(in); !ok || got != want {
			t.Errorf("telegramEmoji(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "x", "😂", "👍🏽", "🤷‍♂️ x", "❤‍"} {
		if got, ok := telegramEmoji(in); ok {
			t.Errorf("telegramEmoji(%q) = %q, want none", in, got)
		}
	}
}
