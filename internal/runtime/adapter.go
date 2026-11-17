package runtime

import (
	"context"
	"errors"
	"slices"
	"sync"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// OpenOutbound opens an instance of channel's adapter for one execution.
// The instance receives credentials and protocol state scoped to the
// channel and is closed by the caller. The engine runs engine.CheckInstance
// before using it.
func OpenOutbound(ctx context.Context, e *engine.Engine, ch model.Channel) (adapter.Instance, adapter.Descriptor, *scopedCredentials, error) {
	factory, desc, ok := e.Adapter(ch.Type)
	if !ok {
		return nil, adapter.Descriptor{}, nil, &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter not registered", Retry: model.NoRetry}
	}
	creds := &scopedCredentials{host: e.Credentials(), channel: ch.Ref, reference: ch.CredentialRef}
	inst, err := factory.Open(ctx, ch.Clone(), adapter.Dependencies{
		Credentials: creds, HTTP: e.HTTP(), Clock: e.Clock(), Observer: e.Observer(),
		Protocol: newProtocolState(e, ch.Ref),
	})
	if err != nil {
		return nil, adapter.Descriptor{}, nil, err
	}
	if err := engine.CheckInstance(desc, inst); err != nil {
		if inst != nil {
			_ = inst.Close(context.WithoutCancel(ctx))
		}
		return nil, adapter.Descriptor{}, nil, &model.Error{Code: model.Permanent, SafeMessage: "adapter instance does not match its descriptor", Retry: model.NoRetry}
	}
	return inst, desc, creds, nil
}

// capabilityReady checks that the descriptor declares c and the open
// instance reports it available: exactly one status for c, available. A
// capability the instance does not report is unavailable, never assumed
// available. A capability withdrawn for lack of permission is
// model.Unauthorized and never retried. A report naming a capability twice,
// or marking one available with a reason, is invalid adapter output
// (CheckStatuses) and fails the gate whatever c is.
func capabilityReady(ctx context.Context, d adapter.Descriptor, inst adapter.Instance, c model.Capability) *model.Error {
	_, f := capabilityStatus(ctx, d, inst, c)
	return f
}

// CapabilityStatusOf is CapabilityReady that also returns the status the
// instance reported for c, available, with its limits.
func CapabilityStatusOf(ctx context.Context, d adapter.Descriptor, inst adapter.Instance, c model.Capability) (model.CapabilityStatus, *model.Error) {
	return capabilityStatus(ctx, d, inst, c)
}

func capabilityStatus(ctx context.Context, d adapter.Descriptor, inst adapter.Instance, c model.Capability) (model.CapabilityStatus, *model.Error) {
	if !slices.Contains(d.Capabilities, c) {
		return model.CapabilityStatus{}, &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter does not declare " + string(c), Retry: model.NoRetry}
	}
	statuses, err := inst.Capabilities(ctx)
	if err != nil {
		return model.CapabilityStatus{}, model.NewError(model.Unavailable, "adapter capabilities unavailable")
	}
	if f := CheckStatuses(statuses); f != nil {
		return model.CapabilityStatus{}, f
	}
	i := slices.IndexFunc(statuses, func(s model.CapabilityStatus) bool { return s.Name == c })
	if i < 0 {
		return model.CapabilityStatus{}, &model.Error{Code: model.Unsupported, SafeMessage: string(c) + " unavailable", Retry: model.NoRetry}
	}
	s := statuses[i]
	switch {
	case s.Available:
		return s, nil
	case s.Reason == model.Unauthorized:
		return s, &model.Error{Code: model.Unauthorized, SafeMessage: "provider permission for " + string(c) + " withdrawn", Retry: model.NoRetry}
	case s.Reason.DefaultRetry() == model.RetrySameOperation:
		return s, model.NewError(s.Reason, string(c)+" temporarily unavailable")
	}
	return s, &model.Error{Code: model.Unsupported, SafeMessage: string(c) + " unavailable", Retry: model.NoRetry}
}

// CheckStatuses validates an instance's capability report: at most one
// status per capability, and no available status carrying a reason.
// Anything else is model.Permanent, never retried: the adapter's output
// cannot be read either way.
func CheckStatuses(statuses []model.CapabilityStatus) *model.Error {
	seen := make(map[model.Capability]bool, len(statuses))
	for _, s := range statuses {
		if seen[s.Name] || (s.Available && s.Reason != "") {
			return &model.Error{Code: model.Permanent, SafeMessage: "adapter reported invalid capability statuses", Retry: model.NoRetry}
		}
		seen[s.Name] = true
	}
	return nil
}

// scopedCredentials is the credential port an outbound instance receives.
// It binds every request to the instance's channel and credential
// reference, so an adapter never resolves another channel's credential, and
// remembers whether the host refused a credential during the current
// attempt.
type scopedCredentials struct {
	host      ports.Credentials
	channel   model.ChannelRef
	reference string

	mu      sync.Mutex
	refused bool
}

// Resolve forwards req to the host after scoping it. An empty Channel or
// Reference takes the bound value; any other value outside the scope is
// model.Unauthorized. A refusal (model.Unauthorized) is recorded and
// returned with retry advice none.
func (s *scopedCredentials) Resolve(ctx context.Context, req ports.CredentialRequest) (ports.Credential, error) {
	if req.Channel == (model.ChannelRef{}) {
		req.Channel = s.channel
	}
	if req.Reference == "" {
		req.Reference = s.reference
	}
	switch {
	case req.Channel != s.channel || req.Reference != s.reference:
		return nil, &model.Error{Code: model.Unauthorized, SafeMessage: "credential outside the channel scope", Retry: model.NoRetry}
	case req.Purpose == "":
		return nil, &model.Error{Code: model.Validation, SafeMessage: "credential purpose required", Retry: model.NoRetry}
	case req.Reference == "":
		s.refuse()
		return nil, &model.Error{Code: model.Unauthorized, SafeMessage: "channel has no credential reference", Retry: model.NoRetry}
	case s.host == nil:
		return nil, &model.Error{Code: model.Unavailable, SafeMessage: "credentials port not configured", Retry: model.NoRetry}
	}
	c, err := s.host.Resolve(ctx, req)
	if err == nil {
		return c, nil
	}
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return nil, err
	}
	var me *model.Error
	if !errors.As(err, &me) || !me.Code.Valid() {
		return nil, model.NewError(model.Unavailable, "credential store unavailable")
	}
	if me.Code == model.Unauthorized {
		s.refuse()
		return nil, &model.Error{Code: model.Unauthorized, SafeMessage: me.SafeMessage, Retry: model.NoRetry}
	}
	return nil, me
}

func (s *scopedCredentials) refuse() {
	s.mu.Lock()
	s.refused = true
	s.mu.Unlock()
}

// beginAttempt clears the refusal flag before an attempt; refusedNow reads
// it after.
func (s *scopedCredentials) beginAttempt() {
	s.mu.Lock()
	s.refused = false
	s.mu.Unlock()
}

func (s *scopedCredentials) refusedNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// verdict is the engine's reading of one provider answer.
type verdict int

const (
	// verdictApplied: the provider applied the effect.
	verdictApplied verdict = iota
	// verdictFinal: the effect was not applied and must not be retried.
	verdictFinal
	// verdictRetry: the effect was not applied and a retry is safe.
	verdictRetry
	// verdictUnknown: the effect may or may not have been applied.
	verdictUnknown
)

// answer is a classified provider answer: the evidence state to record and
// how the engine proceeds.
type answer struct {
	verdict    verdict
	state      model.DeliveryState
	references []model.ExternalRef
	failure    *model.Error
	// changes are the reference changes an applied provider state effect
	// reported (adapter.Outcome.ReferenceChanges).
	changes []adapter.ReferenceChange
}

// classify reads an adapter outcome. Only a typed answer counts as
// evidence that nothing was applied: an untyped error, a cancelled call or
// an unrecognised state is outcome unknown. refused reports that the host
// refused the attempt's credential, so no provider call was made.
func classify(ctx context.Context, out adapter.Outcome, err error, refused bool) answer {
	refs := slices.Clone(out.References)
	if refused && (err != nil || !operations.AppliedState(out.State)) {
		return answer{verdict: verdictFinal, state: model.Failed, references: refs,
			failure: &model.Error{Code: model.Unauthorized, SafeMessage: "credential refused", Retry: model.NoRetry}}
	}
	if err != nil {
		var me *model.Error
		switch {
		case ctx.Err() != nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return unknownAnswer(refs)
		case !errors.As(err, &me) || !me.Code.Valid():
			return unknownAnswer(refs)
		case me.Code == model.OutcomeUnknown:
			return unknownAnswer(refs)
		case engine.Retryable(me):
			return answer{verdict: verdictRetry, state: model.Failed, references: refs, failure: cloneError(me)}
		}
		return answer{verdict: verdictFinal, state: model.Failed, references: refs, failure: noRetry(me)}
	}
	switch out.State {
	case model.Queued, model.ProviderAccepted, model.Delivered:
		return answer{verdict: verdictApplied, state: out.State, references: refs, changes: slices.Clone(out.ReferenceChanges)}
	case model.Rejected:
		f := out.Failure
		if f == nil {
			f = model.NewError(model.Permanent, "provider rejected the effect")
		}
		return answer{verdict: verdictFinal, state: model.Rejected, references: refs, failure: noRetry(f)}
	case model.Failed:
		f := out.Failure
		if f == nil {
			f = model.NewError(model.Permanent, "provider reported failure")
		}
		if engine.Retryable(f) {
			return answer{verdict: verdictRetry, state: model.Failed, references: refs, failure: cloneError(f)}
		}
		return answer{verdict: verdictFinal, state: model.Failed, references: refs, failure: noRetry(f)}
	}
	return unknownAnswer(refs)
}

func unknownAnswer(refs []model.ExternalRef) answer {
	return answer{verdict: verdictUnknown, state: model.Unknown, references: refs, failure: outcomeUnknown()}
}

func outcomeUnknown() *model.Error {
	return model.NewError(model.OutcomeUnknown, "provider outcome unknown; reconcile before retrying")
}

func cloneError(e *model.Error) *model.Error {
	c := *e
	if !c.Retry.Valid() {
		c.Retry = c.Code.DefaultRetry()
	}
	return &c
}

func noRetry(e *model.Error) *model.Error {
	c := *e
	c.Retry = model.NoRetry
	return &c
}
