package imessagegateway_test

import (
	"context"
	"net/http"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store/memory"
)

// The host side of an iMessage channel through the gateway, as
// docs/adapters/relay.messages.md ("Set up an iMessage channel") shows it.
// The end-to-end tests run it, so the page's snippet compiles and works.

// gatewaySecrets is the host's credential port for the channel:
// relay.webhook verifies what the gateway posts, relay.callback is what uma
// presents to the gateway's callback and lookup.
type gatewaySecrets struct{ webhook, callback []byte }

func (s gatewaySecrets) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	switch r.Purpose {
	case relay.PurposeWebhook:
		return ports.Secret{Bytes: s.webhook}, nil
	case relay.PurposeCallback:
		return ports.Secret{Bytes: s.callback}, nil
	}
	return nil, model.NewError(model.Unauthorized, "no credential for this purpose")
}

// newEngine registers the relay adapter. CallbackHosts names the gateway's
// host exactly as its callback URL does; AllowHTTP is for a gateway on a
// private network or on localhost only. Timeout covers a backend that
// answers only once Messages sent the message (BlueBubbles).
func newEngine(secrets ports.Credentials, gatewayHost string, ids ports.IDs) (uma.Engine, error) {
	factory := relay.NewFactory(relay.Options{
		CallbackHosts: []string{gatewayHost},
		AllowHTTP:     true,
		Timeout:       time.Minute,
	})
	return uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: ports.SystemClock{}}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{factory},
		Credentials: secrets,
		// A client that follows no redirect: the callback credential goes
		// to the configured URL only.
		HTTP: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		Clock: ports.SystemClock{},
		IDs:   ids,
	})
}

// imessageChannel is the channel configuration: medium imessage, the
// gateway's HMAC signature with a signed timestamp, replays refused, and
// the gateway's callback and lookup.
func imessageChannel(gatewayURL string) relay.Config {
	return relay.Config{
		Medium: relay.MediumIMessage,
		Auth: relay.Auth{
			Scheme:    "hmac",
			Header:    "X-Webhook-Signature",
			Timestamp: relay.AuthTimestamp{Header: "X-Webhook-Timestamp"},
			Replay:    relay.AuthReplay{Source: "signature"},
		},
		Callback: &relay.Callback{
			URL:       gatewayURL + "/uma/reply",
			LookupURL: gatewayURL + "/uma/lookup",
		},
	}
}

// configureIMessage stores the channel and a per-contact inbox bound to the
// address the gateway reports as "to" (the line's number, or the Apple ID
// the Mac sends from), and returns the handler to mount where the gateway
// posts (its uma webhook URL).
func configureIMessage(ctx context.Context, engine uma.Engine, host model.Principal, gatewayURL, address string) (model.ChannelRef, model.InboxRef, http.Handler, error) {
	cmd := func(op model.OperationID) model.Command {
		return model.Command{Call: model.Call{Correlation: model.CorrelationID(op)}, Operation: op}
	}
	admin, err := engine.For(host)
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	tenant := host.Tenant
	ch, err := admin.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{
		Command: cmd("imessage-channel"),
		Channel: model.Channel{
			Ref: model.ChannelRef{Tenant: tenant}, Type: model.TypeRelayMessages,
			CredentialRef: "imessage-gateway", Configuration: imessageChannel(gatewayURL).Extension(),
		},
	})
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	channel, err := ch.Operation.Result.AsChannel()
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	lock := true
	ib, err := admin.Configuration().ConfigureInbox(ctx, model.ConfigureInbox{
		Command: cmd("imessage-inbox"),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: tenant}, Channel: channel, Name: "imessage",
			Destinations: []string{address},
			Selection: model.Selection{
				Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1,
			},
		},
	})
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	inbox, err := ib.Operation.Result.AsInbox()
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	webhook, err := engine.Runtime().Webhook(ctx, uma.RuntimeCall{
		Principal: host, Call: model.Call{Correlation: "imessage-webhook"},
	}, channel)
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	return channel, inbox, webhook, nil
}
