// Package jira is the Jira Cloud issues adapter for uma, type key
// jira.issues: one channel per Jira Cloud site, one work item per issue, one
// message per issue description and comment.
//
// # Construction
//
// NewFactory returns the adapter.Factory a host registers in uma.Config.
// The package registers nothing at init and keeps no global state. Every
// provider call goes through the injected ports.HTTPClient (the engine's
// Config.HTTP, as adapter.Dependencies.HTTP); WithHTTPClient supplies one
// when the host injects none, and WithBaseURL points the adapter at a proxy
// or test server.
//
// # Capabilities
//
// The adapter declares six capabilities (Factory.Describe). What each one
// does on Jira, and what it needs:
//
//	Capability   Jira                                           Needs
//	receive_http admin webhooks: jira:issue_* and comment_*     webhook secret
//	send         add comment (POST .../issue/{id}/comment)      Config.Replies, Config.AccountID
//	edit         update comment (PUT .../comment/{id})          Config.Replies, Config.AccountID
//	delete       delete comment (DELETE .../comment/{id})       Config.Replies, Config.AccountID
//	reconcile    comments by echo property; comment reads      Config.Replies
//	refetch      issue or comment read, for same-revision       read access to issues
//	             changes
//
// Not declared, and why, is on the adapter's documentation page: react and
// receipt (Jira has none), internal_note (a Jira comment restriction needs
// a role or group of the site), work_item_act (a transition is a
// workflow-specific ID, resolved per issue), history, receive and the rest.
// Asking for one is model.Unsupported, never another capability.
//
// # Credentials
//
// The channel's credential reference is resolved through ports.Credentials
// on every provider call and every webhook request:
//
//   - CredentialPurposeAPI accepts ports.Password (the Atlassian account
//     email and an API token: basic authentication), ports.Secret (an API
//     token, with Config.Email) and ports.OAuth2 (an OAuth 2.0 (3LO) access
//     token, sent as a bearer token, with Config.CloudID: the request then
//     goes through the API gateway,
//     https://api.atlassian.com/ex/jira/{cloudid}). A 401 re-resolves once
//     with Fresh set.
//   - CredentialPurposeWebhook accepts ports.Secret: the secret of the
//     admin webhook.
//
// The account needs the Browse projects, Add comments, and (to edit and
// delete) Edit all comments or Edit own comments and Delete all comments or
// Delete own comments project permissions. OAuth 2.0 tokens need the
// read:jira-work and write:jira-work scopes.
//
// # Inbound
//
// Instance.Handler serves the webhooks of an admin webhook: registered in
// Jira's administration (System, WebHooks) or with POST
// /rest/webhooks/1.0/webhook by a site administrator, with a secret. That
// is the mechanism a plain API-token integration can use. The other two
// mechanisms are not available to one: Connect apps sign with a JWT whose
// shared secret the app installation provides, and OAuth 2.0 (3LO) apps are
// registered with the dynamic webhook API and receive a bearer token signed
// with the client secret; Forge apps use their own triggers.
//
// The handler verifies X-Hub-Signature, "method=signature" where signature
// is the hex HMAC of the raw body under the secret, in constant time before
// decoding anything. Only method sha256 is accepted: Jira documents that it
// "might start using another method", and an unknown method is refused
// rather than guessed. Retries (up to five, on 408, 409, 425, 429 and 5xx)
// resend the body.
//
// A delivery is identified for deduplication by a digest of its raw body,
// never by X-Atlassian-Webhook-Identifier: the signature covers the body
// only, so any header is whatever the sender of a captured body writes. The
// identifier is kept as the envelope's correlation, nothing more. The event
// is named by the signed body's webhookEvent; a payload that is not the
// shape of the event it names (a comment event without a comment, an issue
// event with one) is refused as malformed.
//
// Inboxes bind projects by key (the envelope destination) and use
// per-work-item selection. The work item reference is namespace Namespace,
// kind work_item, ID the numeric issue ID, with no parent: an issue keeps
// its ID when its key changes or it moves to another project, so the
// reference, which is also the reply route, survives. Senders are Atlassian
// account IDs under Namespace, kind user. App accounts (accountType "app")
// are bot senders (model.ContactKindBot) and author automated messages;
// Atlassian and customer accounts are people.
//
//   - jira:issue_created: the description is the first message (message ID
//     issue:<id>) carrying the issue state; an empty description reports
//     the state only.
//   - jira:issue_updated: the state, and, when the changelog names the
//     description, an edit of the description message (recorded as a first
//     observation if unknown, deleted when it was cleared).
//   - jira:issue_deleted: binding deleted.
//   - comment_created, comment_updated and comment_deleted: a message
//     (comment:<id>), its edit and its deletion. A comment restricted to a
//     group or role, or a Service Management internal note, has an internal
//     audience. An edit names its editor (updateAuthor).
//   - a comment, or an edit of one, by Config.AccountID whose payload
//     carries the echo property reports the marked attempt
//     (model.EchoRef).
//
// The state maps the status category: "new" (To Do) and "indeterminate" (In
// Progress) are open, "done" is closed; the raw status is the status name.
// Labels are the field FieldLabels. Config.LocalFields declares the fields
// uma owns; every other field is ingest-only.
//
// Comment and description bodies are Atlassian Document Format documents
// (REST API v3); a payload that carries a plain string is read as it is.
// Mentions, emoji and cards keep their visible text and media is dropped.
//
// # Outbound
//
// Replies are opt-in (Config.Replies): send creates a comment on the reply
// route, as an ADF document (blank-line-separated blocks are paragraphs,
// newlines hard breaks); only public text content is sent, at most 32767
// characters. Until enabled, send, edit and delete report unavailable with
// reason disabled. A sent comment's ID is reported as its provider message
// reference.
//
// Jira comments carry no visible round-trip marker, so the request sets a
// comment property (entity property, EchoPropertyKey) naming the operation
// and attempt, in the same request that creates or edits the comment.
// Reconcile reads the issue's comments newest first, within
// ReconcileWindow, and the property of those written by Config.AccountID.
//
// Edit and delete target comment:<id> messages. The description (issue:<id>)
// is rejected unsupported. The adapter checks no authorship: any comment the
// credential may change is changed, and Jira's refusal is a final rejected
// outcome, never a new comment.
//
// # Provider answers
//
// 201 and 200 are provider_accepted; 400 and 413 are rejected (a 400 is
// also Jira's answer to a user who may not edit or delete a comment); 401
// and 403 are unauthorized (a rejected outcome for a mutation); 405 is
// Jira's answer to an anonymous call; 404 is rejected not_found, naming
// the work item binding inaccessible for a send (Jira answers 404 for a
// missing issue and for one the credential may not browse); 429 is
// rate_limited with the stated wait (Retry-After, else X-RateLimit-Reset,
// else one minute). Comment creation has no idempotency key, so a 5xx
// answer or a transport failure after the request was written is
// outcome_unknown and reconciled before any retry; only a failed dial or TLS
// handshake, which never reached Jira, is transient. Reads retry on any 5xx
// or transport failure.
//
// # Provider documentation
//
// The endpoints, fields and limits were checked against
// https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issue-comments/,
// https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issue-comment-properties/,
// https://developer.atlassian.com/cloud/jira/platform/webhooks/,
// https://developer.atlassian.com/cloud/jira/platform/rate-limiting/,
// https://developer.atlassian.com/cloud/jira/platform/basic-auth-for-rest-apis/,
// https://developer.atlassian.com/cloud/jira/platform/oauth-2-3lo-apps/ and
// https://developer.atlassian.com/cloud/jira/platform/apis/document/structure/.
package jira
