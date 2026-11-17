package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/agent/a2a"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store/memory"
)

const tenant model.TenantID = "demo"

const (
	mountPath  = "/a2a/"
	inboxName  = "agents"
	consumerID = "desk-executor"
)

var (
	// service is the principal the webhook receives as and the runtime
	// runs as; desk is the host actor that executes the work.
	service = model.Principal{Tenant: tenant, Actor: "a2a-service"}
	desk    = model.Principal{Tenant: tenant, Actor: "desk-executor"}
)

// options configure a host.
type options struct {
	// PublicURL is the absolute URL the endpoint is mounted at: the card's
	// interface URL.
	PublicURL string
	// Tokens maps bearer tokens to agent IDs.
	Tokens map[string]string
	// BlockingWait bounds a blocking SendMessage (zero: the adapter's 30s).
	BlockingWait time.Duration
	// PollInterval is how often a blocking call looks for its answer
	// (zero: the adapter's 250ms).
	PollInterval time.Duration
	// LateDelay is how long the executor takes over a "late" request,
	// which lets a blocking caller's wait run out.
	LateDelay time.Duration
}

// host is one engine with one a2a.messages channel and the executor that
// does the work.
type host struct {
	engine  uma.Engine
	exec    *executor
	handler http.Handler
}

// newHost composes the engine, configures the channel and its
// per-agent-context inbox, mounts the endpoint and opens the executor's
// subscription. Nothing runs until Run.
func newHost(ctx context.Context, o options) (*host, error) {
	factory, err := a2a.New(a2a.Options{Authenticate: bearerAuth(o.Tokens)})
	if err != nil {
		return nil, err
	}
	engine, err := uma.New(uma.Config{
		// Not durable: contexts, tasks and subscriptions are lost on
		// restart. Use a durable store outside a demo.
		Store: memory.New(memory.Options{}),
		// Allows everything. A real host decides per principal and
		// resource; the adapter authorizes nothing itself.
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true, ReasonCode: "demo"}, nil
		}),
		Adapters: []adapter.Factory{factory},
		Clock:    ports.SystemClock{},
		IDs:      ports.NewUUIDv7(ports.SystemClock{}),
		// No Blobs: a raw (file bytes) part is refused with -32603. Configure
		// a blob store to accept them.
	})
	if err != nil {
		return nil, err
	}
	h := &host{engine: engine}
	if err := h.setup(ctx, o); err != nil {
		return nil, errors.Join(err, engine.Close(ctx))
	}
	return h, nil
}

func (h *host) setup(ctx context.Context, o options) error {
	admin, err := h.engine.For(service)
	if err != nil {
		return err
	}
	cfg := admin.Configuration()
	acc, err := cfg.ConfigureChannel(ctx, model.ConfigureChannel{
		Command: command("configure-channel"),
		Channel: model.Channel{
			Ref: model.ChannelRef{Tenant: tenant}, Type: a2a.Type,
			Configuration: a2a.Configuration(channelConfig(o)),
		},
	})
	if err != nil {
		return err
	}
	channel, err := acc.Operation.Result.AsChannel()
	if err != nil {
		return err
	}
	// One conversation per context the adapter allocates; tasks live in
	// per-agent-context conversations only.
	if _, err := cfg.ConfigureInbox(ctx, model.ConfigureInbox{
		Command: command("configure-inbox"),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: tenant}, Channel: channel, Name: inboxName,
			Destinations: []string{inboxName}, Selection: model.Selection{Mode: model.ModePerAgentContext},
		},
	}); err != nil {
		return err
	}
	endpoint, err := h.engine.Runtime().Webhook(ctx, runtimeCall(), channel)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle(mountPath, endpoint)
	h.handler = mux

	client, err := h.engine.For(desk)
	if err != nil {
		return err
	}
	opened, err := client.Events().Open(ctx, model.OpenSubscription{
		Command: command("open-subscription"), Consumer: consumerID,
		Filter: model.Filter{Kinds: []string{model.EventMessageReceived, model.EventTaskUpdated}},
		Start:  model.StartBeginning, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		return err
	}
	h.exec = &executor{client: client, sub: opened.Subscription.Ref, lateDelay: o.LateDelay}
	return nil
}

// channelConfig is the channel's configuration: the card's content, the
// declared security scheme the host's bearerAuth enforces, and the
// blocking bounds.
func channelConfig(o options) a2a.Config {
	c := a2a.Config{
		Destination: inboxName, PublicURL: o.PublicURL,
		Name: "Desk agent", Description: "Echoes text, writes reports and holds tasks for cancel tests", Version: "0.1.0",
		Skills: []a2a.Skill{{
			ID: "desk", Name: "Desk", Description: "echo <text>, report <topic>, late <topic>, hold",
			Tags: []string{"demo"}, Examples: []string{"echo hello", "report Q3", "hold"},
		}},
		SecuritySchemes: map[string]json.RawMessage{
			"bearer": json.RawMessage(`{"httpAuthSecurityScheme":{"scheme":"Bearer"}}`),
		},
		SecurityRequirements: []map[string][]string{{"bearer": {}}},
	}
	if o.BlockingWait > 0 {
		c.BlockingWait = o.BlockingWait.String()
	}
	if o.PollInterval > 0 {
		c.PollInterval = o.PollInterval.String()
	}
	return c
}

// Handler serves the endpoint (the JSON-RPC methods and the Agent Card)
// under mountPath.
func (h *host) Handler() http.Handler { return h.handler }

// Run delivers queued replies and executes the work until ctx ends, then
// drains the delivery worker.
func (h *host) Run(ctx context.Context) error {
	worker := make(chan error, 1)
	go func() {
		worker <- h.engine.Runtime().Run(context.WithoutCancel(ctx), runtimeCall(),
			uma.WorkerOptions{Owner: "desk-worker", Limit: 10, Poll: 100 * time.Millisecond})
	}()
	err := h.exec.Run(ctx)
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return errors.Join(err, h.engine.Runtime().Drain(drainCtx, runtimeCall()), <-worker)
}

// Close closes the engine.
func (h *host) Close(ctx context.Context) error { return h.engine.Close(ctx) }

func command(op model.OperationID) model.Command {
	return model.Command{Call: model.Call{Correlation: model.CorrelationID(op)}, Operation: op}
}

func runtimeCall() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: service, Call: model.Call{Correlation: "desk-runtime"}}
}
