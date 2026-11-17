// Package linear is the Linear issues adapter for uma, type key
// linear.issues: one channel per workspace credential (a personal API key or
// an OAuth access token), one work item per issue, one message per issue
// description and comment.
//
// Linear's API is GraphQL (https://linear.app/developers/graphql). The
// adapter writes its few operations by hand over the standard library and
// adds no GraphQL client; every field it queries is in Linear's published
// schema (https://github.com/linear/linear/blob/master/packages/sdk/src/schema.graphql).
//
// # Construction
//
// NewFactory returns the adapter.Factory a host registers in uma.Config. The
// package registers nothing at init and keeps no global state. Every
// provider call goes through the injected ports.HTTPClient (the engine's
// Config.HTTP, as adapter.Dependencies.HTTP); the adapter has no client of
// its own.
//
// # Capabilities
//
// The adapter declares six capabilities (Factory.Describe):
//
//	Capability     Linear                                       Needs
//	receive_http   Issue and Comment webhooks                   webhook signing secret
//	send           commentCreate                                Config.Replies, Config.AccountID
//	edit           commentUpdate                                Config.Replies, Config.AccountID
//	delete         commentDelete                                Config.Replies, Config.AccountID
//	reconcile      comment read by derived ID; message          Config.Replies
//	               mutations by reading the comment
//	refetch        issue or comment read, for same-revision     read access
//	               changes
//
// Not declared: react, history, work_item_act, extension_execute and
// internal_note (nothing in Linear's API prevents the first four; they are
// not built), stream, send_ephemeral and signal. Asking for one is
// model.Unsupported, never another capability.
//
// # Credentials
//
// The channel's credential reference is resolved through ports.Credentials on
// every provider call and every webhook request:
//
//   - CredentialPurposeAPI accepts ports.Secret (a personal API key, sent as
//     is in Authorization: Linear refuses a Bearer prefix on a personal key)
//     or ports.OAuth2 (sent as Authorization: Bearer <token>). A 401 or an
//     AUTHENTICATION_ERROR re-resolves once with Fresh set.
//   - CredentialPurposeWebhook accepts ports.Secret: the webhook's signing
//     secret.
//
// # Inbound
//
// Instance.Handler serves Linear's webhooks
// (https://linear.app/developers/webhooks). It verifies Linear-Signature,
// the hex HMAC-SHA256 of the raw body under the signing secret, in constant
// time before decoding anything, then refuses a delivery whose signed
// webhookTimestamp (UTC milliseconds) is more than TimestampWindow from the
// clock, as Linear recommends. The unsigned Linear-Timestamp header is
// never read.
//
// A delivery is identified for deduplication by a digest of its raw body,
// never by Linear-Delivery: the signature covers the body only, so a
// captured signed body could be replayed under a new delivery ID.
// Linear-Event is unsigned too and must agree with the signed type.
//
// Inboxes bind teams by key (the envelope destination) and use
// per-work-item selection. The work item reference is namespace Namespace,
// kind work_item, ID the issue UUID; it is also the reply route. Senders
// are Linear user UUIDs under Namespace, kind user; applications are bot
// senders.
//
//   - Issue create: the description is the first message, carrying the
//     issue state; an empty description reports the state only.
//   - Issue update: the state, and when updatedFrom names description, an
//     edit (or deletion, when emptied) of the description message.
//   - Issue remove: binding deleted.
//   - Comment create, update and remove on an issue: a message, its edit
//     and its deletion. An edit by Config.AccountID of a body ending in an
//     uma marker reports the marked attempt (model.EchoRef).
//
// Workflow state types triage, backlog, unstarted and started map to open;
// completed, canceled and duplicate to closed. The raw status is
// "<type>:<state name>". Labels are the field FieldLabels.
//
// # Outbound
//
// Replies are opt-in (Config.Replies). A reply is one commentCreate whose
// comment ID the adapter derives from the operation and attempt (a version
// 4 UUID: CommentCreateInput accepts the new comment's ID). A lost answer is
// reconciled by reading that comment: found is accepted, absent is failed
// not_found and safe to retry. The body also ends with a hidden marker (an
// HTML comment) so an echo that arrives before the send answer is
// recognised. Edits and deletes target comment:<uuid> messages; an issue
// description is rejected unsupported.
//
// # Provider answers
//
// Linear reports failures in a GraphQL errors array. Not found (the message
// says so) is rejected not_found, naming binding inaccessible for a reply;
// INVALID_INPUT and 400 are rejected; FORBIDDEN and 403 are rejected
// unauthorized; 401 and AUTHENTICATION_ERROR are unauthorized after one fresh
// credential; 429 and RATELIMITED, in any status, are rate_limited with
// Retry-After, else the exhausted quota's reset header, else a minute. A
// 5xx or a transport failure after a write was sent is outcome_unknown and
// reconciled before any retry; a failed dial is transient.
package linear
