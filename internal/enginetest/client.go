package enginetest

import (
	"hop.top/uma/internal/attachments"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/events"
	"hop.top/uma/internal/extensions"
	"hop.top/uma/internal/interactions"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/privacy"
	"hop.top/uma/internal/services"
	"hop.top/uma/internal/streams"
	"hop.top/uma/model"
)

// Client is an engine client with its services attached, the way the
// public API composes them.
type Client struct{ *engine.Client }

// For binds p on e, as Engine.For does.
func For(e *engine.Engine, p model.Principal) (Client, error) {
	services.RegisterHeld()
	c, err := e.For(p)
	if err != nil {
		return Client{}, err
	}
	return Client{c}, nil
}

func (c Client) Configuration() services.Configuration { return services.NewConfiguration(c.Client) }
func (c Client) Contacts() services.Contacts           { return services.NewContacts(c.Client) }
func (c Client) Conversations() services.Conversations { return services.NewConversations(c.Client) }
func (c Client) Labels() services.Labels               { return services.NewLabels(c.Client) }
func (c Client) Messages() services.Messages           { return services.NewMessages(c.Client) }
func (c Client) WorkItems() services.WorkItems         { return services.NewWorkItems(c.Client) }
func (c Client) Tasks() services.Tasks                 { return services.NewTasks(c.Client) }
func (c Client) Operations() operations.Service        { return operations.New(c.Client) }
func (c Client) Events() events.Service                { return events.New(c.Client) }
func (c Client) Attachments() attachments.Service      { return attachments.New(c.Client) }
func (c Client) Extensions() extensions.Service        { return extensions.New(c.Client) }
func (c Client) Interactions() interactions.Service    { return interactions.New(c.Client) }
func (c Client) Streams() streams.Service              { return streams.New(c.Client) }
func (c Client) Privacy() privacy.Service              { return privacy.New(c.Client) }
