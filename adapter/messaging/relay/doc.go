// Package relay is the generic JSON message relay adapter (type key
// relay.messages): SMS, WhatsApp and iMessage providers that have no adapter
// of their own, reached through a gateway the operator runs. One channel is
// one gateway and one medium (MediumSMS, MediumWhatsApp or MediumIMessage).
//
// The package is part of the root module and imports the standard library,
// the public uma contract packages and adapter/web/webhookauth only.
//
// # Reply route
//
// The gateway reaches uma by webhook and uma reaches the gateway by callback:
//
//   - Inbound: the gateway POSTs a JSON object to the handler the host
//     mounts (adapter.HTTPReceiver). webhookauth authenticates the request
//     from the channel's configuration before a byte of it is parsed. The
//     JSON keys are configuration (Fields: ordered aliases per role, neutral
//     defaults from, to, body, message_id, media), so a sender already in the
//     field keeps its names. The answer carries no reply.
//   - Outbound: Send POSTs each reply to one configured URL (Callback). The
//     host decides which hosts a channel may name (Options.CallbackHosts) and
//     the credential comes from the host's credential port, so a channel
//     never chooses where a secret is sent. Redirects are never followed, the
//     call and the answer read are bounded, and the operation travels as the
//     Idempotency-Key.
//   - Lookup, optional: Reconcile GETs a second URL the callback configures
//     (Callback.LookupURL) with the operation as the operation_id query
//     parameter, under the same host allowlist, URL rules, credential and
//     bounds as the callback. The gateway answers sent or not_found by the
//     Idempotency-Key it remembers.
//   - Delivery status, optional: a channel that sets Config.Inbound.Status
//     takes status updates on the same webhook, authenticated and
//     deduplicated like a message. A body holding the status key (accepted,
//     sent, delivered, read or failed, for an operation_id and attempt_id the
//     callback carried, or a message_id) is a status envelope whose delivery
//     report the engine applies to that attempt, as it does a Twilio or Meta
//     status callback. It settles a reply whose answer was lost and moves
//     one the gateway accepted on, up the order accepted and sent, failed,
//     delivered, read, and never down: read is delivered with the time the
//     engine records in Operation.ReadConfirmedAt. The webhook answers
//     {"status":"accepted","matched":false} for an update that reached no
//     attempt. See Instance.statusEnvelope.
//   - Start recipient: the Factory implements adapter.RecipientNormalizer, so
//     Conversations.Start gives an iMessage email recipient the lowercase
//     spelling inbound reports before the identity is claimed, the
//     conversation opened and the start fingerprinted. See
//     Factory.NormalizeRecipient. It also implements
//     adapter.LegacyRecipientReuser for iMessage: a start typed exactly as
//     an earlier start claimed the identity (before the lowercase rule)
//     reuses that identity and its conversation; see
//     Factory.ReuseLegacyRecipient.
//
// A reply written into the webhook answer is not offered: uma replies after
// the inbound message was accepted, from any process, and an answer cannot be
// looked up again, so it could be neither idempotent nor reconciled.
//
// # What is declared
//
// receive_http, send, reconcile and start_conversation, and the event
// delivery.updated besides message.received. Reconcile is available on a
// channel only when its callback names a lookup URL; without one it is
// reported disabled, and a send whose answer was lost stays outcome_unknown
// and is never sent again, unless the gateway reports it by a status update.
// A gateway that remembers Idempotency-Key values can still recognize a
// duplicate it is given by hand.
//
// receipt is not declared, and never reported: it is the adapter telling the
// sender of a message that it was delivered or read, and the relay contract
// has no such call. Status updates from the gateway are the other
// direction, a setting of receive_http.
//
// Message IDs are the gateway's (adapter.MessageIDsSender): core gives them
// no identity or echo meaning, and an inbound delivery is deduplicated by an
// event ID derived from the sender and the message ID.
package relay
