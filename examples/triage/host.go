package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store/memory"
)

// Actors of the host.
const (
	serviceActor model.ActorID = "triage-service"
	setupActor   model.ActorID = "triage-setup"
	triageActor  model.ActorID = "triage"
)

// grants is the host's policy: a fixed action list per actor. The triage
// actor may change the account's own view of a conversation
// (conversation.state_account) and nothing shared: a provider that reports a
// state verb as shared scope (a chat room, a ticket) is refused for it.
var grants = map[model.ActorID][]string{
	serviceActor: {model.ActionChannelReceive},
	setupActor: {
		model.ActionChannelRead, model.ActionChannelConfigure,
		model.ActionInboxRead, model.ActionInboxConfigure,
	},
	triageActor: {
		model.ActionInboxRead, model.ActionConversationRead, model.ActionMessageRead,
		model.ActionOperationRead, model.ActionConversationStateAccount,
	},
}

type roles struct{ tenant model.TenantID }

func (r roles) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	if a.Principal.Tenant != r.tenant || a.Resource.Tenant != r.tenant {
		return policy.Decision{ReasonCode: "tenant"}, nil
	}
	for _, action := range grants[a.Principal.Actor] {
		if action == a.Action {
			return policy.Decision{Allow: true, PolicyVersion: "roles/v1"}, nil
		}
	}
	return policy.Decision{ReasonCode: "not_granted", PolicyVersion: "roles/v1"}, nil
}

// Mailbox is one provider account the host triages: a channel and the inbox
// its mail lands in.
type Mailbox struct {
	// Name labels the mailbox in reports.
	Name string
	// Type and Configuration are the adapter's: google.mail or imap.mail.
	Type          model.TypeKey
	Configuration model.Extension
	// CredentialRef names the credential the host's port resolves.
	CredentialRef string
	// Destination is the inbox destination inbound mail is delivered to: the
	// mailbox address for both adapters.
	Destination string
}

// Config is everything the host needs.
type Config struct {
	Tenant    model.TenantID
	Mailboxes []Mailbox
	// Adapters are the factories of the mailboxes' types.
	Adapters    []adapter.Factory
	Credentials ports.Credentials
	// HTTP is the client of HTTP adapters (google.mail); nil is unset.
	HTTP ports.HTTPClient
	Rule Rule
	// Clock and IDs default to the system clock and UUIDv7.
	Clock ports.Clock
	IDs   ports.IDs
}

// Host is one triage process: an engine over an in-memory store, a receiver
// per mailbox and the sweep. Memory loses its conversations on exit, so a
// restart fetches the mailbox's recent mail again and sweeps skip what the
// provider already shows.
type Host struct {
	cfg     Config
	engine  uma.Engine
	client  uma.Client
	service uma.RuntimeCall
	inboxes map[string]model.InboxRef
	chans   map[string]model.ChannelRef

	cancel context.CancelFunc
	rx     sync.WaitGroup
}

// Start configures a channel and an inbox per mailbox and starts a receiver
// on each channel; ctx bounds the receivers.
func Start(ctx context.Context, cfg Config) (*Host, error) {
	if cfg.Rule == nil {
		cfg.Rule = DefaultRule
	}
	if cfg.Clock == nil {
		cfg.Clock = ports.SystemClock{}
	}
	if cfg.IDs == nil {
		cfg.IDs = ports.NewUUIDv7(cfg.Clock)
	}
	engine, err := uma.New(uma.Config{
		Store:       memory.New(memory.Options{Clock: cfg.Clock}),
		Authorizer:  roles{tenant: cfg.Tenant},
		Adapters:    cfg.Adapters,
		Credentials: cfg.Credentials,
		HTTP:        cfg.HTTP,
		Clock:       cfg.Clock,
		IDs:         cfg.IDs,
		Retry:       uma.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: time.Minute},
	})
	if err != nil {
		return nil, err
	}
	h := &Host{
		cfg: cfg, engine: engine,
		service: uma.RuntimeCall{Principal: model.Principal{Tenant: cfg.Tenant, Actor: serviceActor}, Call: model.Call{Correlation: "triage"}},
		inboxes: map[string]model.InboxRef{}, chans: map[string]model.ChannelRef{},
	}
	if h.client, err = engine.For(model.Principal{Tenant: cfg.Tenant, Actor: triageActor}); err != nil {
		return nil, errors.Join(err, engine.Close(ctx))
	}
	if err := h.setup(ctx); err != nil {
		return nil, errors.Join(err, engine.Close(ctx))
	}
	run, cancel := context.WithCancel(context.WithoutCancel(ctx)) // #nosec G118 -- Close calls h.cancel
	h.cancel = cancel
	for name, ch := range h.chans {
		h.rx.Add(1)
		go func() {
			defer h.rx.Done()
			// Receive returns when the host drains or run ends.
			_ = h.engine.Runtime().Receive(run, h.service, ch, uma.ReceiveOwner("triage-"+name))
		}()
	}
	return h, nil
}

// setup configures one channel and one inbox per mailbox. A mail thread is
// a conversation, so every inbox selects per contact without locking to one.
func (h *Host) setup(ctx context.Context) error {
	admin, err := h.engine.For(model.Principal{Tenant: h.cfg.Tenant, Actor: setupActor})
	if err != nil {
		return err
	}
	cfg := admin.Configuration()
	lock := false
	for _, mb := range h.cfg.Mailboxes {
		ch, err := cfg.ConfigureChannel(ctx, model.ConfigureChannel{
			Command: model.Command{Call: model.Call{Correlation: "setup"}, Operation: model.OperationID("setup/channel/" + mb.Name)},
			Channel: model.Channel{
				Ref:  model.ChannelRef{Tenant: h.cfg.Tenant},
				Type: mb.Type, Configuration: mb.Configuration, CredentialRef: mb.CredentialRef,
			},
		})
		if err != nil {
			return fmt.Errorf("channel %s: %w", mb.Name, err)
		}
		h.chans[mb.Name] = model.ChannelRef{Tenant: h.cfg.Tenant, ID: ch.Operation.Result.ID}
		in, err := cfg.ConfigureInbox(ctx, model.ConfigureInbox{
			Command: model.Command{Call: model.Call{Correlation: "setup"}, Operation: model.OperationID("setup/inbox/" + mb.Name)},
			Inbox: model.Inbox{
				Ref:     model.InboxRef{Tenant: h.cfg.Tenant},
				Channel: h.chans[mb.Name], Name: mb.Name, Destinations: []string{mb.Destination},
				Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
			},
		})
		if err != nil {
			return fmt.Errorf("inbox %s: %w", mb.Name, err)
		}
		h.inboxes[mb.Name] = model.InboxRef{Tenant: h.cfg.Tenant, ID: in.Operation.Result.ID}
	}
	return nil
}

// Channel returns the channel of the named mailbox.
func (h *Host) Channel(name string) model.ChannelRef { return h.chans[name] }

// Outcome is one queued change after the workers ran.
type Outcome struct {
	Mailbox string
	Action  Action
	// State is the operation's delivery state: "provider_accepted" or
	// "confirmed" once the provider applied it.
	State model.DeliveryState
	// Failure is the provider's refusal, when it refused.
	Failure *model.Error
}

// Pass runs one triage pass: a sweep of every mailbox, then the queued
// operations, then a report of how each ended. A change settles
// Conversation.ProviderState only once the provider applied it, so the
// report comes from the operations and the next sweep sees the settled state.
func (h *Host) Pass(ctx context.Context) ([]Outcome, error) {
	var queued []Outcome
	for _, mb := range h.cfg.Mailboxes {
		acts, err := Sweep(ctx, h.client, h.inboxes[mb.Name], h.cfg.Rule)
		for _, a := range acts {
			queued = append(queued, Outcome{Mailbox: mb.Name, Action: a})
		}
		if err != nil {
			return queued, fmt.Errorf("sweep %s: %w", mb.Name, err)
		}
	}
	// Step runs one bounded claim round in this goroutine; zero means no
	// work is due.
	for {
		n, err := h.engine.Runtime().Step(ctx, h.service, uma.WorkerOptions{Owner: "triage-pass", Limit: 10})
		if err != nil {
			return queued, err
		}
		if n == 0 {
			break
		}
	}
	for i := range queued {
		op, err := h.client.Operations().Get(ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "triage"}, Ref: queued[i].Action.Operation})
		if err != nil {
			return queued, err
		}
		queued[i].State, queued[i].Failure = op.State, op.Failure
	}
	return queued, nil
}

// Conversations lists every conversation of the named mailbox, for reports
// and tests.
func (h *Host) Conversations(ctx context.Context, name string) ([]model.Conversation, error) {
	var out []model.Conversation
	var after model.Cursor
	for {
		p, err := h.client.Conversations().List(ctx, model.ListRequest{
			Call:   model.Call{Correlation: "triage"},
			Filter: model.Filter{Inboxes: []model.InboxRef{h.inboxes[name]}},
			Page:   model.PageRequest{Limit: uma.MaxPageLimit, After: after},
		})
		if err != nil {
			return out, err
		}
		out = append(out, p.Items...)
		if p.Complete || p.Next == "" {
			return out, nil
		}
		after = p.Next
	}
}

// Close stops the receivers and closes the engine.
func (h *Host) Close(ctx context.Context) error {
	err := h.engine.Runtime().Drain(ctx, h.service)
	h.cancel()
	h.rx.Wait()
	return errors.Join(err, h.engine.Close(ctx))
}
