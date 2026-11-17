package runtime

import (
	"context"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Import limits: the envelopes one Import call reads, imported or skipped.
const (
	DefaultImportLimit = 100
	MaxImportLimit     = 500
)

// ImportRequest is one bounded history import; see Import.
type ImportRequest struct {
	Requester    model.ActorID
	Channel      *model.ChannelRef
	Conversation *model.ConversationRef
	Since        time.Time
	Until        time.Time
	After        string
	Limit        int
}

// ImportReport is what one Import call did.
type ImportReport struct {
	Imported int
	Skipped  int
	Gaps     int
	Next     string
	Complete bool
}

// importScope is what an import reads: the channel, and the conversation
// with its inbox when the import is scoped to one.
type importScope struct {
	channel model.Channel
	conv    *model.Conversation
}

// Import reads a window of the provider's message history through the
// channel adapter's HistoryReader and accepts each message through the
// channel's sink, as ingress would, marked as imported by the requester
// (Sink.AcceptImported, Message.ImportedBy): it wakes no snooze and is
// unread for no one.
//
// Requester must hold model.ActionChannelImport on the channel or, for a
// conversation, on the conversation, checked first; then call's service
// principal binds the sink with model.ActionChannelReceive on the channel
// and again into the resolved inbox for every message, as Receive. A
// denial is model.Unauthorized before any provider call or write. The
// adapter must declare model.CapabilityHistory (else model.Unsupported).
//
// The window is Since <= t < Until, Until zero for no upper bound; it is
// the adapter's to apply, and the engine does not check envelopes against
// either bound. Until at or before Since is model.Validation.
//
// For an adapter whose message IDs the provider assigns
// (adapter.MessageIDsProvider) a message is imported once, by its provider
// message ID: one the destination inbox already holds, however it arrived,
// is skipped and left as it is, as is an envelope that carries no provider
// message ID. For an adapter whose message IDs the sender chooses, as mail
// does, an ID proves nothing (anyone can repeat it): no message is skipped
// for its ID, and an envelope is skipped for naming nothing only when it
// has neither a message ID nor an event ID; the sink's event claim keeps a
// message from being imported twice. In both, an envelope that is no new
// message or, for a conversation, lands in another inbox is skipped. The
// call reads at most Limit envelopes (DefaultImportLimit when 0, at most
// MaxImportLimit) across as many pages; Next resumes where it stopped and
// is empty, with Complete, once the window holds no more. Each page the
// adapter marks as a gap counts in Gaps.
//
// A reader that fails ends the call with its error as it is, RetryAfter
// included, and the report's Next is the cursor of the page that failed,
// so a later call resumes there. Import does not retry; the host paces.
func (rt *Runtime) Import(ctx context.Context, call Call, req ImportRequest) (ImportReport, error) {
	e := rt.e
	done, err := e.Begin()
	if err != nil {
		return ImportReport{}, err
	}
	defer done()
	limit, err := checkImport(call, req)
	if err != nil {
		return ImportReport{}, err
	}
	sc, err := loadImportScope(ctx, e, call.Principal.Tenant, req)
	if err != nil {
		return ImportReport{}, err
	}
	res := sc.channel.Ref.Ref()
	if sc.conv != nil {
		res = sc.conv.Ref.Ref()
	}
	requester := e.Bind(model.Principal{Tenant: call.Principal.Tenant, Actor: req.Requester})
	if err := requester.Authorize(ctx, policy.Authorization{Action: model.ActionChannelImport, Resource: res}, res); err != nil {
		return ImportReport{}, err
	}
	snk, err := ingress.Bind(ctx, e, call.Principal, sc.channel.Ref)
	if err != nil {
		return ImportReport{}, err
	}
	if err := declares(snk.Descriptor(), model.CapabilityHistory); err != nil {
		return ImportReport{}, err
	}
	inst, desc, _, err := OpenOutbound(ctx, e, snk.Channel())
	if err != nil {
		return ImportReport{}, err
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }()
	hr, ok := inst.(adapter.HistoryReader)
	if !ok {
		return ImportReport{}, model.NewError(model.Unsupported, "channel adapter cannot read history")
	}
	if f := capabilityReady(ctx, desc, inst, model.CapabilityHistory); f != nil {
		return ImportReport{}, f
	}
	im := &importer{e: e, call: call, snk: snk, by: req.Requester, sc: sc, hr: hr, limit: limit, byID: engine.ProviderAssigned(snk.Descriptor()), since: req.Since.UTC(), until: req.Until.UTC()}
	return im.run(ctx, req.After)
}

// checkImport validates the call and the request and returns the
// effective limit.
func checkImport(call Call, req ImportRequest) (int, error) {
	if err := call.Principal.Validate(); err != nil {
		return 0, err
	}
	if err := model.ActorID(req.Requester).Validate(); err != nil {
		return 0, model.NewError(model.Validation, "requester: must be an actor")
	}
	switch {
	case (req.Channel == nil) == (req.Conversation == nil):
		return 0, model.NewError(model.Validation, "import: exactly one of channel and conversation is required")
	case req.Channel != nil && req.Channel.Validate() != nil:
		return 0, model.NewError(model.Validation, "channel: malformed")
	case req.Conversation != nil && req.Conversation.Validate() != nil:
		return 0, model.NewError(model.Validation, "conversation: malformed")
	case req.Since.IsZero():
		return 0, model.NewError(model.Validation, "since: required")
	case !req.Until.IsZero() && !req.Until.After(req.Since):
		return 0, model.NewError(model.Validation, "until: must be after since")
	case req.Limit < 0:
		return 0, model.NewError(model.Validation, "limit: must not be negative")
	case req.Limit == 0:
		return DefaultImportLimit, nil
	}
	return min(req.Limit, MaxImportLimit), nil
}

// loadImportScope reads the channel, through the conversation and its
// inbox for a conversation. A reference outside tenant, or to nothing, is
// not found.
func loadImportScope(ctx context.Context, e *engine.Engine, tenant model.TenantID, req ImportRequest) (importScope, error) {
	var sc importScope
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		ref := req.Channel
		if req.Conversation != nil {
			if req.Conversation.Inbox.Tenant != tenant {
				return engine.ErrNotFound()
			}
			conv, err := r.GetConversation(ctx, *req.Conversation)
			if err != nil {
				return err
			}
			if conv.Ref != *req.Conversation {
				return engine.ErrNotFound()
			}
			inbox, err := r.GetInbox(ctx, conv.Ref.Inbox)
			if err != nil {
				return err
			}
			sc.conv, ref = &conv, &inbox.Channel
		}
		if ref.Tenant != tenant {
			return engine.ErrNotFound()
		}
		ch, err := r.GetChannel(ctx, *ref)
		if err != nil {
			return err
		}
		if ch.Ref != *ref {
			return engine.ErrNotFound()
		}
		sc.channel = ch
		return nil
	})
	if model.IsCode(err, model.NotFound) {
		return importScope{}, engine.ErrNotFound()
	}
	return sc, err
}
