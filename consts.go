package uma

// Authorized queries.
//
// A list query pages the store in its recording order (the store's insertion
// position, unique per record and unchanged by updates), authorizes every
// scanned item as the client's principal, and exposes only admitted items.
// Inaccessible items are skipped without trace: no totals, no placeholders.
// A page may be short or empty and still carry Next; Complete reports that
// the store found no further eligible item within this query.
//
// Pages are separate store reads, not one snapshot: an item recorded after
// a page was read appears on a later page, never twice, and items already
// paged past are never revisited. Each page reauthorizes, so a policy change
// takes effect on the next page.
const (
	// DefaultPageLimit is the page size of a ListRequest with Limit 0.
	DefaultPageLimit = 50
	// MaxPageLimit caps the page size; a larger Limit is reduced to it.
	MaxPageLimit = 100
)

// Events.List limits. ListEvents.Limit bounds the events one call scans,
// admitted or not, in one store read; it does not bound the events
// returned.
const (
	// DefaultEventListLimit is the scan size of a ListEvents with Limit 0.
	DefaultEventListLimit = 100
	// MaxEventListLimit caps the scan size; a larger Limit is reduced to it.
	MaxEventListLimit = 1000
)

// FingerprintVersion1 is the canonical operation fingerprint encoding this
// version writes. Changing the encoding or a command's field set requires a
// new version: a stored fingerprint is only compared with one of the same
// version.
const FingerprintVersion1 uint32 = 1

// WorkKindOperation is the work kind of outbound operation execution, the
// kind Runtime.Run executes.
const WorkKindOperation = "operation.execute"
