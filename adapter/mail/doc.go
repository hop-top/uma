// Package mail groups the mail adapters for uma. Package smtp is the
// smtp.mail adapter (MX receiver and relay submission) and package imap the
// imap.mail adapter (IMAP retrieval, SMTP submission). The shared
// authentication used by them and by the provider-API mail adapters lives in
// package auth, shared MIME parsing and composition in package mime, the
// recipients of a reply (reply to all, Cc, Bcc) in package recipients and the
// provider mailbox list and create extensions in package mailboxes.
package mail
