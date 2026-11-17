// Package imap is the imap.mail adapter: one channel per mailbox account,
// retrieving inbound mail over IMAP and sending outbound mail by SMTP
// submission, both authenticated with the channel's one credential.
//
// # Connections
//
// Every connection resolves the credential through package auth with the
// channel's credential reference: purpose PurposeIMAP for retrieval,
// PurposeSubmission for submission, accepting OAuth2, Password and
// ClientCert. TLS comes before AUTH: implicit TLS first, STARTTLS when the
// implicit port is unset or refuses the connection, plaintext only to a
// loopback address the channel opted into. The adapter runs IMAP STARTTLS
// itself rather than through go-imap, which hides the upgraded connection,
// so SCRAM-SHA-256-PLUS can bind to it. An empty AUTHENTICATE continuation
// goes out as an empty line, not go-imap's "=". IMAP cannot re-authenticate
// a session: a session is replaced before its credential expires (the
// auth reconnect margin, Config.ReconnectMarginMS).
//
// # Receiving
//
// Instance.Run examines the mailbox read-only and fetches with BODY.PEEK,
// so retrieval never changes flags; flags are not read evidence. The cursor
// is the mailbox's UIDVALIDITY and the highest delivered UID, kept in the
// engine's protocol store when there is one. Messages above the cursor are
// delivered in UID order and the cursor advances after each; between rounds
// the receiver waits in IDLE, re-issued at most every Config.IdleRestartMS
// (29 minutes at most, RFC 2177), or polls on a server without IDLE. The
// first synchronization delivers the messages of Config.InitialSyncWindowMS.
// The instance implements adapter.ReceiverReporter: connected while an
// authenticated session stands, not connected when it ends or a connection
// is being retried.
//
// A UIDVALIDITY change is a gap: every stored UID is void. The receiver
// reports ObservationGap, counts the gap in its state and resynchronizes the
// messages received since the last delivered one. Event IDs derive from the
// destination and a digest of the message bytes, never from a UID, so
// ingress drops the copies it already holds.
//
// A message can leave the mailbox between the search, the metadata fetch and
// the body fetch: another session expunges it. Every fetch addresses messages
// by UID, because a sequence number names another message once the mailbox
// shrinks. A server answers a UID FETCH of an expunged UID with OK and no data
// and may send an EXPUNGE during any UID command (RFC 3501 sections 6.4.8 and
// 7.4.1, RFC 9051 sections 6.4.9 and 7.5.1); an EXPUNGE carries a sequence
// number, not a UID, so it is not the signal. The receiver takes a message for
// gone when the server has no bytes for it and a UID SEARCH for it finds
// nothing: it skips the message, moves the cursor on and reports
// ObservationVanished. A message that is there and cannot be delivered (it does
// not parse, it has no usable From address, it has no bytes) is skipped the
// same way and reported as ObservationSkipped: the mailbox does not change the
// bytes of a message, so fetching it again fails alike. Neither observation
// names the message. The cursor never moves past a message for a reason a retry
// can fix: a lost connection, a server that answers nothing for a message it
// still holds (asked up to three times in a round, then the round fails with an
// unavailable error), or a refusal by the sink. Such a message is fetched
// again by the next round or receiver, and a server that never answers for it
// stops the mailbox behind it, loudly, until the message leaves. A message
// expunged before the search was never seen and leaves no trace; a message
// expunged after its body arrived is delivered.
//
// A message is identified for deduplication by its destination and a digest
// of the bytes fetched, never by its Message-ID: the sender chooses that, so
// trusting it would let a forged mail that reuses the id of a genuine one,
// and arrives first, suppress the genuine one as a redelivery. The Message-ID
// threads only. A mailbox does not change the text of a message it holds, so
// a re-fetch (a UIDVALIDITY change, a crash before the cursor advanced, a
// reconnect, a copy within the mailbox) reproduces the bytes and
// deduplicates. A message over Config.MaxMessageBytes is delivered from its
// header block alone: its key digests that block and carries the RFC822.SIZE
// the server reports, so it differs from every message whose whole text is
// the block and from one with the same block and another size. Another body
// of the same size under one header block is not told apart without fetching
// the body; the block holds the Received headers the receiving server added,
// which a sender cannot predict. The same mail reaching the mailbox by two
// routes differs in those headers and is two messages.
//
// Each message is parsed by package mime into a text part and, for other
// MIME parts, a PartsSchema part describing them. The attachment bytes are
// staged through the engine's adapter.AttachmentSink (package stage), up to
// stage.MaxAttachments attachments of stage.MaxAttachmentBytes each, and each
// staged attachment is named in an attachment part after the descriptors; a
// descriptor reports "staged" when its bytes are staged. Attachments past the
// bounds, or refused by the engine's attachment checks, stay described only,
// as do all of them when the engine has no blob store. A staging failure a
// retry may fix ends Run like a sink refusal, so the message is fetched again.
// A message over Config.MaxMessageBytes is fetched header only: it has no
// parts to describe or stage. The threading headers
// become envelope references (model.EmailHeaders.Normalize), Auto-Submitted
// marks automated mail, and MessageSchema metadata records the mailbox
// position and header fields.
//
// # History
//
// The adapter declares history (adapter.HistoryReader). It reads Config.Mailbox
// only, never the sent mailbox, in UID order, over a window judged by
// INTERNALDATE ([Since, Until)): UID SEARCH with SINCE and BEFORE widened by a
// day, refined on the fetched date. Each envelope is built by the receiver's
// own mapping, so an imported message and its live delivery share one event ID.
// Attachments are staged through adapter.HistoryRequest.Stager as the receiver
// stages them (same keys and bounds), so the message holds its bytes once
// whichever path brought it; without a stager they are described only.
// The cursor is base64url JSON {v, k, mailbox, uid_validity, last_uid,
// last_date} with k a hash of the read's scope. A read touches no receiver
// state. A UIDVALIDITY change between calls, or a message that cannot be
// delivered, is a gap. See history.go.
//
// # Provider state
//
// The adapter declares the state capabilities archive, move, read_state, flag
// and trash, all with scope account and the folder move shape. The verbs write
// to the mailbox: \Seen and \Flagged by STORE, and moves by MOVE (RFC 6851) or,
// without it, COPY followed by \Deleted and UID EXPUNGE of the one UID copied
// (UIDPLUS, RFC 4315). Retrieval stays read-only: each verb runs on a session
// of its own, selected read-write, and the receiver never selects read-write.
// Roles resolve from the special-use attributes (RFC 6154) of LIST. A move
// reports the COPYUID location of the copy as the new reference
// (adapter.Outcome.ReferenceChanges); a Message-ID reference is looked up only
// in the mailboxes the adapter keeps mail in or follows, so the location is
// what finds the message afterwards. A move is not idempotent: Reconcile looks
// a move up in its destination, and a flag change by reading the flags.
//
// The receiver follows the messages it delivered and reports a change as a
// conversation.state envelope: flags by CONDSTORE (RFC 7162) CHANGEDSINCE, or a
// periodic comparison without it, and a message that left the mailbox is looked
// up in the archive, trash, junk, retrieved and watched mailboxes to tell a move
// from an expunge. Reports cover one message and are never complete. See
// state_refs.go, state_write.go, state_reconcile.go and state_sync.go.
//
// # Mail that cannot be keyed to a sender
//
// A sender is keyed by its From address as model.NormalizeEmail accepts it: a
// bare address. Valid RFC 5322 mail can have a From that is not one (a quoted
// local part with a space, a group), or no From a parser can read. Such a
// message is not delivered under any other identity: a sender the adapter did
// not read from the message would be one anyone could forge or collide with.
// The receiver skips it, moves the cursor on and reports ObservationSkipped,
// once; nothing of it is stored in uma. Unlike package smtp (550 5.1.7 at SMTP
// time) and package httpmail (400), a pull-only receiver has nobody to tell.
//
// The observation names no message: ports.Observation never carries a provider
// identity, and the receiver adds none. The mailbox keeps the message, since
// retrieval never changes flags and never expunges. Resetting the cursor, or a
// UIDVALIDITY change, does not bring it in: the message is fetched again,
// fails alike and is skipped again. To find it, compare the mailbox with what
// was delivered: every delivered message records its UIDVALIDITY and UID in
// its MessageSchema metadata, so a UID of the mailbox that no delivered message
// carries, around the time of the observation, is a candidate (expunged
// messages, mail outside the initial synchronization window and copies
// dropped as redeliveries leave the same gap). Read it in a mail client. To
// have uma deliver it, an operator who vouches for the sender appends a copy
// whose From is a bare address; that copy is a new message with its own
// digest.
//
// # Start recipients
//
// The sender of an inbound message and the recipient of a start are one
// identity under one spelling: the address with its domain lowercased and its
// local part byte-exact (model.NormalizeEmail; RFC 5321 treats the domain as
// case-insensitive and reserves the local part's case to the receiving host).
// Factory.NormalizeRecipient (adapter.RecipientNormalizer) applies it before
// a start is checked, fingerprinted or stored, so a start to "Bob@EXAMPLE.org"
// claims the identity of a message from "Bob@example.org", the same operation
// key with either spelling is one operation, and the send goes to a route it
// accepts. Bob@ and bob@ stay two identities. Plus-addressing, provider dot
// rules and punycode are not folded. A recipient that is not a bare address
// (display name, angle brackets, a list, space) is refused at the call.
//
// # Sending
//
// Instance.Send composes a text/plain message to the route address with an
// operation-derived Message-ID and submits it through package smtp's
// Submission. Replies are definitive: 2xx is provider_accepted, 4xx
// transient, 5xx rejected, and no reply after the end of DATA is
// outcome_unknown, never resent. Config.Sent names the sent-copies mailbox:
// the adapter appends a copy after acceptance unless the provider saves one
// itself (SavedByProvider). Instance.Reconcile searches that mailbox for the
// Message-ID; without a match it reports a safe retry only when the provider
// saves every submitted message, and otherwise leaves the outcome unknown.
package imap
