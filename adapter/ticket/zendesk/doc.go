// Package zendesk is the Zendesk Support tickets adapter for uma (type key
// zendesk.tickets). It talks to the Support API over the host's injected
// ports.HTTPClient with the standard library only.
//
// # Work items
//
// A ticket is a work item: bind the channel to a per-work-item inbox
// (model.ModePerWorkItem). Each ticket's conversation is keyed by its work
// item reference (Namespace, model.RefKindWorkItem, the ticket ID), which
// is also the route of every outbound effect. Public comments are public
// messages; internal notes, and comments whose visibility Zendesk does not
// state, are restricted messages and never public. Ticket changes report
// the ticket's state (see StatusMapping); merges bind the ticket merged
// into its target, deletions and spam marks deleted. Envelope destinations
// are brand IDs.
//
// # Capabilities
//
//   - receive_http: Instance.Handler serves a Zendesk webhook subscribed to
//     ticket events. Every request is verified first: the
//     X-Zendesk-Webhook-Signature header must equal
//     base64(HMAC-SHA256(secret, timestamp + body)) in constant time, and
//     the X-Zendesk-Webhook-Signature-Timestamp must be within the
//     channel's webhook_tolerance_seconds of now.
//   - receive: Instance.Run polls the incremental ticket event export, so
//     comments a webhook missed still arrive.
//   - send: a public reply is a public comment.
//   - internal_note: in an inbox that opted in (model.Inbox.DeliverNotes),
//     an internal note added through Messages.AddInternalNote is a private
//     comment; elsewhere notes stay in uma.
//   - extension_execute: ExtensionInternalNote posts an internal note
//     (authorized as model.ActionMessageNote, never under a public
//     audience); ExtensionUpdate updates ticket fields (authorized as
//     model.ActionWorkItemAct). The factory refuses invalid fields at the
//     call (adapter.ActionValidator), with nothing stored; Execute applies
//     the same rules again at the attempt.
//   - reconcile: reads the ticket's audits for the attempt's marker.
//
// # Credentials
//
// One credential reference per channel, resolved through ports.Credentials
// per call and purpose:
//
//   - CredentialPurposeAPI, with configuration auth "api_token": a
//     ports.Secret API token (with configuration email) or a
//     ports.Password (agent address, API token), sent as HTTP Basic
//     "{email}/token:{token}"; with auth "oauth": a ports.OAuth2 access
//     token, or a ports.Secret holding one, sent as a Bearer token. The
//     host owns OAuth consent and refresh.
//   - CredentialPurposeWebhook: the webhook's signing secret, a
//     ports.Secret.
//
// # Echoes
//
// Every update carries the effect's operation, attempt, kind and audience
// in the audit's custom metadata. A reply's or delivered note's echo is
// reported with model.EchoRef; an extension effect's echo is dropped. See Instance.Send
// for the outcome mapping: an update Zendesk may have applied is never
// retried before reconciliation.
package zendesk
