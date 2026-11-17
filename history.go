package uma

import (
	"time"

	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

// Import limits (ImportRequest.Limit): the envelopes one Runtime.Import
// call reads, imported or skipped.
const (
	DefaultImportLimit = runtime.DefaultImportLimit
	MaxImportLimit     = runtime.MaxImportLimit
)

// ImportRequest is one bounded history import (Runtime.Import): the past
// messages of a channel, or of one conversation, that occurred at or after
// Since and before Until. Exactly one of Channel and Conversation is set.
//
// Until is the window's exclusive upper bound, judged by the time the
// provider reports for the message (for mail, when it was received); the
// zero time leaves the window open. It must be after Since
// (model.Validation otherwise). The engine passes it to the adapter and
// does not check envelopes against it, as it does not check Since.
//
// Requester is the actor the import runs for, such as the desk user who
// asked for it; it must hold model.ActionChannelImport on the channel or
// the conversation, and each imported message records it as
// model.Message.ImportedBy. After resumes a previous call where its report's Next
// stopped. Limit caps the envelopes the call reads: DefaultImportLimit when
// 0, at most MaxImportLimit.
type ImportRequest struct {
	Requester    model.ActorID
	Channel      *model.ChannelRef
	Conversation *model.ConversationRef
	Since        time.Time
	Until        time.Time
	After        string
	Limit        int
}

// ImportReport is what one Runtime.Import call did. Imported counts the
// messages it recorded and Skipped the envelopes it left out: messages
// already held, envelopes that are no new message or carry no provider
// message ID, and messages outside the conversation's inbox; for an adapter
// whose message IDs the sender chooses, as mail does, a message is never
// skipped for its ID, and an envelope with neither a message ID nor an
// event ID is skipped. Gaps counts the pages the provider marked as missing
// part of the window. Next resumes the import (ImportRequest.After); it is
// empty and Complete is true once the window holds no more. When the
// adapter's reader fails, Import returns its error as it is, RetryAfter
// included, with a report whose Next is the cursor of the page that
// failed; Import does not retry.
type ImportReport struct {
	Imported int
	Skipped  int
	Gaps     int
	Next     string
	Complete bool
}
