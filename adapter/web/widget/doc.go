// Package widget is the native.webwidget adapter: a browser chat channel
// the host serves over its own HTTP mux. It uses the standard library and
// public uma packages only.
//
// # Surfaces
//
// The adapter has two HTTP surfaces, both mounted by the host:
//
//   - The receiver (capability receive_http), served through
//     Runtime.Webhook: POST .../session issues a visitor session token,
//     POST .../messages hands a visitor message to the engine, POST
//     .../uploads stages a visitor file through the engine's attachment
//     sink for a later message to reference, POST .../actions and POST
//     .../submissions hand a control press, a command or a form to the
//     engine as an interaction (see Interactions), and POST
//     .../interactions evaluates a form submission synchronously. The
//     handler routes on the last path segment, so the host mounts it under
//     any prefix. Every request but an upload is a short JSON POST: bodies
//     are bounded by http.MaxBytesReader on top of the engine's own limit,
//     and a request without an application/json content type is refused,
//     which keeps cross-site form posts out of the cookie-based session
//     endpoint. An upload carries the file under its own type and the
//     bearer token, bounded by the engine's attachment limit.
//   - The event stream, Factory.Events: a GET serving Server-Sent Events to
//     one visitor. It reads the adapter's own visitor log only and never
//     touches the engine, so a long-lived connection never holds engine
//     work open and Engine.Close never waits on a browser.
//
// # Sessions
//
// The host decides who a visitor is. SessionVerifier inspects the session
// request (the host's own cookie, login or site binding) and returns a
// Visitor whose assurance is explicit: AssuranceVerified for a
// host-authenticated user, AssuranceAnonymous otherwise. There is no
// default: an empty assurance is refused. Anonymous and verified visitors
// live in separate identity namespaces (NamespaceVisitor, NamespaceUser), so
// an anonymous visitor can never assume a verified user's identity by
// choosing the same ID.
//
// The adapter then issues a session token: the visitor, assurance, site
// (the inbox destination), session ID, channel and expiry, signed with
// HMAC-SHA256 under the channel's session key. The key is resolved through
// ports.Credentials for CredentialPurpose on every request and outbound
// call, never cached; it must be a ports.Secret of at least MinKeyBytes.
// Verification compares MACs in constant time before decoding anything,
// then requires the bound channel, an unexpired token and a site the
// channel still lists. Rotating the key ends every session; removing a site
// ends that site's sessions.
//
// The token travels in the Authorization header as a bearer token. The
// event stream also accepts it as the token query parameter, because the
// browser EventSource API cannot set headers; keep the session TTL short
// and the stream URL out of access logs.
//
// # Delivery
//
// Outbound effects (messages, native streams, ephemeral replies and typing
// signals) are appended to the visitor's log (Log) and pushed to every open
// event stream of that visitor. Server history is authoritative: the client
// never supplies history, it only resumes from a cursor. Each event carries
// its log sequence as the SSE id, so a reconnecting EventSource resumes
// with Last-Event-ID and receives exactly the events after it. When the log
// no longer retains the events after the cursor, the stream first sends a
// reset event, and the client reloads history from the host. Ephemeral
// replies and signals are not replayed after they expire.
//
// SSE rather than long-polling or WebSockets: delivery is one-way (visitor
// input uses POST), SSE is plain HTTP that the standard library streams
// with http.ResponseController, and EventSource reconnects and resends the
// cursor on its own. WebSockets need a non-standard-library implementation;
// long-polling needs the same cursor logic plus a request per batch.
//
// The default Log is in memory and per process: run the receiver, workers
// and event streams in one process, or supply a shared Log.
//
// # Mutations
//
// The adapter declares edit, delete and react. Uma is the widget's
// provider, so any message of a visitor's log can be changed through the
// engine, one the widget sent or one the visitor sent. The target is the
// message's provider message reference (MessageRef) on the channel; the
// route is the conversation's visitor or session, as for a send, and the
// audience must be public. Each mutation is appended to the route's visitor
// log and delivered like a message, so it settles provider_accepted once
// the log stored it, and a lost answer settles outcome_unknown and is never
// sent again. A log refusing one with model.Unauthorized, model.NotFound or
// model.Unsupported settles it rejected with that code.
//
// The event stream carries three event types for them, each naming the
// message in "message": "message.edit", whose parts are the whole new
// content; "message.delete", without content; and "reaction", with the
// "emoji" and, once withdrawn, "removed". A client ignores event types it
// does not know, so an older client shows the original message. Reactions
// belong to the host: the widget has no reactor identity, and the host may
// hold one reaction per emoji on a message.
//
// Visitors do not edit, delete or react: the receiver has no frames for
// them.
//
// # Receipts
//
// The adapter declares receipt. The host tells a visitor that a message the
// visitor sent was delivered or read (Messages.Receipt, kinds
// model.ReceiptDelivered and model.ReceiptRead); both kinds are
// expressible, and any other is refused as unsupported without a log
// append. The target is the visitor's message, a provider message
// reference (MessageRef) with the 'v' prefix the receiver gave it: a
// message the widget sent is no target, as a receipt answers a received
// message. The route is the conversation's visitor or session, as for a
// send, and the audience must be public. The receipt is appended to the
// route's visitor log like an edit, so it settles provider_accepted once
// the log stored it, adds no reference, and a lost answer settles
// outcome_unknown and is never sent again. A log refusing it with
// model.Unauthorized, model.NotFound or model.Unsupported settles it
// rejected with that code.
//
// The event stream carries one event type for it, "receipt", with the
// visitor's message in "message" and the receipt kind in "kind":
//
//	event: receipt
//	data: {"type":"receipt","message":"v...","kind":"read"}
//
// It reaches the visitor of the route only, and a session route only that
// session, like every entry of the log. A client ignores event types it
// does not know, and an EventSource dispatches a named event to its
// listener only, so an older client shows no receipt. The host submits a
// receipt once per operation: the engine replays a retried submission
// without a second append, and a client treats a receipt it sees again as
// the same state, never a new one.
//
// # Durable delivery
//
// A delivery is applied once Log.Append returns: the operation settles
// provider_accepted and is never sent again. With the default MemoryLog a
// restart therefore loses every entry not yet read by a stream: a
// reconnecting browser gets a reset event and reloads history from the
// host, and nothing redelivers those entries. A host that runs a durable
// store (uma.Config.RequireDurable) should supply a durable Log, kept next
// to its other state. The storelog subpackage is one: it keeps each visitor
// log in the host's uma store as a recipient log (store.RecipientLogKey),
// with the same retention and reset behaviour as MemoryLog. A host writing
// its own follows the same rules, and runs the widgettest suite:
//
//   - Append assigns per-key sequences 1, 2, 3 without gaps in one
//     transaction (a row per visitor holding the last sequence, and a row
//     per entry), and reports a failed commit as an untyped error, never
//     as nothing stored: the commit may have applied.
//   - Read pages entries by sequence; Watch wakes in-process streams, and
//     a host serving streams from several processes adds a notification
//     (such as PostgreSQL LISTEN/NOTIFY) or a bounded rescan.
//   - Entries carry their ExpiresAt; Read may skip or a sweep may delete
//     expired ones, and old entries may be trimmed: a stream behind the
//     oldest kept entry resets.
//
// A Log outside the uma store is also outside privacy erasure: see the
// Privacy erasure section of Log. The storelog subpackage is inside it.
//
// # Interactions
//
// A visitor acts on a control, types a command or sends a form. The page
// posts it to .../actions (ActionRequest: an action ID with an optional
// value, or a command with arguments) or .../submissions (SubmissionRequest:
// the values of a form under its schema), and the receiver hands it to the
// engine as an interaction envelope: the sender is the session's visitor,
// the references are the session room and the message the control sits on,
// and the ID is scoped to the visitor like a message ID. The answer is 202
// with an AcceptedInteraction only after the engine accepted the
// interaction, which means it holds it durably, so a visitor who got 202 is
// never silently dropped; a retry with the same client ID is a duplicate.
// The deadline is Options.InteractionTimeout. The widget has no provider
// to give it a response token and states no respond window.
//
// .../interactions stays the synchronous path: it evaluates a submission
// (field errors, dynamic options) through the host's interaction validator
// and records nothing, so a page validates as the visitor types and posts to
// .../submissions only when the form is ready.
//
// The adapter declares respond and respond_follow_up. A response is
// appended to the visitor's log as an interaction.response event and
// delivered like any other: the operation settles provider_accepted once
// the log stored it. Its mode is the engine's:
//
//   - message: a new message, "message" its ID; public.
//   - ephemeral: shown to this visitor only until the event's expires_at
//     (EphemeralTTL after delivery); restricted.
//   - update: replaces the whole content of the message the control sits
//     on, like an edit; public.
//
// The event names the interaction by the ID the receiver returned, so a
// page ties a response to the press that caused it. The log entry also keeps
// the engine's interaction reference (Entry.Interaction) server side; a
// host's own Log stores it with the rest of the entry.
//
// # Controls
//
// An outbound message can ask the visitor to act. A structured part named
// ControlsSchema at ControlsVersion carries Controls: buttons (an action ID,
// a label and an optional value), selects (an action ID and options) and
// one form (a schema, a submit label and typed fields). The adapter checks
// the shape when the part is sent and delivers it unchanged; the page
// renders it, sends a press or a pick as an action and a form as a
// submission. A structured part of any other schema, or of a later version
// of this one, is delivered unchanged and a page that does not know it
// leaves it out: the widget never turns one into text. ControlsPart builds
// the part.
//
// # Routes
//
// A visitor message's sender is the visitor (kind KindVisitor or KindUser),
// and it carries the session as a room reference whose ParentID is the
// visitor. A per-contact inbox replies to the visitor, reaching every
// session; a per-room inbox keeps one conversation per session and replies
// to that session only.
//
// # Not supported
//
// Outbound attachment parts are passed to the visitor as attachment IDs
// for the host to serve. Visitor typing is not forwarded: a status
// envelope records nothing. Visitor-initiated edits, deletes and
// reactions are not received.
package widget
