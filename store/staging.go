package store

import (
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// StagingIntent records a blob key the engine is about to stage bytes
// under, before any byte is written, so a blob that no record binds can be
// found and deleted after a crash. The owner's commit binds the intent in
// the unit that stores the key in its record; cleanup and the retention
// sweep clear it. Exactly one of Attachment and Export names the owner.
type StagingIntent struct {
	// Key is the blob key, in the unit's tenant.
	Key ports.BlobKey `json:"key"`
	// Attachment is the attachment the bytes are staged for.
	Attachment model.AttachmentRef `json:"attachment"`
	// Export is the export artifact the bytes are staged for.
	Export model.ExportRef `json:"export"`
	// Abandoned is set by AbandonStaging: the sweep owns the key, and
	// BindStaging refuses it.
	Abandoned bool `json:"abandoned,omitempty"`
}

// StagingSweep asks AbandonStaging for up to Limit intents recorded
// before Before. Limit must be positive.
type StagingSweep struct {
	Before time.Time
	Limit  int
}

// StagingBatch reports one AbandonStaging call: the intents it abandoned,
// in recording order, and whether no further intent is eligible.
type StagingBatch struct {
	Intents  []StagingIntent
	Complete bool
}
