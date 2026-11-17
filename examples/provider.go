package examples

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// VerifiedSource owns authentication and returns only verified events for its bound account.
// It acknowledges its provider only through Accepted, after checking the durable receipt.
type VerifiedSource interface {
	Next(context.Context) (model.Ingress, error)
	Accepted(context.Context, model.IngressReceipt) error
	Close(context.Context) error
}
type SourceFactory struct {
	Connect func(context.Context, model.Channel, adapter.Dependencies) (VerifiedSource, error)
}

func (SourceFactory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: "org.example.sample.inbound", ContractMajor: adapter.ContractMajor,
		Capabilities: []model.Capability{"receive"}, Events: []string{"message.received"},
		IdentityNamespaces: []string{"account-scoped"}, Maturity: "experimental",
		ConfigurationSchema: model.Extension{Name: "org.example.sample.inbound.config", Version: 1},
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
	}
}
func (SourceFactory) Validate(c model.Extension) []model.ValidationIssue {
	if c.Name != "org.example.sample.inbound.config" || c.Version != 1 {
		return []model.ValidationIssue{{Field: "configuration", Code: "schema_mismatch"}}
	}
	return nil
}
func (f SourceFactory) Open(ctx context.Context, c model.Channel,
	d adapter.Dependencies) (adapter.Instance, error) {
	source, err := f.Connect(ctx, c, d)
	if err != nil {
		return nil, err
	}
	return &InboundOnly{Source: source}, nil
}

type InboundOnly struct{ Source VerifiedSource }

func (*InboundOnly) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: "receive", Available: true}}, nil
}
func (r *InboundOnly) Close(ctx context.Context) error { return r.Source.Close(ctx) }
func (r *InboundOnly) Run(ctx context.Context, sink adapter.Sink) error {
	for {
		input, err := r.Source.Next(ctx)
		if err != nil {
			return err
		}
		receipt, err := sink.Accept(ctx, input)
		if err != nil {
			return err
		}
		if !receipt.Durable {
			return &model.Error{Code: model.Unavailable, SafeMessage: "durable acceptance required"}
		}
		if err := r.Source.Accepted(ctx, receipt); err != nil {
			return err
		}
	}
}

var _ adapter.Factory = SourceFactory{}
var _ adapter.Instance = (*InboundOnly)(nil)
var _ adapter.Receiver = (*InboundOnly)(nil)
