package model

// EventRef identifies one committed event of a tenant's event log: the
// tenant and the event's ID (Event.ID), unique within the tenant. It is the
// argument of Events.Get. It is not a Ref: the resource kind KindEvent names
// the event log as a whole (EventLogRef), and an event is about the
// resource in its own Event.Resource.
type EventRef struct {
	Tenant TenantID `json:"tenant"`
	ID     ID       `json:"id"`
}

// NewEventRef returns a validated event ref.
func NewEventRef(t TenantID, id ID) (EventRef, error) {
	return newValid(EventRef{Tenant: t, ID: id})
}

// Validate checks tenant and ID.
func (r EventRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the tenant; an empty ID is allowed.
func (r EventRef) ValidateScope() error { return r.validate("", true) }

func (r EventRef) validate(path string, leafOptional bool) error {
	return checkTenantScoped(path, r.Tenant, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r EventRef) Scope() TenantID { return r.Tenant }

// IsZero reports whether r is unset.
func (r EventRef) IsZero() bool { return r == EventRef{} }
