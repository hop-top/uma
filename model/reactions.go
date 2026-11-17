package model

import (
	"bytes"
	"encoding/json"
	"slices"
	"time"
	"unicode"
	"unicode/utf8"
)

// maxEmojiBytes bounds a reaction emoji.
const maxEmojiBytes = 64

// MaxReactionsPerMessage bounds the attributed entries of
// Message.Reactions. A reaction beyond it is never refused: it folds into
// the emoji's count-only entry and the set stops being complete (see
// Message.AddReaction). Count-only entries do not count toward it.
const MaxReactionsPerMessage = 256

// ValidateEmoji applies the reaction emoji rule shared by outbound
// reactions and inbound reaction envelopes: one token of 1-64 bytes of
// valid UTF-8 without control characters or any Unicode space. Emoji
// compare byte-exact and are never normalized: a variation selector
// (U+FE0F) or a skin tone makes a distinct emoji.
func ValidateEmoji(s string) error { return checkEmoji("emoji", s) }

func checkEmoji(field, s string) error {
	switch {
	case s == "":
		return invalidField(field, "required")
	case len(s) > maxEmojiBytes:
		return invalidField(field, "exceeds 64 bytes")
	case !utf8.ValidString(s):
		return invalidField(field, "must be valid UTF-8")
	}
	for _, r := range s {
		switch {
		case unicode.IsControl(r):
			return invalidField(field, "must not contain control characters")
		case unicode.IsSpace(r):
			return invalidField(field, "must be a single token without spaces")
		}
	}
	return nil
}

// MessageReaction is one entry of a message's stored reaction state: what
// uma knows, never a fabricated identity.
//
// An attributed entry names who reacted, By (an inbound provider
// identity) or Actor (a uma caller), exactly one of them, with Count 1;
// its key is (Emoji, By) or (Emoji, Actor). A count-only entry has neither
// and Count at least 1: the provider's total for the emoji, of which the
// emoji's attributed entries are a subset; its key is Emoji. Readers take
// an emoji's total from its count-only entry when present, else from its
// attributed entries. A set holds each key at most once.
type MessageReaction struct {
	// Emoji follows ValidateEmoji.
	Emoji string `json:"emoji"`
	// By is the provider identity that reacted; nil for a uma caller's
	// reaction and for a count-only entry.
	By *IdentityRef `json:"by,omitempty"`
	// Actor is the uma caller whose React command added the reaction;
	// empty for an inbound reaction and for a count-only entry.
	Actor ActorID `json:"actor"`
	// Count is 1 for an attributed entry, the provider's total for the
	// emoji for a count-only entry.
	Count int `json:"count"`
	// External holds the provider's references of the reaction itself,
	// such as the provider message that carried it; never the target
	// message's references.
	External []ExternalRef `json:"external,omitempty"`
	// At is when the reaction was made or, for a count-only entry, last
	// counted; zero when unknown.
	At time.Time `json:"at"`
}

// Attributed reports whether r names who reacted.
func (r MessageReaction) Attributed() bool { return r.By != nil || r.Actor != "" }

// Validate checks the emoji, the attribution and count shape and the
// references.
func (r MessageReaction) Validate() error { return r.validate("") }

func (r MessageReaction) validate(path string) error {
	var who, count error
	switch {
	case r.By != nil && r.Actor != "":
		who = invalidField(fieldPath(path, "actor"), "must be empty when by is set")
	case r.By != nil:
		who = r.By.validate(fieldPath(path, "by"), false)
	case r.Actor != "":
		who = checkToken(fieldPath(path, "actor"), string(r.Actor))
	}
	switch {
	case r.Attributed() && r.Count != 1:
		count = invalidField(fieldPath(path, "count"), "must be 1 for an attributed reaction")
	case !r.Attributed() && r.Count < 1:
		count = invalidField(fieldPath(path, "count"), "must be at least 1")
	}
	return firstError(
		checkEmoji(fieldPath(path, "emoji"), r.Emoji),
		who, count,
		checkRefs(fieldPath(path, "external"), r.External),
	)
}

// Clone returns a deep copy.
func (r MessageReaction) Clone() MessageReaction {
	r.By = clonePtr(r.By, same)
	r.External = slices.Clone(r.External)
	return r
}

// sameKey reports whether r and o share their dedupe key.
func (r MessageReaction) sameKey(o MessageReaction) bool {
	if r.Emoji != o.Emoji || r.Attributed() != o.Attributed() {
		return false
	}
	switch {
	case r.By != nil || o.By != nil:
		return r.By != nil && o.By != nil && *r.By == *o.By
	default:
		return r.Actor == o.Actor
	}
}

// ValidateReactions checks a stored reaction set: every entry valid, each
// key at most once and at most MaxReactionsPerMessage attributed entries.
func ValidateReactions(rs []MessageReaction) error {
	attributed := 0
	for i, r := range rs {
		at := indexPath("reactions", i)
		if err := r.validate(at); err != nil {
			return err
		}
		for _, prev := range rs[:i] {
			if prev.sameKey(r) {
				return invalidField(at, "duplicates an earlier reaction")
			}
		}
		if r.Attributed() {
			attributed++
		}
	}
	if attributed > MaxReactionsPerMessage {
		return invalidField("reactions", "exceeds 256 attributed reactions")
	}
	return nil
}

// AddReaction records the attributed reaction r and reports whether m
// changed; a reaction whose key is present, or one that is not attributed,
// changes nothing. When the emoji has a count-only entry, its count grows
// with it. At MaxReactionsPerMessage attributed entries r is not stored
// by name: it folds into the emoji's count-only entry (created at the
// emoji's attributed count plus one) and ReactionsComplete turns false.
func (m *Message) AddReaction(r MessageReaction) bool {
	if !r.Attributed() {
		return false
	}
	attributed, emoji := 0, 0
	for _, cur := range m.Reactions {
		if cur.sameKey(r) {
			return false
		}
		if cur.Attributed() {
			attributed++
			if cur.Emoji == r.Emoji {
				emoji++
			}
		}
	}
	count := m.countEntry(r.Emoji)
	if attributed < MaxReactionsPerMessage {
		r = r.Clone()
		r.Count = 1
		m.Reactions = append(m.Reactions, r)
		if count != nil {
			count.Count++
		}
		return true
	}
	m.ReactionsComplete = false
	if count != nil {
		count.Count++
		return true
	}
	m.Reactions = append(m.Reactions, MessageReaction{Emoji: r.Emoji, Count: emoji + 1, At: r.At})
	return true
}

// RemoveReaction removes the attributed entry with r's key and reports
// whether m changed; an absent key, or an r that is not attributed,
// changes nothing. When the emoji has a count-only entry, its count drops
// with it, and the entry goes at zero.
func (m *Message) RemoveReaction(r MessageReaction) bool {
	if !r.Attributed() {
		return false
	}
	i := slices.IndexFunc(m.Reactions, r.sameKey)
	if i < 0 {
		return false
	}
	m.Reactions = slices.Delete(m.Reactions, i, i+1)
	if count := m.countEntry(r.Emoji); count != nil {
		count.Count--
		if count.Count <= 0 {
			m.Reactions = slices.DeleteFunc(m.Reactions, func(x MessageReaction) bool {
				return !x.Attributed() && x.Emoji == r.Emoji
			})
		}
	}
	return true
}

// sameReactor reports whether r and o are attributed to the same reactor:
// the same identity, or the same acting caller.
func (r MessageReaction) sameReactor(o MessageReaction) bool {
	switch {
	case !r.Attributed() || !o.Attributed():
		return false
	case r.By != nil || o.By != nil:
		return r.By != nil && o.By != nil && *r.By == *o.By
	default:
		return r.Actor == o.Actor
	}
}

// ReactionStep is one change to a message's attributed reactions: Reaction
// added, or removed when Remove is set.
type ReactionStep struct {
	Reaction MessageReaction
	Remove   bool
}

// ReactionSteps returns, in order, the changes that make r (added, or
// removed when remove is set) take effect on m for the reactor r names.
//
// Under multiple cardinality (single unset) that is r itself. Under single
// cardinality a reactor holds at most one reaction, so an add is preceded
// by the removal of each earlier reaction of the reactor with another
// emoji, and a removal removes the reactor's reactions whatever their
// emoji; r's own emoji is then only the one to add. A reactor with nothing
// to remove yields no step for a removal. Apply each step with
// ApplyReactionStep, on the message as the previous step left it, so every
// step is its own stored change. The steps read m, never change it.
func (m Message) ReactionSteps(r MessageReaction, remove, single bool) []ReactionStep {
	if !single {
		return []ReactionStep{{Reaction: r, Remove: remove}}
	}
	var steps []ReactionStep
	for _, x := range m.Reactions {
		if x.sameReactor(r) && (remove || x.Emoji != r.Emoji) {
			steps = append(steps, ReactionStep{Reaction: x.Clone(), Remove: true})
		}
	}
	if !remove {
		steps = append(steps, ReactionStep{Reaction: r})
	}
	return steps
}

// ApplyReactionStep applies s with AddReaction or RemoveReaction and
// reports whether m changed.
func (m *Message) ApplyReactionStep(s ReactionStep) bool {
	if s.Remove {
		return m.RemoveReaction(s.Reaction)
	}
	return m.AddReaction(s.Reaction)
}

// countEntry returns the count-only entry of emoji, nil when none.
func (m *Message) countEntry(emoji string) *MessageReaction {
	for i := range m.Reactions {
		if !m.Reactions[i].Attributed() && m.Reactions[i].Emoji == emoji {
			return &m.Reactions[i]
		}
	}
	return nil
}

// SchemaReactionSummary names the builtin structured part (version 1,
// a ReactionSummary) that carries a provider's per-emoji reaction counts:
// the one part of a reaction_added envelope from a system actor.
const SchemaReactionSummary = CoreNamespace + ".reaction.summary"

// ReactionSummary is a provider's full snapshot of a message's reaction
// counts. It replaces every count-only entry of the message: an emoji it
// lacks, or counts at zero, has none afterwards; attributed entries stay.
// Complete reports whether the provider vouches the snapshot is whole; it
// becomes Message.ReactionsComplete.
type ReactionSummary struct {
	// Counts holds one entry per emoji.
	Counts []ReactionCount `json:"counts,omitempty"`
	// Complete marks an authoritative, whole snapshot.
	Complete bool `json:"complete"`
}

// ReactionCount is the provider's total for one emoji.
type ReactionCount struct {
	// Emoji follows ValidateEmoji.
	Emoji string `json:"emoji"`
	// Count is the total; zero means none.
	Count int `json:"count"`
}

// Validate checks every count and that each emoji appears once.
func (s ReactionSummary) Validate() error { return s.validate("") }

func (s ReactionSummary) validate(path string) error {
	for i, c := range s.Counts {
		at := indexPath(fieldPath(path, "counts"), i)
		if err := checkEmoji(fieldPath(at, "emoji"), c.Emoji); err != nil {
			return err
		}
		if c.Count < 0 {
			return invalidField(fieldPath(at, "count"), "must not be negative")
		}
		for _, prev := range s.Counts[:i] {
			if prev.Emoji == c.Emoji {
				return invalidField(fieldPath(at, "emoji"), "duplicates an earlier count")
			}
		}
	}
	return nil
}

// DecodeReactionSummary reads a reaction summary part. A name other than
// SchemaReactionSummary is model.Unsupported, another version
// model.SchemaMismatch, and unknown or malformed data model.Validation.
func DecodeReactionSummary(x Extension) (ReactionSummary, error) {
	var s ReactionSummary
	switch {
	case x.Name != SchemaReactionSummary:
		return s, NewError(Unsupported, "name: unknown reaction summary schema")
	case x.Version != 1:
		return s, &Error{Code: SchemaMismatch, SafeMessage: "version: unsupported reaction summary version", Retry: NoRetry}
	}
	dec := json.NewDecoder(bytes.NewReader(x.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || dec.More() {
		return ReactionSummary{}, invalidField("data", "must be a reaction summary")
	}
	if err := s.validate("data"); err != nil {
		return ReactionSummary{}, err
	}
	return s, nil
}
