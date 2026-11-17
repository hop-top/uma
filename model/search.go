package model

import (
	"mime"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Message search.
//
// Filter.Text, Filter.Since, Filter.Before and Filter.Attachments narrow
// a message list. Messages.List is the only query that may honour them; a
// query or store that does not evaluate a set field rejects it with
// Validation, never ignoring it. Every field narrows: a message matches
// when it satisfies all set fields together with the other filter fields.
//
// Text matches text parts (Part kind "text") only. Structured parts,
// Message.Metadata (where mail adapters keep the subject), attachment
// names and work item subjects are never searched. Text and message are
// split into tokens by SearchTokens; a message matches when every query
// token equals one of its tokens: AND, order-free, whole tokens. There is
// no phrase, prefix or stemmed match, no stop word and no ranking.
//
// Since and Before bound Message.OccurredAt, the provider or sender time
// (for mail, the Date header), as the half-open range [Since, Before),
// exact to the nanosecond. Adjacent windows never overlap.
//
// Attachments selects by attachment parts: AttachmentFilter.Has requires
// at least one attachment part (true) or none (false); MediaTypes
// requires an attachment whose record's media type, parameters stripped,
// matches one of the patterns. An attachment whose record is missing or
// redacted never matches. Matching a media type also requires
// ActionAttachmentRead on a matching attachment, so a list never confirms
// the media type of an attachment the caller cannot read.
//
// Search changes neither order nor admission: results come in recording
// order like every list, pages carry no total, and every message is
// authorized as for any other list; a message the caller may not read is
// skipped without trace. A redacted message keeps no searchable text and
// no attachments, so it never matches Text, MediaTypes or Has true.
//
// Limits: Text holds at most MaxSearchTextBytes bytes and 1 to
// MaxSearchTokens distinct tokens, none longer than MaxSearchTokenBytes;
// MediaTypes lists at most MaxSearchMediaTypes patterns.
const (
	// MaxSearchTextBytes bounds Filter.Text.
	MaxSearchTextBytes = 256
	// MaxSearchTokens bounds the distinct tokens of Filter.Text.
	MaxSearchTokens = 16
	// MaxSearchTokenBytes bounds one folded token. A longer message token
	// is not indexed, so it never matches; a longer query token is
	// Validation.
	MaxSearchTokenBytes = 64
	// MaxSearchMediaTypes bounds AttachmentFilter.MediaTypes.
	MaxSearchMediaTypes = 16
)

// AttachmentFilter selects messages by their attachment parts. At least
// one field is set; MediaTypes with Has false selects nothing and is
// Validation.
type AttachmentFilter struct {
	// Has, when set, requires at least one attachment part (true) or no
	// attachment part (false).
	Has *bool `json:"has,omitempty"`
	// MediaTypes requires an attachment part whose attachment record's
	// media type, parameters stripped, matches one pattern: lowercase
	// "type/subtype" exactly, or "type/*" for any subtype of type. At most
	// MaxSearchMediaTypes distinct patterns.
	MediaTypes []string `json:"media_types,omitempty"`
}

// Clone returns a deep copy.
func (a AttachmentFilter) Clone() AttachmentFilter {
	a.Has = clonePtr(a.Has, same)
	a.MediaTypes = slices.Clone(a.MediaTypes)
	return a
}

// SearchTokens returns the search tokens of texts: the one tokenizer that
// both stores index message text with and that queries match against.
//
// A token is a maximal run of Unicode letters, marks and decimal digits
// within one text; every other rune, invalid UTF-8 included, separates
// tokens, so "foo@bar.com" is three tokens and "v2.1" two. Tokens are
// case-folded by simple case folding: each rune becomes the smallest rune
// of its unicode.SimpleFold orbit, so "Été", "ÉTÉ" and "été" fold alike
// (the folded form is an index key, not display text). There is no
// Unicode normalization: a precomposed and a decomposed spelling of the
// same text are different tokens. Scripts written without spaces, such as
// Chinese or Japanese, tokenize as whole runs. A folded token longer than
// MaxSearchTokenBytes is left out.
//
// The result holds each token once, in first-occurrence order; it is nil
// when texts hold no token.
func SearchTokens(texts ...string) []string {
	var out []string
	for _, s := range texts {
		eachSearchToken(s, func(tok string) {
			if len(tok) <= MaxSearchTokenBytes && !slices.Contains(out, tok) {
				out = append(out, tok)
			}
		})
	}
	return out
}

// SearchTokens returns the search tokens of c's text parts; structured
// and attachment parts are never searched.
func (c Content) SearchTokens() []string {
	texts := make([]string, 0, len(c.Parts))
	for _, p := range c.Parts {
		if p.Kind == textPartKind {
			texts = append(texts, p.Text)
		}
	}
	return SearchTokens(texts...)
}

// eachSearchToken calls fn with every folded token of s, in text order.
func eachSearchToken(s string, fn func(string)) {
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			fn(b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r) {
			b.WriteRune(foldRune(r))
			continue
		}
		flush()
	}
	flush()
}

// foldRune returns the smallest rune of r's simple case folding orbit.
func foldRune(r rune) rune {
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		least = min(least, f)
	}
	return least
}

// validateSearch checks the message search fields of f.
func (f Filter) validateSearch(path string) error {
	if f.Text != "" {
		if err := checkSearchText(fieldPath(path, "text"), f.Text); err != nil {
			return err
		}
	}
	if f.Since != nil && f.Since.IsZero() {
		return invalidField(fieldPath(path, "since"), "must be a non-zero time")
	}
	if f.Before != nil && f.Before.IsZero() {
		return invalidField(fieldPath(path, "before"), "must be a non-zero time")
	}
	if f.Since != nil && f.Before != nil && !f.Since.Before(*f.Before) {
		return invalidField(fieldPath(path, "before"), "must be after since")
	}
	if f.Attachments != nil {
		return f.Attachments.validate(fieldPath(path, "attachments"))
	}
	return nil
}

func checkSearchText(field, s string) error {
	switch {
	case len(s) > MaxSearchTextBytes:
		return invalidField(field, "exceeds "+strconv.Itoa(MaxSearchTextBytes)+" bytes")
	case !utf8.ValidString(s):
		return invalidField(field, "must be valid UTF-8")
	}
	long := false
	eachSearchToken(s, func(tok string) { long = long || len(tok) > MaxSearchTokenBytes })
	n := len(SearchTokens(s))
	switch {
	case long:
		return invalidField(field, "a token exceeds "+strconv.Itoa(MaxSearchTokenBytes)+" bytes")
	case n == 0:
		return invalidField(field, "must hold at least one letter or digit")
	case n > MaxSearchTokens:
		return invalidField(field, "exceeds "+strconv.Itoa(MaxSearchTokens)+" distinct tokens")
	}
	return nil
}

func (a AttachmentFilter) validate(path string) error {
	types := fieldPath(path, "media_types")
	switch {
	case a.Has == nil && len(a.MediaTypes) == 0:
		return invalidField(path, "needs has or media_types")
	case len(a.MediaTypes) > 0 && a.Has != nil && !*a.Has:
		return invalidField(types, "requires has unset or true")
	case len(a.MediaTypes) > MaxSearchMediaTypes:
		return invalidField(types, "exceeds "+strconv.Itoa(MaxSearchMediaTypes)+" entries")
	}
	for i, v := range a.MediaTypes {
		at := types + "[" + strconv.Itoa(i) + "]"
		if !mediaTypePattern(v) {
			return invalidField(at, `must be lowercase "type/subtype" or "type/*" without parameters`)
		}
		if slices.Contains(a.MediaTypes[:i], v) {
			return invalidField(at, "duplicate")
		}
	}
	return nil
}

// mediaTypePattern reports whether v is a lowercase "type/subtype" or
// "type/*" without parameters or surrounding space.
func mediaTypePattern(v string) bool {
	if len(v) > maxTokenBytes {
		return false
	}
	mt, params, err := mime.ParseMediaType(v)
	typ, sub, ok := strings.Cut(mt, "/")
	return err == nil && len(params) == 0 && mt == v && ok &&
		typ != "" && !strings.Contains(typ, "*") &&
		sub != "" && (sub == "*" || !strings.Contains(sub, "*"))
}
