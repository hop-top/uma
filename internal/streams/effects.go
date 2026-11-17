package streams

import (
	"context"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/route"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// StreamEffector executes stream start, update and end operations.
func StreamEffector() runtime.Effector { return streamEffector{} }

type streamEffector struct{}

func (streamEffector) Intent() string { return intentStream }

func (streamEffector) Decode(e model.Extension) (runtime.Job, error) {
	in, err := decodeStreamIntent(e)
	if err != nil {
		return nil, err
	}
	switch in.Step {
	case stepStart, stepUpdate, stepEnd:
		return &streamJob{in: in}, nil
	}
	return nil, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown stream step", Retry: model.NoRetry}
}

// streamJob is one stream command. Load reads the stream, waits for the
// command's predecessor and chooses the provider call for the stream's
// mode; Settle moves the stream with the command's final state.
//
// A predecessor still queued or awaiting a retry is always waited for. One
// whose provider outcome is unknown is waited for only while the runtime
// will still reconcile it (unknownFate), and only until the stream
// deadline: past it the command is not sent and the stream ends partial.
// When nothing can resolve the outcome (no reconciler, or reconcile
// unavailable for good on the channel), nothing waits on it: every update
// carries the whole response and supersedes it, and an end whose final
// update is unknown re-asserts that update's content. Reconciliation that
// gave up within the retry budget resolves nothing either: before the
// deadline the command goes ahead the same way.
type streamJob struct {
	in   streamIntent
	rt   route.Route
	caps []model.Capability
	// target is the message an edit-mode update edits.
	target *model.ExternalRef
	call   func(adapter.Instance) (runtime.ProviderCall, bool)
	wait   bool
	local  bool
	// loadFailed skips Settle: the stream is left as the failure found it.
	loadFailed bool
	// expired: the stream deadline passed while the command waited on a
	// predecessor whose outcome is unknown; Settle ends the stream partial.
	expired bool
	// reassert: the end carries its final update's content, whose own
	// outcome is unknown.
	reassert bool
	// providerRef is the provider stream an applied start opened.
	providerRef string
}

func (j *streamJob) Audience() model.Audience { return j.in.Audience }

func (j *streamJob) Load(ctx context.Context, e *engine.Engine, tenant model.TenantID) (model.Channel, error) {
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		st, err := r.Stream(ctx, j.in.Stream)
		if err != nil {
			return err
		}
		if j.rt, err = route.Load(ctx, r, j.in.Stream.Conversation); err != nil {
			return err
		}
		j.wait = j.in.Step != stepStart && startPending(st)
		if j.in.Previous != nil && !j.wait && active(st) {
			prev, err := r.Operation(ctx, *j.in.Previous)
			if err != nil {
				return err
			}
			switch prev.Operation.State {
			case model.Queued, model.RetryPending:
				j.wait = true
			case model.Unknown:
				fate := unreconcilable
				if route.Declares(e, j.rt.Channel.Type, model.CapabilityReconcile) == nil {
					if fate, err = unknownFate(ctx, e, r, prev); err != nil {
						return err
					}
				}
				switch {
				case fate == unreconcilable:
					j.reassert = j.in.Step == stepEnd && !j.in.Cancel
				case e.Clock().Now().Before(st.Deadline):
					j.wait = fate == reconcilePending
					j.reassert = !j.wait && j.in.Step == stepEnd && !j.in.Cancel
				default:
					j.expired = true
					return &model.Error{Code: model.DeadlineExpired, SafeMessage: "stream deadline passed waiting on an unknown outcome", Retry: model.NoRetry}
				}
			}
		}
		j.caps = []model.Capability{model.CapabilityStream}
		switch j.in.Step {
		case stepStart:
			j.planStart()
		case stepUpdate:
			err = j.planUpdate(st)
		case stepEnd:
			err = j.planEnd(ctx, r, st)
		}
		if j.wait {
			// A predecessor has no answer yet; the step is planned again
			// when the item comes back.
			return nil
		}
		return err
	})
	if err != nil && !j.expired {
		j.loadFailed = true
	}
	return j.rt.Channel, err
}

func (j *streamJob) Wait() bool  { return j.wait }
func (j *streamJob) Local() bool { return j.local }

func (j *streamJob) Route() (model.ExternalRef, *model.ExternalRef) {
	return j.rt.Conv.ReplyRoute, j.target
}

// Recheck revalidates the route and reauthorizes the originating actor
// immediately before an attempt. An update rereads the stream: one
// overtaken since Load, for example while its earlier attempt was being
// reconciled, is not sent.
func (j *streamJob) Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error {
	if j.in.Step == stepUpdate {
		err := actor.Engine().Store().View(ctx, rec.Operation.Ref.Tenant, func(r store.Reader) error {
			st, err := r.Stream(ctx, j.in.Stream)
			if err != nil {
				return err
			}
			return superseded(st, j.in.Sequence)
		})
		if err != nil {
			return err
		}
	}
	return route.Recheck(ctx, actor, rec, j.rt, j.in.Audience)
}

// superseded refuses an update at or below the stream's applied sequence:
// sending it would overwrite newer content.
func superseded(st model.Stream, seq uint64) error {
	if st.AppliedSequence >= seq {
		return &model.Error{Code: model.Conflict, SafeMessage: "superseded by a later update", Retry: model.NoRetry}
	}
	return nil
}

func (j *streamJob) Capabilities() []model.Capability { return slices.Clone(j.caps) }

func (j *streamJob) Bind(inst adapter.Instance) (runtime.ProviderCall, bool) {
	if j.call == nil {
		return nil, false
	}
	return j.call(inst)
}

// active reports whether a stream has not reached a terminal state.
func active(st model.Stream) bool {
	return model.IsStreamLive(st.State) || st.State == model.StreamEnding
}

// unknownOutcome is what becomes of an outcome_unknown predecessor.
type unknownOutcome int

const (
	// unreconcilable: the channel cannot reconcile it (reconcile not
	// declared, or reported unavailable for good when the answer was lost
	// or at a pass), so nothing ever resolves it.
	unreconcilable unknownOutcome = iota
	// reconcilePending: the runtime still acts on it.
	reconcilePending
	// reconcileAbandoned: reconciliation stayed inconclusive, or deferred
	// by a temporary outage, until the retry budget was spent. Nothing
	// resolves it any more, but commands behind it waited on it, so the
	// stream deadline still applies.
	reconcileAbandoned
)

// unknownFate reads the fate of rec, an outcome_unknown operation, from
// the state its worker wrote, without opening the channel. A reconcile
// pass is scheduled (NextAttemptAt), including one deferred while
// reconcile is temporarily unavailable, or the latest attempt is under
// way, with only the entry recorded before its provider call (a retry
// after reconciliation showed the effect was not applied, or an owner
// that stopped mid-call, whose successor reconciles it): pending.
// Otherwise the worker settled it outcome_unknown for good: abandoned when
// the latest attempt's evidence outgrew the retry budget, the runtime's
// own test for giving up, else unreconcilable.
//
// An owner that stopped mid-call on a channel that later could not
// reconcile leaves a settled attempt that looks under way: commands behind
// it wait out the stream deadline, as on any reconciling adapter.
func unknownFate(ctx context.Context, e *engine.Engine, r store.Reader, rec store.OperationRecord) (unknownOutcome, error) {
	if rec.Operation.NextAttemptAt != nil {
		return reconcilePending, nil
	}
	h, err := operations.LoadHistory(ctx, r, rec.Operation.Ref)
	switch {
	case err != nil:
		return unreconcilable, err
	case h.Unresolved() && h.LastCount == 1:
		return reconcilePending, nil
	case h.LastCount > e.Retry().MaxAttempts:
		return reconcileAbandoned, nil
	}
	return unreconcilable, nil
}

// startPending reports whether a native stream still waits for its start's
// provider answer.
func startPending(st model.Stream) bool {
	return st.Mode == model.StreamNative && st.ProviderStreamRef == "" &&
		(st.State == model.StreamOpening || st.State == model.StreamEnding)
}

// applied reports whether a settled operation state shows the effect was
// carried out, by the provider or locally.
func applied(s model.DeliveryState) bool {
	switch s {
	case model.ProviderAccepted, model.Delivered, model.Committed:
		return true
	}
	return false
}

// planStart opens the provider stream.
func (j *streamJob) planStart() {
	j.call = func(inst adapter.Instance) (runtime.ProviderCall, bool) {
		s, ok := inst.(adapter.Streamer)
		if !ok {
			return nil, false
		}
		return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
			out, err := s.Start(ctx, adapter.StreamRequest{Effect: eff, Mode: j.in.Mode})
			j.providerRef = out.ProviderStreamRef
			return out.Outcome, err
		}, true
	}
}

// planUpdate delivers one update: through the native stream, or as a send
// then edits of the sent message. An update overtaken by a later applied
// one is not sent: it would overwrite newer content.
func (j *streamJob) planUpdate(st model.Stream) error {
	if st.State != model.StreamOpen && st.State != model.StreamEnding {
		return &model.Error{Code: model.Permanent, SafeMessage: "stream is not open", Retry: model.NoRetry}
	}
	if err := superseded(st, j.in.Sequence); err != nil {
		return err
	}
	content := j.in.Content.Clone()
	switch {
	case j.in.Mode == model.StreamNative:
		ref := st.ProviderStreamRef
		j.call = func(inst adapter.Instance) (runtime.ProviderCall, bool) {
			s, ok := inst.(adapter.Streamer)
			if !ok {
				return nil, false
			}
			return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
				out, err := s.Update(ctx, ref, adapter.StreamRequest{
					Effect: eff, Mode: j.in.Mode, Sequence: j.in.Sequence, Content: content,
				})
				return out.Outcome, err
			}, true
		}
	case len(st.ProviderReferences) == 0:
		j.caps = []model.Capability{model.CapabilitySend}
		j.call = sendCall(content)
	default:
		j.caps = []model.Capability{model.CapabilityEdit}
		target := st.ProviderReferences[0]
		j.target = &target
		j.call = editCall(content)
	}
	return nil
}

func editCall(content model.Content) func(adapter.Instance) (runtime.ProviderCall, bool) {
	return func(inst adapter.Instance) (runtime.ProviderCall, bool) {
		ed, ok := inst.(adapter.Editor)
		if !ok {
			return nil, false
		}
		return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
			return ed.Edit(ctx, adapter.EditRequest{Effect: eff, Content: content})
		}, true
	}
}

// planEnd finishes or cancels the stream: through the native stream, as
// the one send of the last buffered update, or locally when the content is
// already in place.
func (j *streamJob) planEnd(ctx context.Context, r store.Reader, st model.Stream) error {
	switch {
	case st.State == model.StreamFailed || st.State == model.StreamUnknown:
		return &model.Error{Code: model.Permanent, SafeMessage: "stream did not open", Retry: model.NoRetry}
	case !active(st):
		return &model.Error{Code: model.Permanent, SafeMessage: "stream already ended", Retry: model.NoRetry}
	}
	var final model.Content
	if j.reassert {
		var err error
		if final, err = lastUpdate(ctx, r, j.in.Previous); err != nil {
			return err
		}
	}
	switch {
	case j.in.Mode == model.StreamNative:
		ref := st.ProviderStreamRef
		j.call = func(inst adapter.Instance) (runtime.ProviderCall, bool) {
			s, ok := inst.(adapter.Streamer)
			if !ok {
				return nil, false
			}
			return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
				var out adapter.StreamOutcome
				var err error
				if j.in.Cancel {
					out, err = s.Cancel(ctx, ref, eff)
				} else {
					out, err = s.Finish(ctx, ref, adapter.StreamRequest{Effect: eff, Mode: j.in.Mode, Sequence: j.in.Sequence, Content: final})
				}
				return out.Outcome, err
			}, true
		}
	case j.in.Mode == model.StreamBuffer && !j.in.Cancel && j.in.Sequence > 0:
		content, err := lastUpdate(ctx, r, j.in.Previous)
		if err != nil {
			return err
		}
		j.caps = []model.Capability{model.CapabilitySend}
		j.call = sendCall(content)
	case j.in.Mode == model.StreamEdit && j.reassert && len(st.ProviderReferences) > 0:
		j.caps = []model.Capability{model.CapabilityEdit}
		target := st.ProviderReferences[0]
		j.target = &target
		j.call = editCall(final)
	default:
		j.reassert = false
		j.local = true
	}
	return nil
}

func sendCall(content model.Content) func(adapter.Instance) (runtime.ProviderCall, bool) {
	return func(inst adapter.Instance) (runtime.ProviderCall, bool) {
		s, ok := inst.(adapter.Sender)
		if !ok {
			return nil, false
		}
		return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
			return s.Send(ctx, adapter.SendRequest{Effect: eff, Content: content})
		}, true
	}
}

// Settle moves the stream with the command's final state. A start records
// the provider stream and opens the stream, or fails it when the provider
// refused; a start whose answer or reference is lost leaves the stream
// outcome_unknown. An applied update advances the applied sequence. An end
// decides the terminal state. A command stopped by the stream deadline ends
// the stream partial.
func (j *streamJob) Settle(ctx context.Context, tx store.Tx, op model.Operation) error {
	if j.loadFailed {
		return nil
	}
	in := j.in
	if j.expired {
		return changeStream(ctx, tx, in.Stream, op, func(st *model.Stream) {
			if active(*st) {
				st.State = model.StreamPartial
			}
		})
	}
	switch in.Step {
	case stepStart:
		return changeStream(ctx, tx, in.Stream, op, func(st *model.Stream) {
			switch {
			case applied(op.State) && j.providerRef != "":
				st.ProviderStreamRef = j.providerRef
				if st.State == model.StreamOpening {
					st.State = model.StreamOpen
				}
			case op.State == model.Failed || op.State == model.Rejected:
				st.State = model.StreamFailed
			default:
				st.State = model.StreamUnknown
			}
		})
	case stepUpdate:
		if !applied(op.State) {
			return nil
		}
		return changeStream(ctx, tx, in.Stream, op, func(st *model.Stream) {
			st.AppliedSequence = max(st.AppliedSequence, in.Sequence)
		})
	}
	buffered := in.Mode == model.StreamBuffer
	return changeStream(ctx, tx, in.Stream, op, func(st *model.Stream) {
		done := applied(op.State)
		if done && (buffered || j.reassert) && !in.Cancel {
			st.AppliedSequence = in.Sequence
		}
		switch {
		case op.State == model.Unknown:
			st.State = model.StreamUnknown
		case done && in.Cancel:
			st.State = model.StreamCancelled
		case done && st.AppliedSequence >= in.Sequence:
			st.State = model.StreamFinished
		case done || st.AppliedSequence > 0 || len(st.ProviderReferences) > 0:
			st.State = model.StreamPartial
		default:
			st.State = model.StreamFailed
		}
	})
}

// lastUpdate returns the content of the buffered update op.
func lastUpdate(ctx context.Context, r store.Reader, op *model.OperationRef) (model.Content, error) {
	if op == nil {
		return model.Content{}, &model.Error{Code: model.Permanent, SafeMessage: "buffered stream has no update", Retry: model.NoRetry}
	}
	rec, err := r.Operation(ctx, *op)
	if err != nil {
		return model.Content{}, err
	}
	in, err := decodeStreamIntent(rec.Command)
	if err != nil {
		return model.Content{}, err
	}
	if in.Step != stepUpdate {
		return model.Content{}, &model.Error{Code: model.Permanent, SafeMessage: "buffered stream has no update", Retry: model.NoRetry}
	}
	return in.Content.Clone(), nil
}

// changeStream applies change to the stored stream and merges op's
// provider references into it, under op's new revision.
func changeStream(ctx context.Context, tx store.Tx, ref model.StreamRef, op model.Operation, change func(*model.Stream)) error {
	st, err := tx.Stream(ctx, ref)
	if err != nil {
		return err
	}
	prev := st.Revision
	change(&st)
	for _, r := range op.ProviderReferences {
		if !slices.Contains(st.ProviderReferences, r) {
			st.ProviderReferences = append(st.ProviderReferences, r)
		}
	}
	st.Revision = op.Revision
	return putStream(ctx, tx, st, prev)
}
