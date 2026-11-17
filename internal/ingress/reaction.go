package ingress

import (
	"maps"
	"slices"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Inbound reactions. A reaction envelope names its target by provider
// message ID and carries either:
//
//   - one emoji text part from a sender: an attributed reaction (reaction
//     added) or its removal (reaction removed), stored on the message
//     under the sender's identity; a removal may carry no emoji
//     (WhatsApp), naming the sender's single reaction; or
//   - from a system actor, one structured part holding a
//     model.ReactionSummary: the provider's per-emoji counts, for
//     providers that report no reacting identities and for refetched
//     copies. It replaces the message's count-only entries, leaves the
//     attributed ones, and never invents an identity.
//
// The channel adapter's reaction cardinality (adapter.Exchange.Reactions)
// applies as it does to an outbound reaction, by the same rule
// (model.Message.ReactionSteps). Under single cardinality a sender's new
// emoji replaces their earlier one, a removal removes it whatever its
// emoji, and a removal naming none removes it. Under multiple cardinality
// a sender holds one reaction per emoji, and a removal naming none needs
// the sender to hold exactly one.
//
// Every stored change moves the message revision and emits
// message.updated with the previous revision. Adding a present reaction,
// removing an absent one and repeating a snapshot write nothing.
// Message.ReactionsComplete is set only by an authoritative snapshot (a
// refetched copy); an event-derived snapshot that changes counts clears
// it.

// checkReaction validates the content of a reaction envelope.
func checkReaction(env model.Envelope) error {
	if env.SystemActor {
		_, err := reactionSnapshot(env)
		return err
	}
	if env.Kind == model.EnvelopeReactionRemoved && len(env.Content.Parts) == 0 {
		return nil
	}
	_, err := reactionEmoji(env)
	return err
}

// reactionEmoji returns the single text part of a reaction envelope,
// checked by the emoji rule outbound reactions follow (model.ValidateEmoji).
func reactionEmoji(env model.Envelope) (string, error) {
	ps := env.Content.Parts
	if len(ps) != 1 || ps[0].Kind != model.PartText || model.ValidateEmoji(ps[0].Text) != nil {
		return "", model.NewError(model.Validation, "envelope.content: a reaction carries one emoji text part")
	}
	return ps[0].Text, nil
}

// reactionSnapshot returns the reaction summary a system actor's reaction
// envelope carries.
func reactionSnapshot(env model.Envelope) (model.ReactionSummary, error) {
	ps := env.Content.Parts
	if env.Kind != model.EnvelopeReactionAdded || len(ps) != 1 || ps[0].Kind != model.PartStructured || ps[0].Structured == nil {
		return model.ReactionSummary{}, model.NewError(model.Validation, "envelope: a system actor's reaction carries one reaction summary")
	}
	return model.DecodeReactionSummary(*ps[0].Structured)
}

// at is when the envelope's event happened.
func (u *ingressUnit) at() time.Time {
	if u.env.OccurredAt.IsZero() {
		return u.now
	}
	return u.env.OccurredAt.UTC()
}

// applyReaction applies a reaction envelope to target. A deleted or
// redacted target takes none.
func (u *ingressUnit) applyReaction(target model.Message, sender *model.Identity) error {
	if target.Deleted || target.Redacted {
		return nil
	}
	if u.env.SystemActor {
		return u.applyReactionSnapshot(target)
	}
	if sender == nil {
		return model.NewError(model.Validation, "envelope: a reaction needs a sender")
	}
	by := sender.Ref
	remove := u.env.Kind == model.EnvelopeReactionRemoved
	single := u.s.desc.Exchange.Reactions == adapter.ReactionsSingle
	emoji := ""
	switch {
	case remove && len(u.env.Content.Parts) == 0 && single:
		// A removal naming no emoji removes the sender's only reaction.
	case remove && len(u.env.Content.Parts) == 0:
		var err error
		if emoji, err = singleReaction(target, by); err != nil || emoji == "" {
			return err
		}
	default:
		var err error
		if emoji, err = reactionEmoji(u.env); err != nil {
			return err
		}
	}
	entry := model.MessageReaction{Emoji: emoji, By: &by, Count: 1, At: u.at()}
	cur := target
	for _, step := range target.ReactionSteps(entry, remove, single) {
		next := cur.Clone()
		if !next.ApplyReactionStep(step) {
			continue
		}
		change := changeReactionAdded
		if step.Remove {
			change = changeReactionRemoved
		}
		prev, stored, err := u.put(next, false)
		if err != nil {
			return err
		}
		if err := u.messageUpdated(stored, change, prev, &model.PayloadReaction{Emoji: step.Reaction.Emoji, By: &by}); err != nil {
			return err
		}
		cur = stored
	}
	return nil
}

// singleReaction returns the emoji of by's only reaction on m: empty when
// by has none (nothing to remove), model.Validation when by has several
// and a removal naming no emoji cannot tell which.
func singleReaction(m model.Message, by model.IdentityRef) (string, error) {
	var emojis []string
	for _, r := range m.Reactions {
		if r.By != nil && *r.By == by {
			emojis = append(emojis, r.Emoji)
		}
	}
	switch len(emojis) {
	case 0:
		return "", nil
	case 1:
		return emojis[0], nil
	}
	return "", model.NewError(model.Validation, "envelope.content: a removal without an emoji needs a sender with one reaction")
}

// applyReactionSnapshot replaces target's count-only entries with a
// system actor's snapshot, announcing each emoji whose total moved.
func (u *ingressUnit) applyReactionSnapshot(target model.Message) error {
	snap, err := reactionSnapshot(u.env)
	if err != nil {
		return err
	}
	cur := target.Clone()
	old := countOnly(cur)
	next := map[string]int{}
	cur.Reactions = slices.DeleteFunc(cur.Reactions, func(r model.MessageReaction) bool { return !r.Attributed() })
	for _, c := range snap.Counts {
		if c.Count > 0 {
			next[c.Emoji] = c.Count
			cur.Reactions = append(cur.Reactions, model.MessageReaction{Emoji: c.Emoji, Count: c.Count, At: u.at()})
		}
	}
	same := maps.Equal(old, next)
	switch {
	case u.authoritative:
		cur.ReactionsComplete = snap.Complete
	case !same:
		cur.ReactionsComplete = false
	}
	if same && cur.ReactionsComplete == target.ReactionsComplete {
		return nil
	}
	prev, stored, err := u.put(cur, false)
	if err != nil {
		return err
	}
	moved := map[string]bool{}
	for _, m := range []map[string]int{old, next} {
		for e := range m {
			moved[e] = true
		}
	}
	emojis := slices.Sorted(maps.Keys(moved))
	for _, e := range emojis {
		before, after := emojiTotal(target, e), emojiTotal(stored, e)
		if before == after {
			continue
		}
		change := changeReactionAdded
		if after < before {
			change = changeReactionRemoved
		}
		if err := u.messageUpdated(stored, change, prev, &model.PayloadReaction{Emoji: e, Count: after}); err != nil {
			return err
		}
	}
	return nil
}

// countOnly returns m's count-only entries by emoji.
func countOnly(m model.Message) map[string]int {
	out := map[string]int{}
	for _, r := range m.Reactions {
		if !r.Attributed() {
			out[r.Emoji] = r.Count
		}
	}
	return out
}

// emojiTotal is the total of emoji on m: its count-only entry when it
// has one, else the number of attributed entries.
func emojiTotal(m model.Message, emoji string) int {
	n := 0
	for _, r := range m.Reactions {
		switch {
		case r.Emoji != emoji:
		case !r.Attributed():
			return r.Count
		default:
			n++
		}
	}
	return n
}
