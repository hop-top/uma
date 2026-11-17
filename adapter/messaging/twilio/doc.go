// Package twilio holds the pieces the Twilio messaging adapters share: the
// X-Twilio-Signature check for webhooks, a small Programmable Messaging REST
// client that runs on an injected ports.HTTPClient, and the mapping of
// Twilio answers and message statuses to uma outcomes. The adapters live in
// subpackages: sms (type key twilio.sms) and whatsapp (type key
// twilio.whatsapp).
//
// Nothing here reads the environment, keeps global state or registers at
// init. Credentials arrive per call as ports.Credential values and every
// request runs under the caller's context with a deadline.
package twilio
