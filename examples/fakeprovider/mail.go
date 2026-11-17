package fakeprovider

import (
	"context"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// TypeMail is the type key of the mail variant: an email provider outside
// the maintained set. It declares receive and send, threads on RFC 5322
// headers and reports sender-chosen Message-IDs, exactly what a third
// party declares for its own email adapter.
const TypeMail model.TypeKey = "org.example.fake.mail"

// MailDomain is the domain of the Message-IDs the mail variant generates
// for what it sends: "<m1@mail.fake.example>", counting up per script.
const MailDomain = "mail.fake.example"

var mail = variant{
	typ: TypeMail,
	exchange: adapter.Exchange{
		MessageIDs: adapter.MessageIDsSender, Threading: adapter.ThreadingEmail,
	},
	caps:   []model.Capability{model.CapabilityReceive, model.CapabilitySend},
	events: []string{model.EventMessageReceived, model.EventDeliveryUpdated},
	open:   func(i *instance) adapter.Instance { return &Mail{i: i} },
	sent:   mailReferences,
}

// mailReferences reports a sent message by the Message-ID the mail variant
// generated for it, as the provider message ID and as its email message_id
// reference, so a reply naming it threads.
func mailReferences(ref model.ExternalRef) []model.ExternalRef {
	id := "<" + ref.ID + "@" + MailDomain + ">"
	return []model.ExternalRef{
		model.ProviderMessageRef(ref.Channel, id),
		{Channel: ref.Channel, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailMessageID, ID: id},
	}
}

// NewMail returns the factory of the mail variant.
func NewMail() *Factory { return newFactory(mail) }

// Email is one inbound email as the mail variant's provider hands it over.
type Email struct {
	EventID     string
	Destination string
	// From is the sender address, the identity in Namespace.
	From       string
	Headers    model.EmailHeaders
	Text       string
	OccurredAt time.Time
}

// MailIngress normalizes email received on channel ch the way an email
// adapter does: its normalized Message-ID is the provider message ID, and
// its threading headers become model.EmailHeaderNamespace references.
// Auto-Submitted mail is an automated message.
func MailIngress(ch model.ChannelRef, m Email) model.Ingress {
	n := m.Headers.Normalize(ch)
	kind := model.EnvelopeMessage
	if n.Automated {
		kind = model.EnvelopeAutomatedMessage
	}
	return model.Ingress{Call: model.Call{Correlation: model.CorrelationID("mail-" + m.EventID)}, Envelope: model.Envelope{
		EventID: m.EventID, MessageID: n.MessageID, Destination: m.Destination, Kind: kind,
		Sender:     model.ExternalRef{Channel: ch, Namespace: Namespace, Kind: "address", ID: m.From},
		Assurance:  "unverified",
		References: n.References,
		Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: m.Text}}},
		Audience:   model.Audience{Visibility: model.Public}, OccurredAt: m.OccurredAt.UTC(),
	}}
}

// Mail is the mail variant: receive and send.
type Mail struct{ i *instance }

// Capabilities reports receive and send.
func (x *Mail) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	return x.i.capabilities(ctx)
}

// Close closes the instance; it is idempotent.
func (x *Mail) Close(context.Context) error { return x.i.close() }

// Run delivers the script's queued ingress to sink.
func (x *Mail) Run(ctx context.Context, sink adapter.Sink) error { return x.i.run(ctx, sink) }

// Send sends a new message under the next scripted result. An applied send
// reports the Message-ID the variant generated for it.
func (x *Mail) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return x.i.send(ctx, r)
}
