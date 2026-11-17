// Package gitlab is the GitLab issues adapter for uma, type key
// gitlab.issues: one channel per GitLab account (a personal, project or group
// access token, or an OAuth token), one work item per issue, one message per
// issue description and note.
//
// # Construction
//
// NewFactory returns the adapter.Factory a host registers in uma.Config. The
// package registers nothing at init and keeps no global state. Every provider
// call goes through the injected ports.HTTPClient (the engine's Config.HTTP,
// as adapter.Dependencies.HTTP); the adapter has no client of its own and no
// GitLab SDK.
//
// # Capabilities
//
// The adapter declares eight capabilities (Factory.Describe). What each one
// does on GitLab, and what it needs:
//
//	Capability     GitLab                                      Needs
//	receive_http   Issue Hook and Note Hook webhooks           webhook secret token
//	send           issue note (POST .../issues/:iid/notes)     Config.Replies, Config.AccountID
//	edit           note or issue description (PUT)             Config.Replies, Config.AccountID
//	delete         note (DELETE)                               Config.Replies, Config.AccountID
//	reconcile      notes by marker; message mutations; issue   send, edit, delete: Config.Replies;
//	               state for acts                              acts: none
//	refetch        issue or note read, for same-timestamp      read access to issues
//	               changes
//	work_item_act  close or reopen (state_event)               write access to issues
//	history        one issue's description and notes (GET)     read access to issues
//
// Not declared: react, internal_note and extension_execute (GitLab has the
// calls; the adapter does not use them yet), stream, send_ephemeral and signal.
// Asking for one is model.Unsupported, never another capability.
//
// # Credentials
//
// The channel's credential reference is resolved through ports.Credentials on
// every provider call and every webhook request:
//
//   - CredentialPurposeAPI accepts ports.OAuth2 or ports.Secret (a personal,
//     project or group access token). Either is sent as a bearer token. A 401
//     re-resolves once with Fresh set.
//   - CredentialPurposeWebhook accepts ports.Secret: the webhook's secret token.
//
// # Inbound
//
// Instance.Handler serves GitLab's issue and comment webhooks. It compares
// X-Gitlab-Token with the secret token in constant time before decoding
// anything. A delivery is identified for deduplication by a digest of its raw
// body; the Idempotency-Key header is kept as the envelope's correlation.
// X-Gitlab-Event must agree with the payload's object_kind. Notes on merge
// requests, commits and snippets, system notes and other events are
// acknowledged and dropped. GitLab documents no webhook for a deleted note.
//
// The work item reference is namespace Namespace, kind work_item, ID the
// issue's iid and parent ID the numeric project ID; it is also the reply
// route. Inboxes bind projects by path_with_namespace (the envelope
// destination). Senders are GitLab user IDs under Namespace, kind user; the
// generated bot users of access tokens are bot senders. An internal note has an
// internal audience.
//
// # Outbound
//
// Replies are opt-in (Config.Replies). A reply is an issue note whose body ends
// with a hidden marker (an HTML comment) naming the operation and attempt, so a
// webhook for a note by Config.AccountID reports model.EchoRef and Reconcile
// finds a lost note by it. Lines of reply text that GitLab would run as quick
// actions are escaped with a backslash, so a reply never changes the issue.
// A lost answer is reconciled by reading what GitLab holds, never by resending.
//
// Issue state and conversation status are independent lifecycles: closing or
// reopening an issue never changes the status of the issue's conversation.
//
// See docs/adapters/gitlab.issues.md for the full reference.
package gitlab
