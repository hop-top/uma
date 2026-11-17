package postgres

import "hop.top/uma/ports"

// UUIDv7 returns the default production ports.IDs: RFC 9562 version 7 UUIDs
// in canonical lowercase text, stamped by the system clock. It is
// ports.NewUUIDv7(ports.SystemClock{}); see ports.UUIDv7 for the ordering
// guarantees. It is safe for concurrent use.
func UUIDv7() ports.IDs { return ports.NewUUIDv7(ports.SystemClock{}) }
