// Package a2a is the a2a.messages adapter: uma as an A2A (Agent2Agent
// protocol, version 1.0) server endpoint. Remote agents send messages and
// read their tasks over JSON-RPC 2.0; the messages land in a
// per-agent-context inbox, where the host executes the work and answers
// through Messages.Reply and Tasks.Act. It uses the standard library and
// public uma packages only.
//
// uma is never the executor and never the client: it keeps protocol state
// and correlation. The adapter maps nothing in a message's content, parts
// or role to a service call or control command; its only write paths are
// Accept, StageAttachment and TaskSink.CancelTask.
//
// # Surfaces
//
// The host mounts the handler of Runtime.Webhook for the channel at any
// path. GET (or HEAD) of agent-card.json or .well-known/agent-card.json
// under the mount serves the Agent Card without authentication; a POST is
// a JSON-RPC request. The host owns the domain-root
// /.well-known/agent-card.json route, if it wants one.
//
// # Authentication
//
// The host authenticates every request through Options.Authenticate,
// required: New fails without it. The function sees the request and the
// channel and returns the verified Principal, the remote agent; a refusal is
// HTTP 401 with a WWW-Authenticate challenge naming the declared security
// schemes, a retryable *model.Error 503. It runs before the body is read.
// The adapter holds no token table and verifies nothing itself: the card's
// securitySchemes and securityRequirements are declarative channel
// configuration.
//
// The chain is: the host supplies authentication; the adapter translates it
// into the trusted principal (the envelope's Sender, namespace
// NamespacePrincipal, kind agent); core authorization still governs every
// access afterwards: the policy decision of the ingress, and the reads and
// cancel of the engine's task sink. An *http.Request never crosses into
// core. The agent identity is created with contact kind agent and is never
// folded into a person (the adapter declares SenderKinds [agent]); the A2A
// message role is never identity evidence.
//
// # Contexts and tasks
//
// A request names no context, a context, or a task:
//
//   - Neither: the adapter allocates a new opaque context ID (random, never
//     derived from the agent or the message) and the engine claims the
//     conversation under it. The ingress receipt's context is authoritative,
//     so a retry of the same messageId, which generates a context of its
//     own, recovers the first one and creates nothing.
//   - A contextId: it must be a context the principal owns, found through
//     TaskSink.Context before anything is accepted. An unknown context and
//     another agent's are the same -32602; a context is never created on
//     demand.
//   - A taskId: it must be a task of the principal (-32001 otherwise), not
//     terminal (-32004), and in the given context if both are given
//     (-32602). The message continues the task.
//
// A message opens no task by itself. The host opens one (Tasks.Act), or the
// request sets returnImmediately, which opens one with the message and
// answers with it at once. referenceTaskIds are kept as references on the
// message and used for nothing else. One task's end closes neither the
// context nor other tasks.
//
// Message IDs: the engine message ID derives from the agent and the client's
// messageId, so equal IDs of two agents never collide, and a retry within
// the engine's ingress horizon (RetentionPolicy.Ingress) is a duplicate.
// Past it a resend is a new message. returnImmediately and the JSON-RPC id
// are not part of the key.
//
// # Parts
//
// A text part is text. A data part becomes a structured part
// SchemaData; a url part a structured SchemaFileURL part, never fetched. A
// raw part is staged through the sink's AttachmentSink (the request is
// bounded by the engine's webhook body limit, which applies to the base64
// body). A part holding none or several of text, raw, url and data is
// -32602. Part metadata is ignored.
//
// Going out, text is text, SchemaData is data, SchemaFileURL a url part, any
// other structured part a data part naming its schema. The adapter has no
// read path to attachment bytes, so an attachment part is a data part naming
// the attachment ID (media type application/vnd.uma.attachment+json), never
// a raw part.
//
// # Blocking SendMessage
//
// By default SendMessage waits, up to the channel's blocking_wait (default
// 30s) or the caller's own deadline if shorter, polling the engine every
// poll_interval (default 250ms; the poll reads the store, so a worker in
// another process counts). It answers with the first of:
//
//   - a uma-authored reply to the message, if the message is bound to no
//     task: a Message response, no task;
//   - the message's task in a terminal or interrupted state, reported after
//     the message or opened by it: a Task response, a failed, canceled or
//     rejected task included.
//
// When the wait runs out the answer is JSON-RPC -32603 with a google.rpc
// ErrorInfo of reason DEADLINE_EXCEEDED, domain Options.ErrorDomain
// (default uma.hop.top), and metadata contextId, messageId and, only when a
// task exists, taskId. This is uma's documented mapping, not an A2A-defined
// code. It never returns a non-terminal task as success, creates no task
// and cancels nothing: the accepted message and any later task stay
// readable through GetTask, and resending the same messageId returns the
// first context.
//
// ErrorDomain names a stable, deployment-owned namespace for uma-defined
// error reasons; it never changes the errors the A2A specification defines,
// which keep the domain a2a-protocol.org.
//
// # Methods and errors
//
// SendMessage, GetTask, ListTasks and CancelTask are served. GetTask and
// ListTasks return tasks of the principal only, a task of another agent
// being absent. A history is returned only for historyLength above zero
// (newest messages, at most 100); a task's status message is shown when set.
// ListTasks needs a contextId when the agent has tasks in several inboxes;
// its totalSize is the size of the page, because uma counts nothing beyond
// it. CancelTask records a cancel request and returns the task in its
// current state: the host decides whether it reaches canceled; a terminal
// task is -32002.
//
//	SendStreamingMessage, SubscribeToTask, GetExtendedAgentCard   -32004
//	the four push notification config methods                      -32003
//	unknown method                                                 -32601
//	batch, malformed request, wrong jsonrpc                        -32600
//	malformed JSON                                                 -32700
//	A2A-Version header missing or not 1.0                          -32009
//	invalid params, unknown or foreign contextId                   -32602
//	unknown or foreign taskId                                      -32001
//	any other uma error                                            -32603
//
// Every error carries data as an array of @type objects. A policy denial
// and any other uma error are -32603 with an ErrorInfo whose reason is the
// uma error code in upper case, and reveal nothing about what was denied.
//
// # Replies
//
// Sender has no transport: a remote agent reads replies, status content and
// artifacts through the protocol and is never pushed to. Send reports the
// reply provider-accepted under a message reference derived from the
// attempt; delivery is never confirmed. Only public audiences are sent.
//
// # Configuration
//
// Configuration builds a channel configuration (schema a2a.messages.config,
// version 1): the inbox destination, public URL, card content, declared
// security schemes, and the two durations. The channel's inbox must be
// per-agent-context.
package a2a
