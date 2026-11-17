// Package slack is the Slack adapter for uma (type key slack.chat). It
// talks to Slack over the host's injected ports.HTTPClient with the
// standard library, and the Socket Mode receiver's WebSocket with
// github.com/coder/websocket, which has no dependencies of its own. One
// channel is one workspace install of one app; the
// docs/adapters/slack.chat.md page of the repository is the reference.
//
// # Capabilities
//
//   - receive (experimental): Instance.Run is the Socket Mode receiver, for an
//     app with no public request URL. It opens a connection with
//     apps.connections.open and the app-level token, reads Slack's envelopes
//     over the WebSocket, hands each payload to the same parsing as the
//     request URL (events, interactions and slash commands) and acknowledges
//     an envelope once the sink recorded it, with the answer of a modal
//     submission or an options request as the acknowledgement's payload. A
//     refused envelope is never acknowledged. Envelopes are recorded one at
//     a time per conversation, in the order read, and up to eight
//     conversations at once, so a slow record holds back only its own
//     conversation's acknowledgements. Each hello reports Slack's count of
//     the app's connections (ObservationSocketConnections), and
//     ObservationSocketShared when another consumer of the app-level token
//     holds one and takes a share of the envelopes. A refresh (disconnect
//     warning or refresh_requested) opens the next connection before the old
//     one is closed, within Run; link_disabled ends it disabled; any other
//     end of a connection returns a retryable failure for the engine to
//     reconnect. The instance implements adapter.ReceiverReporter:
//     connected at each hello, not connected when the connection that said
//     the last one closes and when Run returns.
//   - receive_http: Instance.Handler serves the app's request URL. It
//     verifies X-Slack-Signature (v0 HMAC-SHA256 over the version, the
//     X-Slack-Request-Timestamp and the raw body) in constant time, within
//     a replay window, before it reads anything else, answers
//     url_verification with the challenge and maps Events API deliveries to
//     ingress: messages (and the app_mention of the same message, one
//     event), edits, deletions and reactions. The app's own messages are
//     never reported, messages of other bots are automated messages from
//     bot senders, and with Config.MentionsOnly a channel message that does
//     not mention the bot is dropped.
//     Interactions arrive at the same URL, signed alike: a button or select
//     (block_actions), a slash command and a modal submission become
//     interaction envelopes whose response token is the path of the
//     response_url on the host's Options.ResponseBaseURL, 30 minutes from the
//     request, and are acknowledged with an empty 200 after the sink recorded
//     them.
//     With Config.EvaluateInteractions a modal submission is first put to the
//     host's interaction validator and its field errors answer the modal
//     (response_action errors, nothing recorded), and the options request of
//     an external select (block_suggestion) is answered with the validator's
//     options, or an empty list: Slack takes both answers from the response
//     to the request, never later.
//   - respond, respond_follow_up: Instance.Respond posts to the response URL
//     the interaction's token names (a path on Options.ResponseBaseURL), as a
//     message, an ephemeral message or the replacement of the message the
//     control sits on. The bot token never goes there. A lost answer is not
//     reconciled: a response_url names no message to look up.
//   - extension_execute: Instance.Execute runs the effects of Factory.Extensions,
//     ExtensionViewsOpen, ExtensionViewsPush and ExtensionViewsUpdate, which
//     open, stack and change a modal with views.open, views.push and
//     views.update. The first two take the trigger_id of an interaction
//     (TriggerOf reads it from the recorded interaction) and the instant it
//     stops working, three seconds after Slack's request: an attempt past it
//     makes no call, and an answer lost after the request was written is
//     never retried, as the trigger_id is single-use. The view is checked
//     against what Slack's references state before any call
//     (Factory.ValidateAction at the call). A lost answer is not reconciled.
//
// Config.Receive picks one way of receiving for a channel, as Slack sends an
// app's payloads one way at a time: events_api withholds receive and
// socket_mode withholds receive_http, each reported unavailable with reason
// model.Disabled; empty offers both.
//
// A message is named by "<conversation ID>:<ts>": ts is unique per
// conversation only. Identities are users and bots in namespace
// slack.chat.user; rooms (RefKindRoom) and threads (RefKindThread) scope
// conversations for per-room and per-thread selection, and a user's route
// for per-contact selection. A message outside a thread roots the thread its
// replies join.
//
// Credentials come from ports.Credentials under the channel's credential
// reference, one purpose each, always as a secret: CredentialPurposeAPI
// (the bot token), CredentialPurposeSigning (the signing secret) and, for
// Socket Mode, CredentialPurposeSocket (the app-level token).
package slack
