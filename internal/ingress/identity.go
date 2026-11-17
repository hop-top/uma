package ingress

import (
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Message identity. The event claim (dedupKey) recognizes a redelivered
// event; it cannot recognize one provider message reported under two
// event IDs, as a webhook and a poll report it. A provider-assigned
// message ID is the message's identity in its inbox: a new message claims
// it, and a later report of it is that message again, applied as its
// update (no conversation selection, no new message, no message.received).
// A sender-chosen ID, such as an RFC 5322 Message-ID, is never claimed:
// anyone can repeat it, so two messages may share one.

// claimsIdentity reports whether the envelope's provider message ID is a
// message identity: provider-assigned and present.
func (u *ingressUnit) claimsIdentity() bool {
	return u.env.MessageID != "" && engine.ProviderAssigned(u.s.desc)
}

// knownMessage applies a new-message envelope naming a provider message
// the inbox already holds as that message's update, and reports whether
// it did. The same content in an audience that does not narrow is a
// no-op.
func (u *ingressUnit) knownMessage() (bool, error) {
	if !u.claimsIdentity() {
		return false, nil
	}
	msgs, err := u.tx.MessagesByReference(u.ctx, u.inbox.Ref, model.ProviderMessageRef(u.s.channel.Ref, u.env.MessageID), matchLimit)
	if err != nil || len(msgs) == 0 {
		return false, err
	}
	if len(msgs) == 1 && sameContent(msgs[0].Content, u.env.Content) && !narrows(msgs[0].Audience, u.env.Audience) {
		return true, nil
	}
	return true, u.applyUpdate()
}

// putReceived stores a new inbound message, claiming its provider message
// ID when that is its identity. A claim another unit committed first, past
// knownMessage, is model.Transient: the rerun unit finds the message.
func (u *ingressUnit) putReceived(msg model.Message) error {
	if !u.claimsIdentity() {
		return u.tx.PutMessage(u.ctx, msg, "")
	}
	_, created, err := u.tx.ClaimMessage(u.ctx, store.MessageClaim{
		Reference: model.ProviderMessageRef(u.s.channel.Ref, u.env.MessageID), Message: msg,
	})
	if err == nil && !created {
		return model.NewError(model.Transient, "provider message claimed by a concurrent unit")
	}
	return err
}
