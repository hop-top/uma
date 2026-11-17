package ingress_test

import (
	"strconv"
	"strings"
	"testing"

	"hop.top/uma/model"
)

// Inbound reactions follow model.ValidateEmoji, the rule outbound
// reactions follow.
func TestIngressReactionEmojiRule(t *testing.T) {
	h := newHarness(t, chatType)
	h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	for i, tc := range []struct {
		emoji string
		ok    bool
	}{
		{"👍", true},
		{"👍\uFE0F", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"a b", false},
		{"a\u00a0b", false},
		{"a\u2003b", false},
		{"a\x01", false},
		{"\xff", false},
	} {
		react := h.Inbound("r"+strconv.Itoa(i), "m1", "u2", dest)
		react.Kind = model.EnvelopeReactionAdded
		react.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: tc.emoji}}}
		_, err := h.Accept(react)
		if tc.ok != (err == nil) || (err != nil && !model.IsCode(err, model.Validation)) {
			t.Errorf("emoji %q: err %v, want ok %v", tc.emoji, err, tc.ok)
		}
		if model.ValidateEmoji(tc.emoji) == nil != tc.ok {
			t.Errorf("emoji %q: model rule disagrees with ingress", tc.emoji)
		}
	}
}
