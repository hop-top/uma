// Package httpmail is the http.mail adapter: mail posted over HTTP by a
// relay. It takes mail from an inbound-parse service or an MTA hook and has
// nothing else: it reads no mailbox, serves no SMTP and sends nothing.
//
// # Ingress
//
// Instance.Handler is the webhook the host mounts (Runtime.Webhook). Every
// request is authenticated first, through package webhookauth and the
// configuration's Auth: a shared token, a bearer token, an HMAC or an
// Ed25519 signature, with the header names, encodings, signed layout,
// timestamp window and replay rejection of the sender already in the field.
// The credential comes from the host's ports.Credentials for purpose
// PurposeWebhook, on every request.
//
// The content type then picks the body format among the channel's Formats.
// FormatJSON (application/json) is one object per message. The members
// read, and their names, are Config.JSONFields; the defaults are
// {from, to, subject, body}, with message_id, in_reply_to, references, date
// and attachments as optional additions. A JSON attachment is a description,
// plus its bytes when the entry embeds them as base64. FormatRFC822 (message/rfc822) is the message itself, parsed
// by package mime, so the body, attachments and threading headers are the
// ones of the other mail adapters and its header fields are recorded. Its
// recipients are the To and Cc fields, or the address list of a request
// header the operator names (Config.RecipientHeader) when the relay knows
// the SMTP envelope.
//
// Each recipient address is matched against Config.Recipients and mapped to
// its destination; one envelope reaches the sink per destination. The
// message is answered 200 once the sink accepted it for every destination,
// a retryable sink failure is 503, and the delivery that failed is not a
// replay of itself.
//
// # Sender evidence
//
// uma verifies nothing a request carries. The From address is the sender's
// claim, so the source assurance of a message is AssuranceUnverified and
// the payload records no SPF, DKIM or DMARC result of its own. Payload
// members that look like evidence are ignored; the Authentication-Results
// and Received-SPF fields of a message are recorded as claims, as found.
//
// A channel can declare trust (Config.Evidence): requests that pass
// authentication come from a relay that evaluated the message itself and
// stamped an Authentication-Results value (RFC 8601) under a named
// authserv-id, in a header field of a FormatRFC822 body or in a named
// member of a FormatJSON body. Only the topmost value naming exactly that id
// is read, and only then do its results become evidence and its DMARC result,
// when it names the message's From domain, set the assurance as package smtp
// does for mail it received. The declaration is the operator's: it says the
// authenticated relay, not the message, is the evidence, and that the relay
// removes any stamp under its id that a sender supplied.
//
// A message is identified by its bytes, never by its Message-ID: the id is
// the sender's to choose, so it threads and nothing more.
//
// # Attachments
//
// The attachment bytes a request carries are staged through the engine's
// attachment sink, by package stage as the other mail adapters do: those of
// a FormatRFC822 body, and those a FormatJSON entry embeds as base64 (the
// AttachmentFields.Content member). At most stage.MaxAttachments attachments
// of stage.MaxAttachmentBytes each are staged, under a key derived from the
// message's event ID, so a redelivery restages the same ones. Each staged
// attachment is named in an attachment part and marked "staged" in its
// description; the rest are described only, as is everything when the sink
// cannot stage. A staging failure a retry can fix answers 503 like any other
// retryable sink failure. A URL in a JSON entry is a claim and is never
// fetched: the adapter makes no request on a sender's say.
//
// # Not in this adapter
//
// Outbound mail is not sent. Form-encoded bodies (multipart/form-data,
// application/x-www-form-urlencoded) are not read.
package httpmail
