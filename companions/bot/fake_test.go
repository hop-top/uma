package bot_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// Unit tests drive the dispatcher through a scripted client: only the
// services the dispatcher uses are implemented; any other call panics on
// the nil embedded interface.

const (
	tenant model.TenantID = "tenant"
	self   model.ActorID  = "bot"
	human  model.ActorID  = "ingress"
)

var (
	inbox = model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000a1"}
	sub   = model.SubscriptionRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000f1"}
)

func conv(n int) model.ConversationRef {
	return model.ConversationRef{Inbox: inbox, ID: model.ID("00000000-0000-7000-8000-0000000001" + pad(n))}
}

func pad(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 2 {
		s = "0" + s
	}
	return s
}

// journal records dispatcher calls in order, across services.
type journal struct {
	mu      sync.Mutex
	entries []string
}

func (j *journal) add(s string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, s)
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.entries...)
}

type fakeClient struct {
	uma.Client
	ev    *fakeEvents
	msgs  *fakeMessages
	convs *fakeConversations
	ints  *fakeInteractions
}

func (c *fakeClient) Events() uma.Events               { return c.ev }
func (c *fakeClient) Messages() uma.Messages           { return c.msgs }
func (c *fakeClient) Conversations() uma.Conversations { return c.convs }
func (c *fakeClient) Interactions() uma.Interactions   { return c.ints }

func newFake() *fakeClient {
	j := &journal{}
	return &fakeClient{
		ev:    &fakeEvents{j: j},
		msgs:  &fakeMessages{j: j, byRef: map[model.MessageRef]model.Message{}, ops: map[model.OperationID]model.Reply{}},
		convs: &fakeConversations{byRef: map[model.ConversationRef]model.Conversation{}},
		ints: &fakeInteractions{
			byRef: map[model.InteractionRef]model.Interaction{}, ops: map[model.OperationID]model.Respond{},
		},
	}
}

// fakeEvents serves queued batches like a lease: Poll returns the head
// batch until it is acknowledged.
type fakeEvents struct {
	uma.Events
	j       *journal
	mu      sync.Mutex
	batches []model.EventBatch
	acks    []model.Acknowledge
	ackErr  func() error
	pollErr func() error
	polls   int
}

func (e *fakeEvents) push(events ...model.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	token := "token-" + strconv.Itoa(len(e.batches)+len(e.acks)+1)
	e.batches = append(e.batches, model.EventBatch{Events: events, AckToken: token})
}

func (e *fakeEvents) Poll(ctx context.Context, req model.Poll) (model.EventBatch, error) {
	if err := ctx.Err(); err != nil {
		return model.EventBatch{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.polls++
	if e.pollErr != nil {
		if err := e.pollErr(); err != nil {
			return model.EventBatch{}, err
		}
	}
	if req.Subscription != sub {
		return model.EventBatch{}, model.NewError(model.NotFound, "subscription")
	}
	if len(e.batches) == 0 {
		return model.EventBatch{}, nil
	}
	return e.batches[0], nil
}

func (e *fakeEvents) Ack(_ context.Context, req model.Acknowledge) (model.SubscriptionResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ackErr != nil {
		if err := e.ackErr(); err != nil {
			return model.SubscriptionResult{}, err
		}
	}
	if len(e.batches) == 0 || e.batches[0].AckToken != req.Token {
		return model.SubscriptionResult{}, model.NewError(model.Conflict, "stale token")
	}
	e.j.add("ack " + req.Token)
	e.acks = append(e.acks, req)
	e.batches = e.batches[1:]
	return model.SubscriptionResult{}, nil
}

func (e *fakeEvents) acked() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.acks)
}

type fakeMessages struct {
	uma.Messages
	j        *journal
	mu       sync.Mutex
	byRef    map[model.MessageRef]model.Message
	ops      map[model.OperationID]model.Reply
	replies  []model.Reply
	getErr   error
	replyErr func() error
}

func (m *fakeMessages) Get(_ context.Context, req model.Lookup[model.MessageRef]) (model.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return model.Message{}, m.getErr
	}
	msg, ok := m.byRef[req.Ref]
	if !ok {
		return model.Message{}, model.NewError(model.NotFound, "message")
	}
	return msg, nil
}

func (m *fakeMessages) Reply(_ context.Context, req model.Reply) (model.Acceptance, error) {
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.replyErr != nil {
		if err := m.replyErr(); err != nil {
			return model.Acceptance{}, err
		}
	}
	m.j.add("reply " + string(req.Operation))
	m.replies = append(m.replies, req)
	if _, ok := m.ops[req.Operation]; ok {
		return model.Acceptance{Replayed: true}, nil
	}
	m.ops[req.Operation] = req
	return model.Acceptance{}, nil
}

func (m *fakeMessages) sent() []model.Reply {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]model.Reply(nil), m.replies...)
}

// distinct returns the number of distinct reply operations.
func (m *fakeMessages) distinct() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ops)
}

type fakeConversations struct {
	uma.Conversations
	mu    sync.Mutex
	byRef map[model.ConversationRef]model.Conversation
}

func (c *fakeConversations) Get(_ context.Context, req model.Lookup[model.ConversationRef]) (model.Conversation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.byRef[req.Ref]
	if !ok {
		return model.Conversation{}, model.NewError(model.NotFound, "conversation")
	}
	return v, nil
}

// message stores an inbound message and returns its message.received event.
func (c *fakeClient) message(t *testing.T, n int, in model.ConversationRef, text string) model.Event {
	t.Helper()
	id := model.ID("00000000-0000-7000-8000-0000000002" + pad(n))
	ref := model.MessageRef{Conversation: in, ID: id}
	c.msgs.mu.Lock()
	c.msgs.byRef[ref] = model.Message{
		Ref: ref, Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	}
	c.msgs.mu.Unlock()
	c.convs.mu.Lock()
	if _, ok := c.convs.byRef[in]; !ok {
		c.convs.byRef[in] = model.Conversation{Ref: in, Mode: model.ModePerRoom}
	}
	c.convs.mu.Unlock()
	data, err := json.Marshal(model.MessageReceivedPayload{Message: ref, Trigger: model.TriggerProvider})
	if err != nil {
		t.Fatal(err)
	}
	return model.Event{
		Version: model.EventEnvelopeVersion, ID: model.ID("00000000-0000-7000-8000-0000000003" + pad(n)),
		Kind: model.EventMessageReceived, Tenant: tenant, Inbox: inbox.ID,
		Resource: ref.Ref(), Actor: human, Correlation: model.CorrelationID("rx-" + pad(n)),
		Payload: model.Extension{Name: model.SchemaMessageReceived, Version: model.PayloadVersion1, Data: data},
	}
}

// direct marks a conversation as a direct exchange.
func (c *fakeClient) direct(in model.ConversationRef) {
	c.convs.mu.Lock()
	defer c.convs.mu.Unlock()
	c.convs.byRef[in] = model.Conversation{Ref: in, Mode: model.ModePerContact}
}

func text(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

// failing returns an error hook failing its first n calls with err and
// counting every call.
func failing(n int, err error) (func() error, *int) {
	calls := new(int)
	return func() error {
		*calls++
		if *calls <= n {
			return err
		}
		return nil
	}, calls
}

// pacer records Run and Step pauses without waiting.
type pacer struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (p *pacer) Wait(ctx context.Context, d time.Duration) error {
	p.mu.Lock()
	p.waits = append(p.waits, d)
	p.mu.Unlock()
	return ctx.Err()
}
