package model

import (
	"encoding/json"
	"slices"
	"strings"
)

// MaxReplyRecipients bounds the addresses a reply names in Cc and Bcc
// together.
const MaxReplyRecipients = 50

// ReplyRecipients addresses a reply beyond the conversation's stored reply
// route (Reply.Recipients). The route address stays the reply's first
// recipient; nothing here removes it.
//
//   - ReplyAll adds the recipients of the message the reply answers: the
//     adapter reads that message's To and Cc, drops the channel's own
//     mailbox and every address the reply already names, and sends to the
//     rest. It needs a parent message: a reply that answers none carries
//     no recipients to add.
//   - Cc and Bcc are explicit bare addresses added to the reply's Cc and
//     Bcc lists. A Bcc recipient never appears in the delivered headers.
//
// Cc and Bcc are normalized (NormalizeEmail) and deduplicated by the
// engine; an address in both lists keeps its Cc place only.
type ReplyRecipients struct {
	ReplyAll bool     `json:"reply_all"`
	Cc       []string `json:"cc,omitempty"`
	Bcc      []string `json:"bcc,omitempty"`
}

// IsZero reports whether r adds no recipient.
func (r ReplyRecipients) IsZero() bool { return !r.ReplyAll && len(r.Cc) == 0 && len(r.Bcc) == 0 }

// Clone returns a deep copy.
func (r ReplyRecipients) Clone() ReplyRecipients {
	r.Cc = slices.Clone(r.Cc)
	r.Bcc = slices.Clone(r.Bcc)
	return r
}

// Normalize returns r with every address normalized, each list
// deduplicated (without case in the domain) and an address of Cc removed
// from Bcc. Order is kept. The addresses must be bare RFC 5322 addresses
// and at most MaxReplyRecipients in all; anything else is Validation.
func (r ReplyRecipients) Normalize() (ReplyRecipients, error) {
	return r.normalize("recipients")
}

func (r ReplyRecipients) normalize(path string) (ReplyRecipients, error) {
	if len(r.Cc)+len(r.Bcc) > MaxReplyRecipients {
		return ReplyRecipients{}, invalidField(path, "too many cc and bcc addresses")
	}
	seen := map[string]bool{}
	list := func(field string, in []string) ([]string, error) {
		var out []string
		for _, a := range in {
			n, err := NormalizeEmail(a)
			if err != nil {
				return nil, invalidField(fieldPath(path, field), "must be bare addresses")
			}
			if key := strings.ToLower(n); !seen[key] {
				seen[key] = true
				out = append(out, n)
			}
		}
		return out, nil
	}
	cc, err := list("cc", r.Cc)
	if err != nil {
		return ReplyRecipients{}, err
	}
	bcc, err := list("bcc", r.Bcc)
	if err != nil {
		return ReplyRecipients{}, err
	}
	return ReplyRecipients{ReplyAll: r.ReplyAll, Cc: cc, Bcc: bcc}, nil
}

// Validate checks the addresses and their number.
func (r ReplyRecipients) Validate() error {
	_, err := r.normalize("recipients")
	return err
}

// Keys of CapabilityStatus.Limits for CapabilityReplyRecipients.
const (
	// RecipientLimitReplyAll reports that the channel replies to all.
	RecipientLimitReplyAll = "reply_all"
	// RecipientLimitCc reports that the channel sends explicit Cc.
	RecipientLimitCc = "cc"
	// RecipientLimitBcc reports that the channel sends explicit Bcc.
	RecipientLimitBcc = "bcc"
	// RecipientLimitMax is the most addresses one reply carries in To, Cc
	// and Bcc together, the route address included; 0 states no bound.
	RecipientLimitMax = "max_recipients"
)

// RecipientLimitsName is the Extension name of the limits
// CapabilityReplyRecipients reports in CapabilityStatus.Limits; its Data is
// a JSON object of RecipientLimits.
const RecipientLimitsName = "uma.recipients.limits"

// RecipientLimits are what a channel's reply recipients capability supports.
// A host reads them before offering reply-all or cc: a request using what a
// channel does not support is refused by its adapter as a final rejection.
type RecipientLimits struct {
	ReplyAll      bool `json:"reply_all"`
	Cc            bool `json:"cc"`
	Bcc           bool `json:"bcc"`
	MaxRecipients int  `json:"max_recipients"`
}

// Extension encodes l as the Limits of a capability status.
func (l RecipientLimits) Extension() Extension {
	data, _ := json.Marshal(l) // fixed shape of booleans and a number: cannot fail
	return Extension{Name: RecipientLimitsName, Version: 1, Data: data}
}

// ParseRecipientLimits reads the limits CapabilityReplyRecipients reported.
// A report that is not recipient limits, is unreadable or carries a negative
// bound is Validation: a host reads it fail closed.
func ParseRecipientLimits(e Extension) (RecipientLimits, error) {
	var l RecipientLimits
	if e.Name != RecipientLimitsName || e.Version != 1 {
		return l, NewError(Validation, "limits: not recipient limits")
	}
	if err := json.Unmarshal(e.Data, &l); err != nil {
		return RecipientLimits{}, NewError(Validation, "limits: unreadable")
	}
	if l.MaxRecipients < 0 {
		return RecipientLimits{}, invalidField(RecipientLimitMax, "must not be negative")
	}
	return l, nil
}
