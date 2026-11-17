// Package mime parses inbound mail into the parts uma keeps and composes
// outbound mail. It is shared by the mail adapters (SMTP, IMAP and the
// provider-API adapters that handle raw RFC 5322 messages).
//
// Parse reads one message: the decoded header fields uma needs (addresses,
// subject, date and the raw threading headers), the text body and a
// descriptor of every other part. Text comes from the first text/plain part
// that is not an attachment, else from the first text/html part rendered as
// plain text. Charsets are decoded with the WHATWG encoding labels; nothing
// is registered globally, so importing the package changes no other
// package's behavior. Attachment bytes are hashed and counted; they are kept
// only when Limits asks for them, within a size and count bound. A part's
// filename is the sender's suggestion and reaches the descriptor reduced by
// SanitizeFilename.
//
// Compose writes a single-part text/plain UTF-8 message with the header
// fields the caller sets, including Message-ID, In-Reply-To and References.
package mime
