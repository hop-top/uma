package smtp

import (
	"encoding/json"
	"slices"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/recipients"
	"hop.top/uma/model"
)

// maxThreadReferences bounds the References header of a reply: the
// newest msg-ids are kept.
const maxThreadReferences = 20

// Thread is how an outgoing reply threads: its In-Reply-To msg-id, its
// References msg-ids and its subject.
type Thread struct {
	InReplyTo  string
	References []string
	Subject    string
}

// ThreadOf derives the threading of a send on channel ch from the message
// it answers. The parent msg-id is the request's target, a provider
// message of ch whose ID is a msg-id, else the email message_id among
// SendRequest.Thread's references. References are the parent's own
// References msg-ids followed by the parent. The subject is "Re: " and the
// parent's subject, read from its payloadSchema metadata (a message record
// with headers.subject), unless it already starts with "Re:"; fallback when
// the parent has no single-line subject.
func ThreadOf(ch model.ChannelRef, req adapter.SendRequest, payloadSchema, fallback string) Thread {
	out := Thread{Subject: fallback}
	if t := req.Target; t != nil && t.Channel == ch && t.Namespace == model.ProviderMessageNamespace && t.Kind == model.RefKindMessage && isMsgID(t.ID) {
		out.InReplyTo = t.ID
	}
	th := req.Thread
	if th == nil {
		if out.InReplyTo != "" {
			out.References = []string{out.InReplyTo}
		}
		return out
	}
	for _, r := range th.References {
		if r.Channel != ch || r.Namespace != model.EmailHeaderNamespace || !isMsgID(r.ID) {
			continue
		}
		switch r.Kind {
		case model.RefKindEmailMessageID:
			if out.InReplyTo == "" {
				out.InReplyTo = r.ID
			}
		case model.RefKindEmailReferences:
			if !slices.Contains(out.References, r.ID) {
				out.References = append(out.References, r.ID)
			}
		}
	}
	if out.InReplyTo != "" {
		out.References = append(slices.DeleteFunc(out.References, func(id string) bool { return id == out.InReplyTo }), out.InReplyTo)
	}
	if n := len(out.References); n > maxThreadReferences {
		out.References = out.References[n-maxThreadReferences:]
	}
	if s := parentSubject(th.Metadata, payloadSchema); s != "" {
		if !strings.HasPrefix(strings.ToLower(s), "re:") {
			s = "Re: " + s
		}
		out.Subject = s
	}
	return out
}

// parentSubject reads headers.subject from the metadata entry named
// schema; empty when absent or not a single line.
func parentSubject(meta []model.Extension, schema string) string {
	for _, m := range meta {
		if m.Name != schema {
			continue
		}
		var rec struct {
			Headers struct {
				Subject string `json:"subject"`
			} `json:"headers"`
		}
		if json.Unmarshal(m.Data, &rec) != nil {
			return ""
		}
		s := strings.TrimSpace(rec.Headers.Subject)
		if strings.ContainsAny(s, "\r\n") {
			return ""
		}
		return s
	}
	return ""
}

// Recipients resolves the recipients of a send on this channel: the route
// address, and for a request that replies to all, the To and Cc of the
// message it answers, read from its payloadSchema metadata (a message record
// with headers.to and headers.cc) minus the addresses in self, the mailbox's
// own; see recipients.Plan. A send that names no recipients goes to the route
// alone.
func Recipients(req adapter.SendRequest, route string, self []string, payloadSchema string) (recipients.Set, error) {
	var to, cc []string
	if req.Recipients != nil && req.Recipients.ReplyAll && req.Thread != nil {
		to, cc = parentRecipients(req.Thread.Metadata, payloadSchema)
	}
	return recipients.Plan(req.Recipients, route, self, to, cc)
}

// parentRecipients reads headers.to and headers.cc from the metadata entry
// named schema; none when absent or unreadable.
func parentRecipients(meta []model.Extension, schema string) (to, cc []string) {
	for _, m := range meta {
		if m.Name != schema {
			continue
		}
		var rec struct {
			Headers struct {
				To []mime.Address `json:"to"`
				Cc []mime.Address `json:"cc"`
			} `json:"headers"`
		}
		if json.Unmarshal(m.Data, &rec) != nil {
			return nil, nil
		}
		for _, a := range rec.Headers.To {
			to = append(to, a.Address)
		}
		for _, a := range rec.Headers.Cc {
			cc = append(cc, a.Address)
		}
		return to, cc
	}
	return nil, nil
}

func isMsgID(s string) bool {
	ids := model.ParseMsgIDs(s)
	return len(ids) == 1 && ids[0] == s
}
