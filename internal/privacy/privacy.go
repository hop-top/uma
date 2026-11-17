// Package privacy is the Privacy service: authorized redaction of retained
// content into tombstones, and authorized exports of retained records into
// restricted artifacts read back through Open.
package privacy

import "hop.top/uma/internal/engine"

// Service is the Privacy service of one client.
type Service struct{ c *engine.Client }

// New returns the Privacy service bound to c.
func New(c *engine.Client) Service { return Service{c} }
