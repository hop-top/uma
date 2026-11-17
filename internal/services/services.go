// Package services implements the application services a client exposes:
// configuration, conversations and messages with their authorized queries
// and cursors, work items and agent tasks, local commands committed through
// the operation ledger, and the services not implemented in this version.
package services

import "hop.top/uma/internal/engine"

// Configuration is the Configuration service of one client.
type Configuration struct{ c *engine.Client }

// NewConfiguration returns the Configuration service bound to c.
func NewConfiguration(c *engine.Client) Configuration { return Configuration{c} }

// Contacts is the Contacts service of one client.
type Contacts struct{ c *engine.Client }

// NewContacts returns the Contacts service bound to c.
func NewContacts(c *engine.Client) Contacts { return Contacts{c} }
