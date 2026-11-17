// Package google is the uma mail adapter over the Gmail API v1 (type key
// google.mail): one channel per mailbox. Unlike IMAP and SMTP on Gmail,
// whose https://mail.google.com/ scope is restricted, it can run with
// separable scopes: gmail.send (sensitive) for sending and gmail.readonly
// (restricted, as every Gmail read scope is) for receiving.
//
// It speaks plain JSON over the host's ports.HTTPClient (no Google API
// client library) and declares receive, receive_http, send, reconcile and
// extension_execute (ExtensionLabels, which needs gmail.modify, a scope
// that alone also covers reading and sending). Raw messages are parsed and
// composed with hop.top/uma/adapter/mail/mime.
//
// # Credentials
//
// Every unit of work resolves the access token through ports.Credentials
// with purpose PurposeGmail, accepting ports.OAuth2 only; the host acquires
// and refreshes it (user consent, or a service account with domain-wide
// delegation). A 401 resolves once more with Fresh set and repeats the
// call; a second 401 is model.Unauthorized and never retried. The token
// needs RequiredPermissions.
//
// # Receiving
//
// Runtime.Receive's receiver keeps a users.watch on the inbox, publishing
// to Config.Topic (renewed daily and always before its 7-day expiry), and
// reads users.history.list (messageAdded, INBOX) from the stored historyId
// every round, so notifications Gmail drops (it sends at most one per
// second per user) or Pub/Sub gives up on are filled in. When no historyId
// is stored, or Gmail answers 404 for one it no longer holds, it runs the
// documented full synchronization: the profile's current historyId is
// read first, then every inbox message received within the initial sync
// window is fetched; a forced resync is counted in the receiver state
// (FullResyncs, LastFullResync), since changes in the gap outside the
// window are not received.
//
// Push is the fast path. The host mounts the channel's webhook
// (Runtime.Webhook) at the endpoint of an authenticated Pub/Sub push
// subscription. Each request's OIDC token must be an RS256 token signed by
// a current Google key (GoogleCertsURL, fetched through the HTTP client
// and cached per its max-age), issued by accounts.google.com, for
// Config.PushAudience, naming Config.PushServiceAccount with a verified
// email, and unexpired. The notification's historyId then triggers one
// history page from the stored historyId and wakes the receiver.
//
// Every message is fetched with format=raw and parsed: the text body (or
// rendered HTML) becomes a text part, every other MIME part one
// PartAttachment structured part (metadata and digest, up to 53; the payload
// counts the others), and Gmail metadata a PayloadMessage payload. The raw
// form is the whole message, so the attachment bytes are staged through the
// engine's adapter.AttachmentSink (package stage) from what was read, with no
// users.messages.attachments.get call: up to stage.MaxAttachments attachments
// of stage.MaxAttachmentBytes each, each named in an attachment part after the
// descriptors, and each descriptor reporting "staged". Attachments past the
// bounds, or refused by the engine's attachment checks, stay described only,
// as do all of them when the engine has no blob store; a staging failure a
// retry may fix fails the fetch, so the message is read again. History stages the
// same way through adapter.HistoryRequest.Stager, and describes only without one. A message whose
// raw answer is over the bound (Gmail receives up to 70 MB after encoding;
// raw encodes it again) is read with format=metadata instead and delivered
// from its headers alone, with body_omitted and size_estimate in the payload:
// the receiver goes on. Drafts and messages
// labelled SENT are skipped. So is a message Gmail answers whole whose raw
// form is not base64url or does not parse: Gmail never changes a message's
// bytes, so the receiver moves the cursor on and the round that does reports
// ObservationSkipped (a push request, which moves no cursor, does not).
// An answer that is not what was asked for, whole (a message answer not
// JSON, truncated, without the message's ID or raw form, such as a proxy's
// error page; a history.list or messages.list page or the profile not JSON),
// is a fault of the response: the round fails as transient and the cursor
// stays before what it could not read, so it is read again and never
// skipped for it. A round that fails part way through a history page keeps
// the cursor past the records it handed over whole: the next connection
// does not hand them over again, so one that meets only the same failure
// accepts nothing and Retry.MaxAttempts ends Runtime.Receive. The
// sender is the From address in IdentityNamespace, with its domain
// lowercased and its local part kept byte-exact (model.NormalizeEmail; an
// address it cannot normalize is keyed as parsed), with the Gmail message ID
// as ParentID. The same spelling is the recipient of a start
// (Factory.NormalizeRecipient, adapter.RecipientNormalizer): a start to
// "Bob@EXAMPLE.org" and a message from "Bob@example.org" are one identity,
// the same operation key with either spelling is one operation, and a
// recipient that is not a bare address is refused at the call. Bob@ and bob@
// stay two identities; plus-addressing, Gmail dot rules and punycode are not
// folded. Threading uses the RFC 5322 headers
// only: Message-ID, In-Reply-To, References and List-Id become
// email.header references and Auto-Submitted marks automated mail.
// threadId is a provider hint in the payload; it never selects a
// conversation.
//
// # Sending
//
// Send composes a text/plain message and calls users.messages.send with
// it raw. The Message-ID is uma's, derived from the operation. A reply
// (route with ParentID) reads the parent's metadata and sets In-Reply-To,
// References, the parent's subject and threadId, Gmail's documented
// conditions for joining the thread. messages.send has no idempotency key:
// any 5xx, and any failure after the request may have been written, is
// outcome_unknown; Reconcile then searches Sent for the Message-ID with
// rfc822msgid:. Only a failure before the request was written is
// transient, and only rate limits (429, 403 rateLimitExceeded,
// userRateLimitExceeded, dailyLimitExceeded) are rate_limited. Retry-After
// is reported to the operation ledger and never waited on here.
//
// # Labels
//
// Execute runs ExtensionLabels, users.messages.modify on the message the
// effect targets (Target, else the route's ParentID) with the action's
// add and remove lists. modify is idempotent, so a lost answer is
// transient and repeated, never reconciled. uma's own labels are untouched.
package google
