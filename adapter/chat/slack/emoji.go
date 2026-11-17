package slack

// Slack names a reaction by an emoji name, never by the emoji itself:
// reactions.add and reactions.remove take "thumbsup", never "👍", and
// reaction_added and reaction_removed events report the same name
// (https://docs.slack.dev/reference/methods/reactions.add/,
// https://docs.slack.dev/reference/events/reaction_added/). uma's
// reactions are the emoji, byte for byte.
//
// Slack documents the name format and no list of names, so the adapter
// carries a short table of the common emoji under the shortcodes Slack
// uses for them. An emoji outside it cannot be sent, and a reaction by a
// name outside it, a workspace's custom emoji included, cannot be
// reported: the adapter drops it rather than invent an emoji. A name with
// a skin tone suffix ("::skin-tone-2") is a distinct emoji and likewise
// outside the table.
var reactionNames = []struct {
	emoji string
	// name is the name sent; aliases are also read, inbound.
	name    string
	aliases []string
}{
	{"👍", "thumbsup", []string{"+1"}},
	{"👎", "thumbsdown", []string{"-1"}},
	{"❤️", "heart", nil},
	{"🎉", "tada", nil},
	{"👀", "eyes", nil},
	{"✅", "white_check_mark", nil},
	{"❌", "x", nil},
	{"🔥", "fire", nil},
	{"🚀", "rocket", nil},
	{"💯", "100", nil},
	{"🙏", "pray", nil},
	{"👏", "clap", nil},
	{"🙌", "raised_hands", nil},
	{"😂", "joy", nil},
	{"😄", "smile", nil},
	{"😀", "grinning", nil},
	{"🤔", "thinking_face", nil},
	{"😕", "confused", nil},
	{"😭", "sob", nil},
	{"👌", "ok_hand", nil},
	{"👋", "wave", nil},
	{"💪", "muscle", nil},
	{"💡", "bulb", nil},
	{"⭐", "star", nil},
	{"✨", "sparkles", nil},
	{"🐛", "bug", nil},
}

// reactionName returns the Slack name of emoji.
func reactionName(emoji string) (string, bool) {
	for _, r := range reactionNames {
		if r.emoji == emoji {
			return r.name, true
		}
	}
	return "", false
}

// reactionEmoji returns the emoji a Slack reaction name stands for.
func reactionEmoji(name string) (string, bool) {
	for _, r := range reactionNames {
		if r.name == name {
			return r.emoji, true
		}
		for _, a := range r.aliases {
			if a == name {
				return r.emoji, true
			}
		}
	}
	return "", false
}
