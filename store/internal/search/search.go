// Package search holds the message search rules both maintained stores
// apply, so a backend's index and its reads agree with every other
// backend's: which tokens a message is indexed under, which attachments
// it holds, and how an attachment record's media type is compared with
// AttachmentFilter.MediaTypes. A Postgres query reproduces MediaType and
// MatchMediaType in SQL; any change here changes that query too.
package search

import (
	"slices"
	"strings"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/internal/tombstone"
)

// Tokens returns the search tokens a stored message is indexed under: the
// tokens of its text parts, or none once it is redacted or deleted, so a
// tombstone never matches Filter.Text.
func Tokens(m model.Message) []string {
	if m.Redacted || m.Deleted {
		return nil
	}
	return m.Content.SearchTokens()
}

// Attachments returns the attachments a stored message holds in tenant:
// the distinct attachment parts naming that tenant's attachments, the
// rows of the holder index. AttachmentFilter.Has and MediaTypes judge
// these only.
func Attachments(tenant model.TenantID, m model.Message) []model.AttachmentRef {
	return slices.DeleteFunc(tombstone.Attachments(m), func(a model.AttachmentRef) bool {
		return a.Inbox.Tenant != tenant
	})
}

// MediaType returns rec's media type as MediaTypes patterns are compared
// with it: the text before the first ';', ASCII spaces and tabs trimmed,
// ASCII letters lowercased. A redacted record has none. The rule is
// byte-level so SQL reproduces it exactly: translate, btrim and
// split_part, never a locale-dependent lower.
func MediaType(rec store.AttachmentRecord) string {
	if rec.State == store.AttachmentRedacted {
		return ""
	}
	v, _, _ := strings.Cut(rec.Attachment.Metadata.MediaType, ";")
	v = strings.Trim(v, " \t")
	b := []byte(v)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// Patterns splits validated AttachmentFilter.MediaTypes into exact media
// types and the types of "type/*" patterns.
func Patterns(mediaTypes []string) (exact, types []string) {
	exact, types = []string{}, []string{}
	for _, p := range mediaTypes {
		if typ, ok := strings.CutSuffix(p, "/*"); ok {
			types = append(types, typ)
		} else {
			exact = append(exact, p)
		}
	}
	return exact, types
}

// MatchMediaType reports whether the normalized media type mt equals an
// exact pattern or starts with "type/" for a wildcard pattern's type. An
// empty mt never matches.
func MatchMediaType(mt string, exact, types []string) bool {
	if mt == "" {
		return false
	}
	if slices.Contains(exact, mt) {
		return true
	}
	return slices.ContainsFunc(types, func(typ string) bool { return strings.HasPrefix(mt, typ+"/") })
}
