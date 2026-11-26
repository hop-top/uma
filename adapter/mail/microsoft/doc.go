// Package microsoft is the uma mail adapter over the Microsoft Graph v1.0
// mail API (type key microsoft.mail): one channel per mailbox, for tenants
// and mailboxes that IMAP and SMTP cannot reach.
//
// It speaks plain JSON over the host's ports.HTTPClient (no Graph SDK) and
// declares receive, receive_http, send, reconcile and the provider conversation
// state verbs archive, move, read_state, flag and trash.
//
// # Credentials
//
// Every unit of work resolves the access token through ports.Credentials
// with purpose PurposeGraph, accepting ports.OAuth2 only; the host acquires
// and refreshes it, delegated or app-only. A 401 resolves once more with
// Fresh set and repeats the call; a second 401 is model.Unauthorized and
// never retried. The webhook client state is derived from a ports.Secret
// resolved with purpose PurposeNotifications. The application needs the
// Graph permissions in RequiredPermissions, Mail.ReadWrite and Mail.Send,
// as delegated or as application permissions; app-only tokens need
// Config.Mailbox, since /me requires a signed-in user. That is draft send
// mode, the default; a channel in direct send mode needs Mail.Send alone
// (see Sending and Config.RequiredPermissions).
//
// # Categories
//
// Execute runs ExtensionCategories, a message update (PATCH) on the
// message the effect targets (Target, else the route's ParentID) that
// replaces its categories with the action's list. The update is
// idempotent, so a lost answer is transient and repeated, never
// reconciled. uma's own labels are untouched.
//
// # Conversation state
//
// The state verbs change the mailbox's own view (scope account, move shape
// folder, roles inbox, archive, trash and junk, flag kinds star and important). Each is one
// Graph call per message of the conversation, named by its immutable ID:
// PATCH isRead, PATCH flag (star), PATCH importance (important: high or
// normal), POST move (archive and trash move to the well-known folders,
// their undo to Config.UndoFolder, the inbox by default). Every call sends
// Prefer: IdType="ImmutableId", so a move changes no reference and reports
// none. A lost answer is outcome_unknown and Reconcile reads the messages
// back (a move is applied when found in its destination). The receiver runs
// a delta query per watched folder (inbox, Archive, Deleted Items and
// Config.WatchedFolders) and reports each message's folder, read state and
// star and important flags as a partial conversation.state envelope; a message removed from one
// folder is read back by ID, so a move is one report and never a delete.
//
// # Receiving
//
// Change notifications are the fast path: Runtime.Receive's receiver keeps
// a subscription on the inbox (created messages; renewed before expiry,
// recreated when Graph removes it, reauthorized on request), and the
// webhook (Runtime.Webhook) answers Graph's validationToken check, checks
// each notification's clientState in constant time and fetches the notified
// message from the channel's own mailbox. Delta query on the inbox fills
// gaps: every receiver round resumes from the stored delta link. Both paths
// use immutable IDs, so one message has one ID whichever path sees it, and
// ingress deduplicates on it.
//
// An inbound message maps to a text part (the body, requested as text),
// one PartAttachment structured part per attachment (metadata, up to 53; the
// payload counts the others) and a PayloadMessage payload (subject,
// conversationId, recipients, Authentication-Results). The bytes of file
// attachments are downloaded one at a time from the attachment's raw content
// (.../attachments/{id}/$value) and staged through the engine's
// adapter.AttachmentSink: up to 10 attachments of 25 MiB each, each named in
// an attachment part after the descriptors, and each descriptor reporting
// "staged". Item and reference attachments have no raw bytes to stage, and
// attachments past the bounds, gone, or refused by the engine's attachment
// checks stay described only, as do all of them when the engine has no blob
// store. A download or staging failure a repeat may fix fails the fetch. History stages
// the same way through adapter.HistoryRequest.Stager (describing only without
// one). The sender is the
// from address in IdentityNamespace, with its domain lowercased and its local
// part kept byte-exact (model.NormalizeEmail; an address it cannot normalize
// is keyed as Graph gave it), with the Graph message ID as ParentID. The same
// spelling is the recipient of a start (Factory.NormalizeRecipient,
// adapter.RecipientNormalizer): a start to "Bob@EXAMPLE.org" and a message
// from "Bob@example.org" are one identity, the same operation key with
// either spelling is one operation. A recipient that is not a bare address is
// kept as given, never refused by the normalization: a send decides on it as
// before. Bob@ and bob@ stay two identities; plus-addressing and punycode are
// not folded. Threading uses the RFC 5322 headers only:
// Message-ID (else internetMessageId), In-Reply-To, References and List-Id
// become email.header references and Auto-Submitted marks automated mail.
// conversationId is a provider hint in the payload; it never selects a
// conversation.
//
// # Sending
//
// A reply to a route with ParentID is createReply on that message, then an
// update (body, the route address as sole recipient, an attempt property)
// and send; other routes get a new draft. Graph assigns the Message-ID at
// draft creation and the adapter cannot set it: the outcome reports it with
// the Graph ID. Each draft carries AttemptProperty, a named extended
// property holding the operation attempt, which Reconcile looks up in Sent
// Items after a lost answer. Retry-After is reported to the operation
// ledger and never waited on here.
//
// # Send modes and permissions
//
// Config.SendMode picks how a send is delivered.
//
//   - draft (the default): the draft flow above. It needs Mail.ReadWrite
//     and Mail.Send, reports the draft's Graph and Internet message IDs, keeps
//     the sent copy in Sent Items and lets Reconcile find it.
//   - direct: one POST /users/{mailbox}/sendMail (/me/sendMail without a
//     mailbox) with {message, saveToSentItems}. It needs Mail.Send only, for
//     example an app whose Exchange application access policy limits it to
//     one sender mailbox. Graph answers 202 with no message ID, so the
//     outcome is provider_accepted with no provider reference; a lost
//     answer or a 5xx other than 503 is outcome_unknown and stays so: the
//     adapter reads nothing back (Mail.Send cannot read Sent Items), so the
//     channel cannot reconcile and a send is never repeated on its own.
//     Direct mode keeps no sent copy unless Config.SaveToSentItems is set,
//     sends new messages only (a route with a parent message is refused,
//     since createReply needs a draft) and reports every capability but send
//     and start_conversation unavailable.
//
// Config.RequiredPermissions reports the permissions of a channel's mode.
// The descriptor is the factory's, not the channel's: to register an adapter
// that declares only send and requires only Mail.Send, build the factory
// with Options.SendOnly. Its descriptor declares send and start_conversation
// alone, no events and no reconcile window, and its channels default to
// direct mode (draft mode is refused).
//
// # Sending without the engine
//
// A Mailer sends one message with the same HTTP, token and error handling as
// direct mode (one code path), for a host that needs to send a notice and
// has no channels, routes or effects:
//
//	m, err := microsoft.NewMailer(microsoft.MailerConfig{
//		Mailbox: "no-reply@contoso.example", HTTP: client, Tokens: tokens,
//	})
//	err = m.Send(ctx, microsoft.Message{
//		To: []string{"user@example.org"}, Subject: "Your sign-in code", Text: "123456",
//	})
//
// Tokens is a TokenSource with the shape of hop.top/kit's storage/secret/
// oauth TokenSource (Token and Invalidate); TokenSourceFunc wraps one, as
// this package does not import kit. A 401 invalidates the source and sends
// once with a fresh token, a second 401 is model.Unauthorized; 429 and 5xx
// are retried within MaxRetries, waiting Retry-After (bounded by
// MaxRetryWait) or an exponential backoff, so a message can arrive twice
// when a 5xx followed a delivery; a lost answer is model.OutcomeUnknown and
// never retried. The context bounds everything. Errors carry no message
// content, address or token. Graph's JSON message holds one body: a
// Message with an HTML alternative is sent as that HTML.
package microsoft
