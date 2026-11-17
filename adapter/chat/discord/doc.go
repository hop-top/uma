// Package discord is the Discord adapter for uma (type key discord.chat).
// It talks to Discord's REST API over the host's injected ports.HTTPClient
// with the standard library, and to the gateway's WebSocket with
// github.com/coder/websocket, which has no dependencies of its own. One
// channel is one bot application; the docs/adapters/discord.chat.md page of
// the repository is the reference.
//
// # Capabilities
//
//   - receive: Instance.Run is the gateway receiver (API v10, JSON). It
//     identifies with the guild and direct message intents and their
//     reactions, and MESSAGE_CONTENT only when Config.MentionsOnly is off,
//     heartbeats, resumes after a drop with resume_gateway_url, session_id
//     and the last sequence number, and keeps the session and the handled
//     sequence number as the receiver cursor, so a restart resumes too.
//     Close 4014 (a privileged intent not granted) ends it disabled and is
//     never retried. Messages, edits, deletions, reactions and interactions
//     are recorded in per-channel lanes. The instance implements
//     adapter.ReceiverReporter: connected at READY or RESUMED, not
//     connected when a connection ends.
//   - receive_http: Instance.Handler is the Interactions Endpoint URL. It
//     verifies the Ed25519 signature over X-Signature-Timestamp and the
//     body before anything else, answers PING, records commands, components
//     and modal submissions and acknowledges them with a deferral inside
//     Discord's three seconds. Plain messages never arrive there.
//   - send, start_conversation, edit, delete, react, signal, membership:
//     the REST routes of messages, reactions, the typing indicator and
//     thread members, with a nonce on every message for reconciliation.
//   - history, refetch, reconcile: reads of channel messages.
//   - respond, respond_follow_up, send_ephemeral: the interaction's
//     webhook, with the application ID and the interaction token.
//
// Config.Receive picks one way of receiving or both: gateway withholds
// receive_http and interactions withholds receive, each reported
// unavailable with reason model.Disabled; empty offers both.
//
// A message is named by "<channel ID>:<message ID>". Identities are users,
// bots and webhooks in namespace discord.chat.user; rooms (RefKindRoom, a
// channel) and threads (RefKindThread, a thread channel under its parent)
// scope conversations for per-room and per-thread selection, and a user's
// route for per-contact selection.
//
// Credentials come from ports.Credentials under the channel's credential
// reference, one purpose each, always as a secret: CredentialPurposeAPI
// (the bot token) and CredentialPurposeInteractions (the application's
// public key, hex).
//
// REST calls go through a module-local rate limiter: Discord's global 50
// requests a second and the per-route buckets its X-RateLimit headers
// describe, with 429s and the invalid-request ceiling respected.
package discord
