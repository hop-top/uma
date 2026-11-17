package conformance

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// editText is the content the mutation cases edit a message to, and
// reactEmoji the emoji they react with.
const (
	editText   = "conformance edit"
	reactEmoji = "👍"
)

// mutationKind is one message mutation: the capability that carries it.
type mutationKind struct {
	capability model.Capability
	name       string
}

var (
	kindEdit   = mutationKind{model.CapabilityEdit, "edit"}
	kindDelete = mutationKind{model.CapabilityDelete, "delete"}
	kindReact  = mutationKind{model.CapabilityReact, "react"}

	mutationKinds = []mutationKind{kindEdit, kindDelete, kindReact}
	mutationCaps  = []model.Capability{model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact}
)

func (e *adapterEnv) declares(c model.Capability) bool {
	return slices.Contains(e.desc.Capabilities, c)
}

// kinds returns the mutations the adapter declares, or those it does not.
func (e *adapterEnv) kinds(declared bool) []mutationKind {
	var out []mutationKind
	for _, k := range mutationKinds {
		if e.declares(k.capability) == declared {
			out = append(out, k)
		}
	}
	return out
}

// eachKind runs fn in a fresh environment for each of kinds.
func (e *adapterEnv) eachKind(t *testing.T, kinds []mutationKind, fn func(*testing.T, *adapterEnv, mutationKind)) {
	t.Helper()
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) { fn(t, newAdapterEnv(t, e.h), k) })
	}
}

// canReceive reports whether inbound events can reach the engine.
func (e *adapterEnv) canReceive() bool {
	if e.declares(model.CapabilityReceive) {
		return true
	}
	if !e.declares(model.CapabilityReceiveHTTP) {
		return false
	}
	_, ok := e.provider.(WebhookProvider)
	return ok
}

// mutationTarget is a message the provider holds, stored by the engine.
type mutationTarget struct {
	msg   model.Message
	conv  model.Conversation
	event InboundEvent
	// id is the provider message ID the engine stored.
	id string
}

// ref is the provider message reference mutations of the target carry.
func (g mutationTarget) ref() model.ExternalRef { return model.ProviderMessageRef(adapterChanA, g.id) }

func providerMessageID(m model.Message, ch model.ChannelRef) (string, bool) {
	for _, x := range m.External {
		if x.Channel == ch && x.Namespace == model.ProviderMessageNamespace && x.Kind == model.RefKindMessage {
			return x.ID, true
		}
	}
	return "", false
}

// newTarget loads the stored target m. An adapter declaring a mutation
// reports every message it holds by its provider message reference; one
// declaring none may hold messages without.
func (e *adapterEnv) newTarget(t *testing.T, m model.Message, ev InboundEvent) mutationTarget {
	t.Helper()
	id, ok := providerMessageID(m, adapterChanA)
	if !ok && len(e.kinds(true)) > 0 {
		t.Fatalf("message %s holds no provider message reference: %+v", m.Ref.ID, m.External)
	}
	conv, err := e.client.Conversations().Get(e.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "conformance-get"}, Ref: m.Ref.Conversation})
	if err != nil {
		t.Fatalf("Conversations.Get: %v", err)
	}
	return mutationTarget{msg: m, conv: conv, event: ev, id: id}
}

// sentTarget sends a reply and returns the message the provider accepted,
// with the worker started and idle.
func (e *adapterEnv) sentTarget(t *testing.T) mutationTarget {
	t.Helper()
	conv := e.outbound(t)
	ref := e.mustReply(t, conv.Ref, "op-target")
	e.start(t)
	op := e.op(t, ref)
	wantActApplied(t, op)
	if op.ResultMessage == nil {
		t.Fatalf("send %v stored no message", ref)
	}
	return e.newTarget(t, e.storedMessage(t, *op.ResultMessage), InboundEvent{})
}

// inboundTarget receives a message with visibility vis and returns it,
// with the worker started and idle. It skips the case when the provider
// cannot express the message or the adapter drops it.
func (e *adapterEnv) inboundTarget(t *testing.T, vis model.Visibility) mutationTarget {
	t.Helper()
	if !e.canReceive() {
		t.Skip("capabilities receive and receive_http not declared")
	}
	e.inbound(t)
	ev := e.message(adapterChanA, "target", "peer-1", "support")
	ev.Visibility = vis
	e.deliver(t, adapterChanA, ev)
	wantAcked(t, e.pump(t, adapterChanA, 0, ev))
	m := findMessage(e.messages(t, adapterInboxA), adapterChanA, ev.MessageID)
	if m == nil {
		t.Skip("adapter stored no message for the event")
	}
	g := e.newTarget(t, *m, ev)
	e.start(t)
	return g
}

// anyTarget returns a message uma sent when the adapter sends, else one it
// received.
func (e *adapterEnv) anyTarget(t *testing.T) mutationTarget {
	t.Helper()
	if e.declares(model.CapabilitySend) {
		return e.sentTarget(t)
	}
	return e.inboundTarget(t, "")
}

// update is an update event n of kind to the target, newer than every
// earlier one.
func (e *adapterEnv) update(g mutationTarget, kind InboundKind, n int) InboundEvent {
	ev := e.message(adapterChanA, "update-"+strconv.Itoa(n), "peer-1", "support")
	ev.MessageID, ev.Kind, ev.Text = g.id, kind, ""
	ev.SourceRevision, ev.OccurredAfter = strconv.Itoa(n+1), time.Duration(n)*time.Minute
	return ev
}

func (e *adapterEnv) storedMessage(t *testing.T, ref model.MessageRef) model.Message {
	t.Helper()
	m, err := e.client.Messages().Get(e.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "conformance-get"}, Ref: ref})
	if err != nil {
		t.Fatalf("Messages.Get: %v", err)
	}
	return m
}

func mutationCommand(id model.OperationID, rev model.Revision) model.Command {
	return model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + string(id))}, Operation: id, ExpectedRevision: rev}
}

// mutate submits mutation k of m under operation id, at m's revision.
func (e *adapterEnv) mutate(k mutationKind, m model.Message, id model.OperationID) (model.Acceptance, error) {
	switch k {
	case kindEdit:
		return e.client.Messages().Edit(e.ctx, model.EditMessage{Command: mutationCommand(id, m.Revision), Message: m.Ref, Content: textContent(editText)})
	case kindDelete:
		return e.client.Messages().Delete(e.ctx, model.DeleteMessage{Command: mutationCommand(id, m.Revision), Message: m.Ref})
	}
	return e.react(m, id, reactEmoji, false)
}

func (e *adapterEnv) react(m model.Message, id model.OperationID, emoji string, remove bool) (model.Acceptance, error) {
	return e.client.Messages().React(e.ctx, model.Reaction{Command: mutationCommand(id, ""), Message: m.Ref, Emoji: emoji, Remove: remove})
}

// mustMutate submits a mutation that must be queued with m untouched.
func (e *adapterEnv) mustMutate(t *testing.T, k mutationKind, m model.Message, id model.OperationID) model.OperationRef {
	t.Helper()
	a, err := e.mutate(k, m, id)
	return e.queued(t, a, err, m)
}

func (e *adapterEnv) queued(t *testing.T, a model.Acceptance, err error, m model.Message) model.OperationRef {
	t.Helper()
	if err != nil {
		t.Fatalf("mutation: %v", err)
	}
	if a.Operation.State != model.Queued {
		t.Fatalf("mutation state = %q, want queued", a.Operation.State)
	}
	if got := e.storedMessage(t, m.Ref); got.Revision != m.Revision {
		t.Fatalf("message changed at submission: %+v", got)
	}
	return a.Operation.Ref
}

// wantApplied requires k to have taken effect on m.
func wantApplied(t *testing.T, k mutationKind, m model.Message, applied bool) {
	t.Helper()
	var got bool
	switch k {
	case kindEdit:
		got = len(m.Content.Parts) == 1 && m.Content.Parts[0].Text == editText
	case kindDelete:
		got = m.Deleted
	default:
		got = slices.ContainsFunc(m.Reactions, func(r model.MessageReaction) bool { return r.Emoji == reactEmoji })
	}
	if got != applied {
		t.Fatalf("%s applied to the message = %v, want %v: %+v", k.name, got, applied, m)
	}
}

// wantUnchanged requires the stored message to be m as it was.
func (e *adapterEnv) wantUnchanged(t *testing.T, m model.Message) {
	t.Helper()
	got := e.storedMessage(t, m.Ref)
	if got.Revision != m.Revision || got.Deleted || !slices.EqualFunc(got.Content.Parts, m.Content.Parts, func(a, b model.Part) bool { return a.Text == b.Text }) {
		t.Fatalf("message revision %q, deleted %v, content %+v; want it untouched at revision %q", got.Revision, got.Deleted, got.Content.Parts, m.Revision)
	}
}

// messageChange is a message.updated event naming an operation.
type messageChange struct {
	change string
	event  model.Event
}

// changes returns the message.updated events naming op, in order, and
// fails when one carries content.
func (e *adapterEnv) changes(t *testing.T, op model.OperationRef) []messageChange {
	t.Helper()
	var out []messageChange
	e.view(t, func(r store.Reader) error {
		var after model.Cursor
		for {
			p, err := r.Events(e.ctx, model.Filter{Kinds: []string{model.EventMessageUpdated}}, after, 100)
			if err != nil {
				return err
			}
			for _, ev := range p.Items {
				var keys map[string]json.RawMessage
				var pl model.MessageUpdatedPayload
				if json.Unmarshal(ev.Payload.Data, &keys) != nil || json.Unmarshal(ev.Payload.Data, &pl) != nil {
					return fmt.Errorf("message.updated payload %s", ev.Payload.Data)
				}
				if _, ok := keys["content"]; ok {
					return fmt.Errorf("message.updated carries content: %s", ev.Payload.Data)
				}
				if pl.Operation != nil && *pl.Operation == op {
					out = append(out, messageChange{pl.Change, ev})
				}
			}
			if p.Complete {
				return nil
			}
			after = p.Next
		}
	})
	return out
}

func (e *adapterEnv) wantChanges(t *testing.T, op model.OperationRef, want ...string) {
	t.Helper()
	var got []string
	for _, c := range e.changes(t, op) {
		got = append(got, c.change)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("message.updated changes of %v = %v, want %v", op, got, want)
	}
}
