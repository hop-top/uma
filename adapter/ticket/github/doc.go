// Package github is the GitHub issues adapter for uma, type key
// github.issues: one channel per GitHub account (a GitHub App installation
// or a personal access token), one work item per issue, one message per
// issue description and comment.
//
// # Construction
//
// NewFactory returns the adapter.Factory a host registers in uma.Config.
// The package registers nothing at init and keeps no global state. Every
// provider call goes through the injected ports.HTTPClient (the engine's
// Config.HTTP, as adapter.Dependencies.HTTP); the adapter has no client of
// its own.
//
// # Capabilities
//
// The adapter declares ten capabilities (Factory.Describe). What each one
// does on GitHub, and what it needs:
//
//	Capability         GitHub                                    Needs
//	receive_http       issues and issue_comment webhooks         webhook secret
//	send               issue comment (POST .../comments)         Config.Replies, Config.AccountID
//	edit               comment or issue body (PATCH)             Config.Replies, Config.AccountID
//	delete             comment (DELETE)                          Config.Replies, Config.AccountID
//	react              comment or issue reaction                 Config.Replies, Config.AccountID
//	reconcile          comments by marker; message mutations;    send, edit, delete, react: Config.Replies;
//	                   issue state for acts; issue fields for    acts and updates: none
//	                   update effects
//	refetch            issue or comment read, for same-second    read access to issues
//	                   changes
//	work_item_act      close or reopen (PATCH .../issues/{n})    write access to issues
//	extension_execute  ExtensionUpdate: assignees, labels and    write access to issues
//	                   milestone (PATCH .../issues/{n})
//	history            issue description and comments (GET);     read access to issues; channel-wide:
//	                   channel-wide, of listed repositories      Config.HistoryRepositories
//
// Not declared: internal_note (GitHub has no private comments), stream,
// send_ephemeral and signal. Asking for one is model.Unsupported, never
// another capability.
//
// # Credentials
//
// The channel's credential reference is resolved through ports.Credentials
// on every provider call and every webhook request:
//
//   - CredentialPurposeAPI accepts ports.OAuth2 (a GitHub App installation
//     access token, which the host mints and refreshes) or ports.Secret (a
//     personal access token, classic or fine-grained). Either is sent as a
//     bearer token. A 401 answer re-resolves once with Fresh set.
//   - CredentialPurposeWebhook accepts ports.Secret: the webhook secret.
//
// The token needs read access to issues and metadata, and write access to
// issues to send replies or close and reopen issues.
//
// # Configuration
//
// The channel configuration is ConfigSchema version ConfigVersion with
// Config as its data. Validate checks it without I/O.
//
// # Inbound
//
// The adapter declares receive_http: Instance.Handler serves GitHub's
// issues and issue_comment webhooks. It verifies X-Hub-Signature-256, the
// HMAC-SHA256 of the raw body under the webhook secret, in constant time
// before decoding anything. The legacy SHA-1 signature is never accepted.
// Comments on pull requests and other events are acknowledged and dropped.
//
// A delivery is identified for deduplication by a digest of its raw body,
// never by X-GitHub-Delivery: the signature covers the body only, so anyone
// holding a captured signed body could replay it under a new GUID and have
// it taken for a new event. GitHub's redelivery resends the body under the
// same GUID, so it is still one event; two different bodies are two events
// under any header. The GUID is kept as the envelope's correlation, nothing
// more. X-GitHub-Event is unsigned too: an issues delivery that carries a
// comment, or an issue_comment one that does not, is refused as malformed
// instead of read as the event the header claims. Byte-identical bodies are
// one event; changes within one second of each other already refetch
// (below), so GitHub's copy settles whatever was collapsed. One event sent
// through two webhooks is one event only when the bodies are byte-identical.
//
// Inboxes bind repositories by full name ("owner/repo", the envelope
// destination) and use per-work-item selection. The work item reference is
// namespace Namespace, kind work_item, ID the issue number and parent ID
// the repository full name; it is also the reply route. Senders are GitHub
// user IDs under Namespace, kind user. Bot accounts are bot senders
// (model.ContactKindBot, declared in Descriptor.SenderKinds) and author
// automated messages; User and Organization accounts are people.
//
//   - issues.opened: the description is the first message, carrying the
//     issue state; an empty description reports the state only.
//   - issues.edited: the state, and an edit of the description message.
//   - issues.deleted and issues.transferred: binding deleted, and binding
//     moved with the new issue as successor.
//   - every other issues action: the state.
//   - issue_comment created, edited and deleted: a message, its edit and
//     its deletion. A created comment carries the issue state too, so a
//     comment seen before its issue binds the same work item.
//   - an edit names its editor: the sender of the delivery, which is the
//     user who edited, not the comment's author. An edit by Config.AccountID
//     of a body ending in an uma marker reports the marked attempt
//     (model.EchoRef), so the engine settles the edit from it.
//   - the reactions of a comment: GitHub sends no reaction webhooks, so
//     reactions never arrive as events and no reactor is ever named. A
//     comment payload carries a rollup of counts per content; when it has
//     one, created (with reactions) and edited deliveries add a count-only
//     snapshot after the message (the uma.reaction.summary part of a system
//     actor's reaction envelope, contents mapped to emoji as below, every
//     content listed). Counts move only when a comment payload arrives.
//
// GitHub's updated_at has one-second precision, the envelopes' source
// revision. Two changes within one second reach uma under the same
// revision; when their content differs the engine refetches (refetch):
// the adapter reads the issue or comment again and its current copy wins.
//
// The state maps open to open and closed to closed; the raw status is the
// state with GitHub's state_reason when it has one, on both states:
// "open", "open:reopened", "closed:completed", "closed:not_planned",
// "closed:duplicate". Labels and the milestone are fields FieldLabels and
// FieldMilestone. Config.LocalFields declares the fields uma owns; every
// other field is ingest-only, except the status, which uma can change at
// GitHub when it does not own it (see Work item acts). GitHub has no
// private comments, so internal notes stay in uma.
//
// # Outbound
//
// Replies are opt-in (Config.Replies): send posts an issue comment on the
// reply route, and reconcile lists the issue's comments. Until enabled send,
// edit, delete and react report unavailable with reason disabled, and
// reconcile resolves work item acts only. Only public text content is sent.
//
// GitHub comments carry no metadata besides their body, so the body ends
// with a hidden marker naming the operation and attempt: an HTML comment
// GitHub does not render. A sent comment's ID is reported as its provider
// message reference; a webhook for a comment by Config.AccountID carrying a
// marker reports model.EchoRef, so an echo that arrives before the send
// answer is still recognised. Reconcile looks for the attempt's marker
// among comments by Config.AccountID updated within ReconcileWindow.
//
// # Edit, delete and react
//
// Message mutations need Config.Replies like send. The target is the
// message's provider message ID: comment:<id> for a comment, issue:<id> for
// an issue description, which the route's issue names. The adapter checks
// no authorship: any comment the token may change is changed, a maintainer
// editing a contributor's comment included, and GitHub's refusal (403, or
// 404 and 410 for a target that is gone) is a final rejected outcome,
// unauthorized or not_found with no work item binding, never a new comment
// or a retry.
//
//	Edit    comment: PATCH .../issues/comments/{id}; description: PATCH
//	        .../issues/{n} with the body. The body is the text parts and the
//	        marker of the edit's operation and attempt. Non-text content or
//	        a non-public audience is rejected unsupported before any call.
//	Delete  comment: DELETE .../issues/comments/{id}. An issue description
//	        cannot be deleted: rejected unsupported.
//	React   comment: POST .../issues/comments/{id}/reactions; description:
//	        POST .../issues/{n}/reactions. Adding what the account holds is
//	        GitHub's 200 and applied. The outcome names the reaction
//	        (RefKindReaction, parent ID the message).
//	        Removal reads the reactions of that content (page by page),
//	        deletes the one by Config.AccountID, and is an applied no-op when
//	        there is none.
//
// Emoji map to GitHub's reaction contents byte for byte; any other emoji
// (a skin tone, a bare U+2764) is rejected unsupported before any call:
//
//	👍 +1   👎 -1   😄 laugh   😕 confused   ❤️ heart   🎉 hooray   🚀 rocket   👀 eyes
//
// A user may hold several reactions on a message (the adapter's reaction
// cardinality is multiple).
//
// A lost answer is reconciled by reading what GitHub holds, never by
// resending:
//
//   - edit: the body ends with the attempt's marker → applied; a comment
//     that is gone → rejected not_found; any other body → not applied.
//   - delete: the comment is gone → applied; still there → not applied.
//   - react: the account holds the reaction (a removal: holds none) →
//     applied; otherwise not applied.
//
// Not applied is failed with not_found, so a new attempt is safe. A delete
// has no marker to carry, so a deletion webhook names no attempt and
// settles nothing; reconciliation does.
//
// # Provider answers
//
// Provider answers map as follows: 201 is provider_accepted; 400 and 422
// are rejected; 401 and a 403 without rate limit headers are unauthorized;
// 404 is rejected not_found (issue inaccessible) and 410 rejected not_found
// (issue deleted), naming that work item binding state (Outcome.Binding);
// a redirect is not_found (issue moved); 429, and 403 with Retry-After,
// x-ratelimit-remaining 0 or a secondary rate limit message, is
// rate_limited with the stated wait (Retry-After, else x-ratelimit-reset,
// else one minute), including go-github's client-side short-circuit,
// which dispatches nothing. Comment creation has no idempotency key, so a
// 5xx answer or a transport failure after the request was written is
// outcome_unknown and reconciled before any retry; only a failed dial or
// TLS handshake, which never reached GitHub, is transient. Reads retry on
// any 5xx or transport failure.
//
// Edit, delete and react map alike, except that a 403 without rate limit
// headers and a 404 or 410 are rejected outcomes (unauthorized, not_found)
// with no binding, and that a 5xx or lost answer after the write was sent
// is outcome_unknown. The lookup of a reaction removal is a read.
//
// # Work item acts
//
// The adapter declares work_item_act: WorkItems.Act moves an issue between
// its two states, open and closed. Reopening is an action back to open, not
// a third state. The declared transitions are
//
//   - closed from open, with reason completed (the default) or
//     not_planned;
//   - open from closed, with reason reopened (the default).
//
// Pending and resolved, GitHub's duplicate reason (it needs the duplicated
// issue) and any other reason are not declared, so uma refuses them as
// unsupported before any call; they are never coerced onto closed.
//
// The requested status and reason map to the PATCH as follows; a request
// that omits the reason takes the default, which is sent explicitly:
//
//	Requested status, reason   state    state_reason
//	closed                     closed   completed
//	closed, completed          closed   completed
//	closed, not_planned        closed   not_planned
//	open                       open     reopened
//	open, reopened             open     reopened
//	closed, duplicate          unsupported
//	pending, resolved          unsupported
//
// GitHub answers with the issue's own state and state_reason, which the work
// item keeps in its raw status ("closed:not_planned", "open:reopened"), as
// for an issues webhook.
//
// ActWorkItem sends one "Update an issue" PATCH with state and
// state_reason, the default reason sent explicitly. It never reads the
// issue first: the local copy may be stale, and GitHub applies a repeated
// state idempotently. The answer is delivered, reporting the issue state
// GitHub returned (the same mapping as inbound, with Config.LocalFields)
// under its updated_at as source revision; uma applies it in source order.
// Failures map as for comments, except that a 403 without rate limit
// headers is a rejected outcome (unauthorized) rather than an error.
//
// A lost answer is reconciled by reading the issue: it holds the requested
// state and reason (the default when none was requested) → delivered;
// otherwise the act was not applied → failed, and a new attempt is safe.
// The issues webhook GitHub sends for the change, even when the channel's
// account made it, is an ordinary state observation: it never settles the
// act. Acts and their reconciliation need no replies.
//
// Issue state and conversation status are independent lifecycles. Closing or
// reopening an issue, by WorkItems.Act or by a person on GitHub, never
// changes the status of the issue's conversation, and a conversation
// transition never reaches GitHub.
//
// # History
//
// The adapter declares history: Instance.History reads a window of past
// messages for Runtime.Import, oldest first, as the webhook reports them
// (same provider message IDs and destinations), so a message is imported
// once whichever way it arrived; see Instance.History for the listings,
// the window, paging and the cursor. A conversation's window is its
// issue's; a channel's spans Config.HistoryRepositories, since a channel
// is an account that GitHub does not scope to repositories. Reads need
// read access to issues only and change nothing at GitHub.
package github
