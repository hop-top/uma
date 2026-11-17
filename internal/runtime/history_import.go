package runtime

import (
	"context"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// importer is one Import call's reading state; by is the requester,
// recorded on each imported message (Message.ImportedBy).
type importer struct {
	e     *engine.Engine
	call  Call
	snk   *ingress.Sink
	by    model.ActorID
	sc    importScope
	hr    adapter.HistoryReader
	limit int
	byID  bool // provider-assigned IDs: a message ID is identity
	since time.Time
	until time.Time
	read  int
	rep   ImportReport
}

// run reads pages from cursor until the window ends or the limit is
// reached. A page counts toward the limit by its envelopes; a call reads
// at most limit pages, so pages without envelopes cannot keep it going.
func (im *importer) run(ctx context.Context, cursor string) (ImportReport, error) {
	var route *model.ExternalRef
	if im.sc.conv != nil {
		r := im.sc.conv.ReplyRoute
		route = &r
	}
	for pages := 0; im.read < im.limit && pages < im.limit; pages++ {
		page, err := im.hr.History(ctx, adapter.HistoryRequest{Route: route, Since: im.since, Until: im.until, Cursor: cursor, Limit: im.limit - im.read, Stager: im.snk})
		if err != nil {
			im.rep.Next = cursor
			return im.rep, safeAdapterError(ctx, err, "history read failed")
		}
		if page.Gap {
			im.rep.Gaps++
		}
		take := min(len(page.Envelopes), im.limit-im.read)
		for _, env := range page.Envelopes[:take] {
			if err := im.one(ctx, env); err != nil {
				return im.rep, err
			}
		}
		switch {
		case take < len(page.Envelopes):
			// The rest of this page comes again next time; what this call
			// imported is then skipped.
			im.rep.Next = cursor
			return im.rep, nil
		case page.Next == "":
			im.rep.Complete = true
			return im.rep, nil
		case page.Next == cursor:
			return im.rep, &model.Error{Code: model.Permanent, SafeMessage: "history cursor did not advance", Retry: model.NoRetry}
		}
		cursor = page.Next
	}
	im.rep.Next = cursor
	return im.rep, nil
}

// names reports whether env is a new message that names itself: with
// provider-assigned IDs by a provider message ID, else by that or an event
// ID, which the sink's event claim dedups on.
func (im *importer) names(env model.Envelope) bool {
	if env.Kind != model.EnvelopeMessage && env.Kind != model.EnvelopeAutomatedMessage {
		return false
	}
	return env.MessageID != "" || (!im.byID && env.EventID != "")
}

// one imports one envelope unless it is skipped: no new message, naming
// nothing, outside the conversation's inbox, or (under provider-assigned
// IDs) a message the inbox already holds.
func (im *importer) one(ctx context.Context, env model.Envelope) error {
	im.read++
	if !im.names(env) {
		im.rep.Skipped++
		return nil
	}
	skip, err := im.held(ctx, env)
	if err != nil {
		return err
	}
	if skip {
		im.rep.Skipped++
		return nil
	}
	r, err := im.snk.AcceptImported(ctx, model.Ingress{Call: im.call.Call, Envelope: env}, im.by)
	if err != nil {
		return err
	}
	if r.Duplicate {
		im.rep.Skipped++
	} else {
		im.rep.Imported++
	}
	return nil
}

// held reports whether env is skipped before ingress: its destination is
// outside the conversation's inbox, or, under provider-assigned IDs, its
// inbox already holds its provider message. An unresolvable destination is left to ingress, which
// refuses it.
func (im *importer) held(ctx context.Context, env model.Envelope) (bool, error) {
	ch := im.sc.channel.Ref
	var skip bool
	err := im.e.Store().View(ctx, ch.Tenant, func(r store.Reader) error {
		inbox, err := r.ResolveDestination(ctx, ch, env.Destination)
		switch {
		case model.IsCode(err, model.NotFound) || model.IsCode(err, model.Conflict):
			return nil
		case err != nil:
			return err
		}
		if im.sc.conv != nil && inbox.Ref != im.sc.conv.Ref.Inbox {
			skip = true
			return nil
		}
		if !im.byID {
			return nil
		}
		msgs, err := r.MessagesByReference(ctx, inbox.Ref, model.ProviderMessageRef(ch, env.MessageID), 1)
		skip = len(msgs) > 0
		return err
	})
	return skip, err
}
