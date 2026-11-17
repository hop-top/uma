package ingress

import (
	"context"
	"slices"

	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/workitems"
	"hop.top/uma/model"
)

// Coarse revisions. A provider whose source revision is coarse (GitHub's
// updated_at has one-second precision) can report two changes of one
// resource under the same source revision and time. An observation that
// ties with the applied version (same source revision, or same source
// time) but carries different content cannot be ordered: ingress keeps
// what it holds and, when the adapter declares model.CapabilityRefetch,
// records in the same unit a refetch operation and its work. The work
// reads the resource again through the adapter's Refetcher and applies
// the copy as authoritative (AcceptRefetched), so the provider's current
// state wins whatever order the observations arrived in.

// refetchEventPrefix starts the event ID of every refetched copy; the rest
// is its refetch operation's ID, so a repeated refetch is a duplicate.
const refetchEventPrefix = operations.RefetchKeyPrefix

// AcceptRefetched applies the copy a refetch read as authoritative: a
// message edit or work item state applies even when its source version
// is not newer than the applied one. Its event ID is the refetch
// operation's, so a repeated refetch is a duplicate; otherwise it is
// checked and applied like any ingress.
func (s *Sink) AcceptRefetched(ctx context.Context, op model.OperationRef, in model.Ingress) (model.IngressReceipt, error) {
	in.Envelope = in.Envelope.Clone()
	in.Envelope.EventID = refetchEventPrefix + string(op.ID)
	return s.accept(ctx, in, acceptMode{authoritative: true})
}

// tiedEdit reports whether an edit of m the applied version cannot order
// carries other content than m holds.
func (u *ingressUnit) tiedEdit(m model.Message) bool {
	v, ok := appliedSource(m)
	if !ok || u.authoritative || len(u.env.Content.Parts) == 0 {
		return false
	}
	same := (u.env.SourceRevision != "" && u.env.SourceRevision == v.SourceRevision) ||
		(!u.env.OccurredAt.IsZero() && u.env.OccurredAt.Equal(v.OccurredAt))
	return same && !sameContent(m.Content, u.env.Content)
}

func sameContent(a, b model.Content) bool { return operations.SameContent(a, b) }

// tiedWorkItem reports whether the envelope's work item state ties with
// cur's applied version and differs from it.
func (u *ingressUnit) tiedWorkItem(cur *model.WorkItem, obs workitems.Observation) bool {
	return cur != nil && !u.authoritative && workitems.Tied(*cur, obs)
}

// refetch records a refetch of target, a provider resource stored as
// resource, when the adapter declares the refetch capability: an
// operation in the queued state, as the sink's service principal, keyed
// by this ingress record, and its work. The descriptor decides, as the
// unit has no adapter instance to ask: the channel's reported
// availability gates the step when it runs, which settles it failed when
// refetch is unavailable for good and retries it after a temporary outage.
func (u *ingressUnit) refetch(target model.ExternalRef, resource model.Ref) error {
	if !slices.Contains(u.s.desc.Capabilities, model.CapabilityRefetch) {
		return nil
	}
	return operations.RecordRefetch(u.ctx, u.tx, u.s.e, u.s.principal.Actor, model.OperationID(refetchEventPrefix+string(u.id)), resource,
		operations.RefetchIntent{
			Channel: u.s.channel.Ref, Target: target, Destination: u.env.Destination, References: withoutEchoRefs(u.env.References),
		})
}
