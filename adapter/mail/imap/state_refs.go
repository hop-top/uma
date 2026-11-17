package imap

import (
	"net/url"
	"strconv"
	"strings"

	"hop.top/uma/model"
)

// A provider message reference of this adapter is one of two forms, both
// model.ProviderMessageRef of the channel:
//
//   - a Message-ID, the form every received and sent message starts with.
//     It names the message, not a place: the adapter looks it up in the
//     mailbox it retrieves and in the sent-copies mailbox, never anywhere
//     else, so after a move it no longer finds the message.
//   - a location, the form a move reports for the copy it made, so the next
//     state change finds the message where it is now. It carries the
//     mailbox, the UIDVALIDITY and the UID (RFC 5092 spelling) and the
//     Message-ID, which a reconcile needs to look the message up in a
//     destination: imap:///<mailbox>;UIDVALIDITY=<n>/;UID=<n>;MID=<id>,
//     each name percent-encoded.
const locPrefix = "imap:///"

// loc is a message at a place in the account.
type loc struct {
	mailbox     string
	uidValidity uint32
	uid         uint32
	messageID   string
}

// locationID is the reference ID of l.
func locationID(l loc) string {
	return locPrefix + url.PathEscape(l.mailbox) +
		";UIDVALIDITY=" + strconv.FormatUint(uint64(l.uidValidity), 10) +
		"/;UID=" + strconv.FormatUint(uint64(l.uid), 10) +
		";MID=" + url.PathEscape(l.messageID)
}

// parseLocation reads a location reference ID; ok is false for any other.
func parseLocation(id string) (l loc, ok bool) {
	rest, ok := strings.CutPrefix(id, locPrefix)
	if !ok {
		return loc{}, false
	}
	box, rest, ok := strings.Cut(rest, ";UIDVALIDITY=")
	if !ok {
		return loc{}, false
	}
	uv, rest, ok := strings.Cut(rest, "/;UID=")
	if !ok {
		return loc{}, false
	}
	uid, mid, ok := strings.Cut(rest, ";MID=")
	if !ok {
		return loc{}, false
	}
	var err error
	if l.mailbox, err = url.PathUnescape(box); err != nil || l.mailbox == "" {
		return loc{}, false
	}
	if l.messageID, err = url.PathUnescape(mid); err != nil || l.messageID == "" {
		return loc{}, false
	}
	v, err := strconv.ParseUint(uv, 10, 32)
	if err != nil || v == 0 {
		return loc{}, false
	}
	u, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || u == 0 {
		return loc{}, false
	}
	l.uidValidity, l.uid = uint32(v), uint32(u)
	return l, true
}

// locationRef is the provider message reference of l on the channel.
func (i *Instance) locationRef(l loc) model.ExternalRef {
	return model.ProviderMessageRef(i.ch.Ref, locationID(l))
}

// isMessageRef reports whether r is a provider message reference of this
// channel, the only kind a state request names.
func (i *Instance) isMessageRef(r model.ExternalRef) bool {
	return r.Channel == i.ch.Ref && r.Namespace == model.ProviderMessageNamespace && r.Kind == model.RefKindMessage && r.ID != ""
}
