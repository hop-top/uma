package telegram

// Credential purposes, resolved through ports.Credentials under the
// channel's credential reference, always as a secret.
const (
	// CredentialPurposeAPI is the bot token BotFather issued. Telegram
	// takes it in the request path, never in a header.
	CredentialPurposeAPI = "telegram.messages.api" // #nosec G101 -- purpose name, not a credential
	// CredentialPurposeWebhook is the webhook's secret token, the
	// secret_token of setWebhook.
	CredentialPurposeWebhook = "telegram.messages.webhook" // #nosec G101 -- purpose name, not a credential
)
