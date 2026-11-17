package teams

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"hop.top/uma/model"
)

// reactionTypes are the reactions Teams names in a messageReaction
// activity ("The types of reactions include angry, heart, laugh, like,
// sad, and surprised",
// https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/conversations/subscribe-to-conversation-events)
// and the emoji each one shows. They are also the reactions the bot adds.
var reactionTypes = map[string]string{
	"like":      "👍",
	"heart":     "❤️",
	"laugh":     "😆",
	"surprised": "😮",
	"sad":       "😢",
	"angry":     "😡",
}

// reactionEmoji is the emoji of a reaction type: one of reactionTypes, or
// an ID that starts with the emoji's code points in hexadecimal, such as
// "1f440_eyes" or "2705_whiteheavycheckmark", the form the Teams SDK
// documents for any other reaction
// (https://learn.microsoft.com/en-us/microsoftteams/platform/teams-sdk/in-depth-guides/message-reactions).
// It reports false for a type it cannot read.
func reactionEmoji(kind string) (string, bool) {
	if e, ok := reactionTypes[kind]; ok {
		return e, true
	}
	var b strings.Builder
	for seg := range strings.SplitSeq(kind, "_") {
		var r rune
		if !codePoint.MatchString(seg) {
			break
		}
		if _, err := fmt.Sscanf(seg, "%x", &r); err != nil || !utf8.ValidRune(r) {
			return "", false
		}
		b.WriteRune(r)
	}
	e := b.String()
	if e == "" || model.ValidateEmoji(e) != nil {
		return "", false
	}
	return e, true
}

// codePoint is a code point in a reaction ID: four to six lowercase hex
// digits.
var codePoint = regexp.MustCompile(`^[0-9a-f]{4,6}$`)

// reactionType is the reaction type the bot adds for an emoji: only the
// six named types, whose IDs the Teams SDK documents; any other emoji has
// a type whose name part the adapter cannot derive.
func reactionType(emoji string) (string, bool) {
	for k, e := range reactionTypes {
		if e == emoji {
			return k, true
		}
	}
	return "", false
}
