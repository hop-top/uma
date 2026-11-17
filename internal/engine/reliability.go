package engine

import "time"

// RetentionPolicy sets how long each record family is kept before a purge
// deletes it. Its fields mirror the public policy one for one; the public
// New converts. There are no defaults: a purge requires every field.
type RetentionPolicy struct {
	Events     time.Duration
	Operations time.Duration
	Ingress    time.Duration
	Attempts   time.Duration
	Staging    time.Duration
	Exports    time.Duration
	Contacts   time.Duration
}

func (p RetentionPolicy) fields() []struct {
	name string
	d    time.Duration
} {
	return []struct {
		name string
		d    time.Duration
	}{
		{"Retention.Events", p.Events},
		{"Retention.Operations", p.Operations},
		{"Retention.Ingress", p.Ingress},
		{"Retention.Attempts", p.Attempts},
		{"Retention.Staging", p.Staging},
		{"Retention.Exports", p.Exports},
		{"Retention.Contacts", p.Contacts},
	}
}

func (p RetentionPolicy) validate() error {
	for _, f := range p.fields() {
		if f.d < 0 {
			return ConfigError(f.name, "must not be negative")
		}
	}
	return nil
}

// Required fails with model.Validation naming the first unset horizon.
func (p RetentionPolicy) Required() error {
	for _, f := range p.fields() {
		if f.d == 0 {
			return ConfigError(f.name, "required to purge; there is no default horizon")
		}
	}
	return nil
}

// WorkPolicy bounds background work. Its fields mirror the public policy
// one for one; the public New converts. A nil policy takes the defaults; an
// explicit one needs every field positive.
type WorkPolicy struct {
	// MaxClaims is how many claims of one work item may end without the
	// item completing before its next claim quarantines it.
	MaxClaims int
	// MaxInFlight caps, per work kind and tenant, the items claimed at once
	// across every worker sharing the store.
	MaxInFlight int
}

// Work policy defaults.
const (
	// DefaultMaxClaims: ten unfinished claims ride out crashes and lease
	// expiries of a healthy fleet, yet stop a poison item after ten wasted
	// claims.
	DefaultMaxClaims = 10
	// DefaultMaxInFlight: sixteen claimed items per kind and tenant, sixteen
	// workers at the default claim limit of one, bounds concurrent provider
	// calls without idling a small fleet.
	DefaultMaxInFlight = 16
)

// workPolicy validates an explicit policy or returns the defaults.
func workPolicy(p *WorkPolicy) (WorkPolicy, error) {
	if p == nil {
		return WorkPolicy{MaxClaims: DefaultMaxClaims, MaxInFlight: DefaultMaxInFlight}, nil
	}
	switch {
	case p.MaxClaims <= 0:
		return WorkPolicy{}, ConfigError("Work.MaxClaims", "must be positive")
	case p.MaxInFlight <= 0:
		return WorkPolicy{}, ConfigError("Work.MaxInFlight", "must be positive")
	}
	return *p, nil
}

// Retention returns the configured retention horizons, possibly unset.
func (e *Engine) Retention() RetentionPolicy { return e.retention }

// Work returns the work bounds, defaults filled.
func (e *Engine) Work() WorkPolicy { return e.work }
