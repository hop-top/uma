package telegram

import "strings"

// reactionEmoji is the emoji a bot may set as a reaction, exactly as the Bot
// API reference lists them for ReactionTypeEmoji: "Currently, it can be one
// of" these 73
// (https://core.telegram.org/bots/api#reactiontypeemoji). Several are
// written without the emoji variation selector (the red heart is U+2764
// alone); the list is byte-exact as Telegram prints it, and setMessageReaction
// takes them so. A chat may allow fewer (ChatFullInfo.available_reactions),
// which only Telegram's answer shows.
var reactionEmoji = []string{
	"❤", "👍", "👎", "🔥", "🥰", "👏", "😁", "🤔",
	"🤯", "😱", "🤬", "😢", "🎉", "🤩", "🤮", "💩",
	"🙏", "👌", "🕊", "🤡", "🥱", "🥴", "😍", "🐳",
	"❤‍🔥", "🌚", "🌭", "💯", "🤣", "⚡", "🍌", "🏆",
	"💔", "🤨", "😐", "🍓", "🍾", "💋", "🖕", "😈",
	"😴", "😭", "🤓", "👻", "👨‍💻", "👀", "🎃", "🙈",
	"😇", "😨", "🤝", "✍", "🤗", "🫡", "🎅", "🎄",
	"☃", "💅", "🤪", "🗿", "🆒", "💘", "🙉", "🦄",
	"😘", "💊", "🙊", "😎", "👾", "🤷‍♂", "🤷", "🤷‍♀",
	"😡",
}

const variationSelector = "\ufe0f"

// telegramEmoji returns the form of emoji Telegram lists: emoji itself when
// it is listed, else the listed emoji that differs only by variation
// selectors (a red heart with U+FE0F is the heart without it). The match is
// on those selectors alone; nothing else is normalized.
func telegramEmoji(emoji string) (string, bool) {
	bare := strings.ReplaceAll(emoji, variationSelector, "")
	for _, e := range reactionEmoji {
		if e == emoji {
			return e, true
		}
	}
	for _, e := range reactionEmoji {
		if strings.ReplaceAll(e, variationSelector, "") == bare {
			return e, true
		}
	}
	return "", false
}
