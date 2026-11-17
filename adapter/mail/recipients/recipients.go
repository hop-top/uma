// Package recipients resolves who a mail reply goes to: the route address,
// the recipients of the message it answers (reply to all) and the explicit
// Cc and Bcc a request names (model.ReplyRecipients). Every mail adapter
// that composes the recipient lists itself shares it, so reply to all means
// the same on each.
package recipients

import (
	"net/mail"
	"strings"

	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/model"
)

// Max is the most addresses one reply carries in To, Cc and Bcc together.
// It matches the bound on the addresses recorded per received message, so
// a reply to all of one never exceeds what was recorded.
const Max = 50

// Limits are what the mail adapters report for model.CapabilityReplyRecipients.
func Limits() model.RecipientLimits {
	return model.RecipientLimits{ReplyAll: true, Cc: true, Bcc: true, MaxRecipients: Max}
}

// Set is the resolved recipients of one reply: bare addresses, the route
// address first in To, none repeated across the lists (compared without
// case).
type Set struct {
	To, Cc, Bcc []string
}

// Addresses returns the mime addresses of bare ones.
func Addresses(list []string) []mime.Address {
	out := make([]mime.Address, 0, len(list))
	for _, a := range list {
		out = append(out, mime.Address{Address: a})
	}
	return out
}

// All is every address, in To, Cc, Bcc order: the envelope recipients.
func (s Set) All() []string {
	out := make([]string, 0, len(s.To)+len(s.Cc)+len(s.Bcc))
	out = append(out, s.To...)
	out = append(out, s.Cc...)
	return append(out, s.Bcc...)
}

// Plan resolves the recipients of a reply. route is the reply's route
// address and always the first To. When req asks to reply to all, the
// answered message's parentTo join To and parentCc join Cc, without
// the addresses in self (the mailbox's own) and without any address already
// named; a reply that answers no message passes none. req's Cc and Bcc are
// added as given, minus repeats. More than Max addresses, or an address
// that is not a bare one, is a final model.Validation error.
func Plan(req *model.ReplyRecipients, route string, self, parentTo, parentCc []string) (Set, error) {
	seen := map[string]bool{}
	add := func(dst *[]string, addrs []string, skipSelf bool) {
		for _, a := range addrs {
			key := strings.ToLower(a)
			if seen[key] || (skipSelf && contains(self, key)) {
				continue
			}
			seen[key] = true
			*dst = append(*dst, a)
		}
	}
	var s Set
	add(&s.To, []string{route}, false)
	if req != nil {
		norm, err := req.Normalize()
		if err != nil {
			return Set{}, err
		}
		if norm.ReplyAll {
			add(&s.To, parentTo, true)
			add(&s.Cc, parentCc, true)
		}
		add(&s.Cc, norm.Cc, false)
		add(&s.Bcc, norm.Bcc, false)
	}
	if n := len(s.To) + len(s.Cc) + len(s.Bcc); n > Max {
		return Set{}, &model.Error{Code: model.Validation, SafeMessage: "reply has too many recipients", Retry: model.NoRetry}
	}
	return s, nil
}

func contains(self []string, key string) bool {
	for _, a := range self {
		if strings.ToLower(a) == key {
			return true
		}
	}
	return false
}

// ParseList reads the bare addresses of an address-list header value. A
// value that is not a valid list yields none: the recipients of a message
// whose header cannot be read are not guessed at.
func ParseList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	list, err := mail.ParseAddressList(v)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Address)
	}
	return out
}
