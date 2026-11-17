package telegram

import (
	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// HeaderSecretToken is the header Telegram puts the webhook's secret token
// in: "If you'd like to make sure that the webhook was set by you, you can
// specify secret data in the parameter secret_token. If specified, the
// request will contain a header X-Telegram-Bot-Api-Secret-Token with the
// secret token as content" (https://core.telegram.org/bots/api#setwebhook).
const HeaderSecretToken = "X-Telegram-Bot-Api-Secret-Token" // #nosec G101 -- header name, not a credential

// MinSecretBytes is the shortest webhook secret token the adapter accepts.
// Telegram takes 1 to 256 characters of A-Z, a-z, 0-9, "_" and "-"; a token
// of a few characters is guessable, and the header is the only proof a
// request comes from Telegram, so shorter ones are refused.
const MinSecretBytes = 16

// newVerifier is the check every webhook request passes: the secret token
// is the whole value of HeaderSecretToken, compared in constant time with
// the secret the host resolves on every request. Telegram sends no
// timestamp and no signature, so there is no freshness window to check, and
// an update's own ID is what makes a redelivery a duplicate.
func newVerifier(ch model.Channel, creds ports.Credentials, clock ports.Clock) (*webhookauth.Verifier, error) {
	return webhookauth.New(webhookauth.Config{
		Scheme: webhookauth.SchemeToken, Header: HeaderSecretToken, MinSecretBytes: MinSecretBytes,
	}, webhookauth.FromCredentials(creds, ports.CredentialRequest{
		Channel: ch.Ref, Reference: ch.CredentialRef, Purpose: CredentialPurposeWebhook,
		Accept: []ports.CredentialKind{ports.KindSecret},
	}), webhookauth.Deps{Clock: clock, Scope: string(ch.Ref.Tenant) + "/" + string(ch.Ref.ID)})
}
