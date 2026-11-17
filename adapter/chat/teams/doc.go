// Package teams is the Microsoft Teams adapter for uma (type key
// microsoft.teams). It talks to the Bot Framework over the host's injected
// ports.HTTPClient: the Bot Connector service posts activities to the
// bot's messaging endpoint, and the adapter answers through the Connector
// REST API. One channel is one bot registration (a single-tenant Microsoft
// App ID) in one Microsoft Entra tenant; the
// docs/adapters/microsoft.teams.md page of the repository is the reference.
//
// # Capabilities
//
//   - receive_http: Instance.Handler serves the messaging endpoint. It
//     verifies the Bot Connector token of every request before anything
//     reaches the sink: an RS256 JWT signed by a key of the Bot Framework's
//     OpenID metadata (read through the host's HTTP client, cached a day,
//     read again at most once a minute for an unknown key ID), issuer
//     https://api.botframework.com, audience the bot's App ID, valid within
//     five minutes of clock skew, its serviceurl claim the activity's
//     serviceUrl, its key endorsed for the msteams channel. Signature and
//     claim checks are github.com/golang-jwt/jwt/v5's. It maps messages
//     (and the bot's mentions), edits, deletions and reactions to ingress,
//     and a user's Adaptive Card Action.Execute (an adaptiveCard/action
//     invoke) to an action interaction, acknowledged with the invoke
//     response once the sink recorded it. The bot's own activities are
//     never reported; other bots are automated messages from bot senders.
//   - send, start_conversation: Instance.Send posts a message activity to
//     a channel (a new post), a channel thread, a group chat, or a user's
//     personal chat, which the Connector creates or returns first.
//   - edit, delete: the Connector's update and delete activity, on the
//     bot's own messages; Teams refuses any other.
//   - react: the bot's reaction, by the six reaction types Teams names.
//   - signal: a typing indicator.
//
// A message is named by "<conversation ID>|<activity ID>", the
// conversation without the ";messageid=" suffix of a channel thread.
// Identities are Teams accounts ("29:..." users, "28:..." bots) in
// namespace microsoft.teams.user; conversations (RefKindRoom) and channel
// threads (RefKindThread) scope conversations for per-room and per-thread
// selection, and a user's route for per-contact selection.
//
// The adapter does not mint its own credential: the host's ports.Credentials
// returns the Bot Connector access token (ports.OAuth2) under
// CredentialPurposeConnector, or the host passes a token source through
// Options.Tokens; package botauth is one, minting the token with the client
// credentials (secret or certificate) of the bot's app registration.
package teams
