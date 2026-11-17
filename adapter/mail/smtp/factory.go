package smtp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"sync/atomic"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/recipients"
	"hop.top/uma/model"
)

// Resolver answers the DNS queries of sender evidence: SPF (TXT, MX, A/AAAA,
// PTR), DKIM keys and DMARC records (TXT). *net.Resolver implements it. A
// name that does not exist is a *net.DNSError with IsNotFound set; a
// temporary failure is a net.Error whose Temporary reports true.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// Options are the host-supplied parts of the adapter.
type Options struct {
	// Listen returns the listener the receiver of channel serves for one Run;
	// Run closes it. Nil leaves receive unavailable.
	Listen func(ctx context.Context, channel model.Channel) (net.Listener, error)
	// TLS is the receiver's STARTTLS configuration; nil offers no STARTTLS.
	TLS *tls.Config
	// Resolver answers sender evidence lookups; nil uses net.DefaultResolver.
	Resolver Resolver
	// RootCAs verifies the relay certificate; nil uses the system roots.
	RootCAs *x509.CertPool
}

// Factory opens smtp.mail instances.
type Factory struct{ opts Options }

// NewFactory returns the smtp.mail factory.
func NewFactory(opts Options) *Factory {
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	return &Factory{opts: opts}
}

// Describe declares receive, send and start_conversation: a relay
// submits to any address, one no inbound mail named included.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                model.TypeSmtpMail,
		ContractMajor:       adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: ConfigSchema, Version: ConfigVersion},
		Capabilities:        []model.Capability{model.CapabilityReceive, model.CapabilitySend, model.CapabilityStartConversation, model.CapabilityReplyRecipients},
		IdentityNamespaces:  []string{Namespace},
		RequiredPermissions: []string{PurposeRelay},
		Maturity:            "experimental",
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsSender, Threading: adapter.ThreadingEmail},
	}
}

// NormalizeRecipient returns recipient with the domain of its mailbox
// address lowercased (adapter.RecipientNormalizer), the spelling inbound
// reports: a start to "Bob@EXAMPLE.org" claims the identity of a message
// from "Bob@example.org", and is sent to the route a send accepts. The local
// part stays byte-exact: RFC 5321 reserves its case to the receiving host, so
// Bob@ and bob@ are two mailboxes. Nothing else is folded: no
// plus-addressing, no punycode. An address that is not a bare addr-spec (a
// display name, angle brackets, a list, surrounding space) is refused. A
// reference of another namespace or kind is returned unchanged, for core to
// refuse.
func (f *Factory) NormalizeRecipient(recipient model.ExternalRef) (model.ExternalRef, error) {
	if recipient.Namespace != Namespace || recipient.Kind != RouteKind {
		return recipient, nil
	}
	id, err := model.NormalizeEmail(recipient.ID)
	if err != nil {
		return model.ExternalRef{}, model.NewError(model.Validation, "must be a bare mailbox address")
	}
	recipient.ID = id
	return recipient, nil
}

// Validate checks a channel configuration.
func (f *Factory) Validate(x model.Extension) []model.ValidationIssue {
	_, issues := ParseConfig(x)
	return issues
}

// Open opens an instance for channel. Nothing connects until Run or Send.
func (f *Factory) Open(_ context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	if ch.Type != model.TypeSmtpMail {
		return nil, model.NewError(model.Validation, "channel type is not "+string(model.TypeSmtpMail))
	}
	cfg, issues := ParseConfig(ch.Configuration)
	if len(issues) > 0 {
		return nil, &model.Error{Code: model.Validation, SafeMessage: "invalid configuration: " + issues[0].Field, Retry: model.NoRetry}
	}
	if deps.Clock == nil {
		deps.Clock = systemClock{}
	}
	return &Instance{ch: ch, cfg: cfg, deps: deps, opts: f.opts}, nil
}

// Instance is one opened smtp.mail channel.
type Instance struct {
	ch     model.Channel
	cfg    Config
	deps   adapter.Dependencies
	opts   Options
	closed atomic.Bool
}

// Capabilities reports receive (available when the host supplies a
// listener), and send and start_conversation (available with a
// credential port).
func (i *Instance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	recv := model.CapabilityStatus{Name: model.CapabilityReceive, Available: i.opts.Listen != nil}
	if !recv.Available {
		recv.Reason = model.Unavailable
	}
	send := model.CapabilityStatus{Name: model.CapabilitySend, Available: i.deps.Credentials != nil}
	if !send.Available {
		send.Reason = model.Unavailable
	}
	start := send
	start.Name = model.CapabilityStartConversation
	reply := send
	reply.Name = model.CapabilityReplyRecipients
	reply.Limits = recipients.Limits().Extension()
	return []model.CapabilityStatus{recv, send, start, reply}, nil
}

// Close marks the instance closed; it is idempotent. A running receiver
// stops with its context.
func (i *Instance) Close(context.Context) error {
	i.closed.Store(true)
	return nil
}

func (i *Instance) errClosed() error {
	return &model.Error{Code: model.Unavailable, SafeMessage: "instance closed", Retry: model.NoRetry}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func ipLiteral(s string) bool { return net.ParseIP(s) != nil }

var (
	_ adapter.Factory             = (*Factory)(nil)
	_ adapter.RecipientNormalizer = (*Factory)(nil)
	_ adapter.Receiver            = (*Instance)(nil)
	_ adapter.Sender              = (*Instance)(nil)
)
