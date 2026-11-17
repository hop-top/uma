package privacy

import (
	"context"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// eraseLogs deletes the recipient log of an identity a cascade owns: the
// log addressed by the channel of the identity's inbox, the identity's
// namespace and its source ID (see store.RecipientLogKey). It runs in the
// cascade's unit, so the log goes with the link records and the name, or
// not at all. Every identity of a contact cascade is a live identity of the
// contact the cascade resolved (see cascadeScope), the ones Contacts.Merge
// moved onto it included.
//
// An identity whose inbox is gone, or whose coordinates cannot address a
// log, has none: a log is only ever appended under a stored channel and a
// valid key.
func eraseLogs(ctx context.Context, tx store.Tx, ref model.IdentityRef) error {
	id, err := tx.GetIdentity(ctx, ref)
	if err != nil {
		return err
	}
	inbox, err := tx.GetInbox(ctx, id.Ref.Inbox)
	switch {
	case model.IsCode(err, model.NotFound):
		return nil
	case err != nil:
		return err
	}
	key := store.RecipientLogKey{Channel: inbox.Channel, Namespace: id.Namespace, Recipient: id.SourceID}
	if key.Validate() != nil {
		return nil
	}
	_, err = tx.EraseRecipientLog(ctx, key)
	return err
}
