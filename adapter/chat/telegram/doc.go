// Package telegram is the Telegram adapter for uma (type key
// telegram.messages). It talks to the Telegram Bot API over the host's
// injected ports.HTTPClient with the standard library only. One channel is
// one bot; the docs/adapters/telegram.messages.md page of the repository is
// the reference.
//
// # Capabilities
//
//   - receive: Instance.Run reads updates with getUpdates (long polling),
//     resuming from the engine's cursor (the update_id of the last envelope
//     an update made). A webhook excludes it: Telegram answers a read with a
//     conflict (409) while one is set. The instance implements
//     adapter.ReceiverReporter: connected once a poll is answered, not
//     connected after a failed read and when Run returns.
//   - receive_http: Instance.Handler serves the bot's webhook. It verifies
//     the secret token in X-Telegram-Bot-Api-Secret-Token (through
//     adapter/web/webhookauth, in constant time) before it reads anything
//     else, and maps updates to ingress: messages and channel posts, their
//     edits, and reactions. The bot's own messages are never reported,
//     messages of other bots are automated messages from bot senders, and
//     with Config.MentionsOnly a message of a group, supergroup or channel
//     that does not address the bot is dropped. Media is described, never
//     fetched.
//   - send, start_conversation: Instance.Send posts with sendMessage to the
//     route: a chat, a forum topic of it, or a user (the private chat with
//     them, open only once the user started the bot). Text is literal and
//     sent under no parse mode; a markup part (MarkupSchema) is the host's
//     own HTML or MarkdownV2, with the text beside it escaped for its mode
//     and EscapeHTML and EscapeMarkdownV2 to compose user text into it; an
//     inline keyboard (KeyboardSchema) is passed through, checked. A lost
//     answer stays unknown: Telegram has no lookup to settle it.
//   - edit, delete, react: editMessageText, deleteMessage and
//     setMessageReaction on a message of the channel; a bot changes its own
//     messages, deletes within 48 hours, and holds one reaction, from the 73
//     emoji Telegram lists, so the descriptor declares single reaction
//     cardinality.
//   - respond: callback queries (the press of an inline keyboard button) and
//     bot commands are interactions, reported once the sink recorded them. A
//     response is a message, the notification the callback query shows, or
//     the update of the message the button sits on; answerCallbackQuery
//     clears the client's progress bar. A command has no callback query:
//     only a message answers it.
//
// The descriptor declares both ways of receiving, and Config.Receive picks a
// channel's: under ReceivePoll the instance reports receive_http disabled,
// under ReceiveWebhook receive (model.Disabled), so the engine runs only the
// selected one; empty reports both available, for the host to choose. The
// adapter never calls setWebhook or deleteWebhook.
//
// A message is named by "<chat ID>:<message_id>": message_id is unique per
// chat only. Identities are users and bots, and the chats that speak as
// themselves, in namespace telegram.messages.user; chats (RefKindRoom) and
// forum topics (RefKindThread) scope conversations for per-room and
// per-thread selection, and a user's route, the private chat with them, for
// per-contact selection.
//
// Credentials come from ports.Credentials under the channel's credential
// reference, one purpose each, always as a secret: CredentialPurposeAPI (the
// bot token) and CredentialPurposeWebhook (the webhook secret token).
package telegram
