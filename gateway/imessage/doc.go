// Package imessage is a reference gateway for the relay.messages
// adapter on the imessage medium: the operator's service that docs/adapters/
// relay.messages.md describes, in front of an iMessage backend.
//
// uma's side:
//
//   - POST PathReply takes the relay's callback (medium, to, body,
//     in_reply_to, operation_id, attempt_id) with its Idempotency-Key,
//     authenticated by the channel's relay.callback secret. The key is
//     recorded, synced to disk, before the backend is called; a callback
//     with a key already held is answered from that record and never sent
//     twice. A sent message is answered {"message_id": "<backend id>"}.
//   - GET PathLookup?operation_id=<key> answers {"status":"sent"} with the
//     message id, {"status":"not_found"} (404) only for a key the gateway
//     holds no record of, and {"status":"pending"} or "rejected" otherwise.
//
// The backend's side: its webhook is verified (Blooio's signature, or a
// secret path token for BlueBubbles, which signs nothing), each inbound
// one-to-one message is mapped to the relay's JSON (from, to, body,
// message_id, media) and posted to uma's webhook URL, signed with the
// channel's relay.webhook secret (HMAC-SHA-256 over "<timestamp>.<body>").
// The account's own outbound echoes, delivery updates, reactions and group
// messages are dropped.
//
// Backends: Blooio (hosted, REST v4) and BlueBubbles Server (on a Mac the
// operator runs, AppleScript sends, no Private API). Each talks HTTP through
// the *http.Client it is given.
//
// uma names no supported iMessage provider; this is a starting point. The
// package imports the standard library only. The command is
// cmd/imessagegateway.
package imessage
