// Package smtp is the smtp.mail adapter: an MX receiver for inbound mail and
// submission to a configured relay for outbound mail, on one channel per
// inbox.
//
// # Ingress
//
// Instance.Run serves SMTP on the listener the host's Options.Listen returns
// (the host owns port 25 and process privileges), with STARTTLS when
// Options.TLS is set and no AUTH. Each recipient is checked at RCPT against
// the configured recipient list and mapped to its destination; unknown
// recipients are refused with 550 5.1.1. The server enforces the message size
// and recipient count limits.
//
// Each accepted message is evaluated for sender evidence: SPF for the
// MAIL FROM (or HELO) identity, DKIM signatures and DMARC for the From
// domain, with DNS answered by Options.Resolver. The results are recorded as
// an Authentication-Results value and structured results in the message
// metadata, and the DMARC result sets the source assurance
// (AssuranceDMARCPass and friends). Evidence grades the sender; it never
// proves identity and never links contacts. ARC headers are recorded as
// found, unverified.
//
// The message is parsed (package mime) into a text part and, for other MIME
// parts, a structured PartsSchema part describing them. The attachment bytes
// are staged through the engine's adapter.AttachmentSink (package stage), up
// to stage.MaxAttachments attachments of stage.MaxAttachmentBytes each, in
// the inbox of the destination, and each staged attachment is named in an
// attachment part after the descriptors; a descriptor reports "staged" when
// its bytes are staged. Attachments past the bounds, or refused by the
// engine's attachment checks, stay described only, as do all of them when
// the engine has no blob store. A staging failure a retry may fix is a
// temporary refusal (451). The raw threading headers are normalized with
// model.EmailHeaders.Normalize into the envelope references, so ingress
// threads by In-Reply-To then References. A message whose Auto-Submitted
// value is not "no", or whose reverse path is null, is an automated message.
//
// A message is identified for deduplication by its destination and a digest
// of the DATA bytes, never by its Message-ID: the sender chooses that, so
// trusting it would let a forged message suppress a genuine one that reuses
// its id. The Message-ID threads only.
//
// A message is acknowledged (250) only after the sink accepted it for every
// destination. A destination bound to no inbox, or a disabled one, is a
// permanent refusal (550); every other sink failure is temporary (451), so
// the sending MTA keeps the message and retries.
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
// # Egress
//
// Instance.Send composes a text/plain message to the route address with a
// Message-ID derived from the operation (stable across attempts), threads a
// reply to the message it answers (ThreadOf: In-Reply-To, References and
// "Re:" subject from the effect's target and adapter.SendRequest.Thread),
// and submits it to the relay through package auth (TLS before AUTH,
// one credential resolution per connection, purpose PurposeRelay). Relay
// replies map to outcomes: 2xx after DATA is provider_accepted with the
// Message-ID as provider reference; 4xx is transient and retried; 5xx is
// rejected; a lost or timed-out reply after the end of DATA is
// outcome_unknown and never resent.
package smtp
