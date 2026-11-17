package slack

import (
	"testing"

	"hop.top/uma/model"
)

func TestReactionTableIsConsistent(t *testing.T) {
	names, emojis := map[string]bool{}, map[string]bool{}
	for _, r := range reactionNames {
		if err := model.ValidateEmoji(r.emoji); err != nil {
			t.Errorf("%q is no valid emoji: %v", r.emoji, err)
		}
		for _, n := range append([]string{r.name}, r.aliases...) {
			if names[n] {
				t.Errorf("name %q listed twice", n)
			}
			names[n] = true
		}
		if emojis[r.emoji] {
			t.Errorf("emoji %q listed twice", r.emoji)
		}
		emojis[r.emoji] = true
		if got, ok := reactionEmoji(r.name); !ok || got != r.emoji {
			t.Errorf("name %q reads as %q", r.name, got)
		}
		if got, ok := reactionName(r.emoji); !ok || got != r.name {
			t.Errorf("emoji %q is sent as %q", r.emoji, got)
		}
	}
}

func TestReactionNamesRead(t *testing.T) {
	for in, want := range map[string]string{"thumbsup": "👍", "+1": "👍", "-1": "👎", "heart": "❤️"} {
		if got, ok := reactionEmoji(in); !ok || got != want {
			t.Errorf("reactionEmoji(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "partyparrot", "+1::skin-tone-3", ":thumbsup:"} {
		if got, ok := reactionEmoji(in); ok {
			t.Errorf("reactionEmoji(%q) = %q, want none", in, got)
		}
	}
	// A variant of an emoji is another emoji: never sent as its base.
	for _, in := range []string{"👍🏽", "❤", "👍️"} {
		if got, ok := reactionName(in); ok {
			t.Errorf("reactionName(%q) = %q, want none", in, got)
		}
	}
}
