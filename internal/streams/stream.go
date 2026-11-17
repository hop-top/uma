package streams

import (
	"context"
	"encoding/json"
	"strconv"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/route"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// intentStream names the stored intent of every stream command.
const intentStream = "uma.intent.stream"

// Stream command steps.
const (
	stepStart  = "start"
	stepUpdate = "update"
	stepEnd    = "end"
)

// streamIntent is the durable intent of one stream command. Previous is the
// stream's latest command when this one was accepted; this one executes
// only once Previous has a provider answer. Sequence is the update's
// sequence, or for an end the final sequence.
type streamIntent struct {
	Stream   model.StreamRef     `json:"stream"`
	Step     string              `json:"step"`
	Mode     string              `json:"mode"`
	Audience model.Audience      `json:"audience"`
	Sequence uint64              `json:"sequence"`
	Content  model.Content       `json:"content"`
	Cancel   bool                `json:"cancel"`
	Previous *model.OperationRef `json:"previous,omitempty"`
}

func (in streamIntent) extension() (model.Extension, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return model.Extension{}, model.NewError(model.Validation, "stream command cannot be stored")
	}
	return model.Extension{Name: intentStream, Version: 1, Data: data}, nil
}

func decodeStreamIntent(e model.Extension) (streamIntent, error) {
	var in streamIntent
	if e.Name != intentStream || e.Version != 1 {
		return in, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown operation intent", Retry: model.NoRetry}
	}
	if err := json.Unmarshal(e.Data, &in); err != nil {
		return in, &model.Error{Code: model.Permanent, SafeMessage: "stored operation intent unreadable", Retry: model.NoRetry}
	}
	return in, nil
}

// modeCapabilities are the capabilities a stream mode needs.
func modeCapabilities(mode string) []model.Capability {
	switch mode {
	case model.StreamNative:
		return []model.Capability{model.CapabilityStream}
	case model.StreamEdit:
		return []model.Capability{model.CapabilitySend, model.CapabilityEdit}
	}
	return []model.Capability{model.CapabilitySend}
}

// Start opens a logical streaming response in the conversation under the
// requested audience and mode. The channel's adapter must declare the
// mode's capabilities; no other mode is substituted. A native start is
// queued for the provider; an edit or buffer start commits locally. The
// result is the new stream, whose first update carries sequence 1 and
// whose deadline is the requested one (model.DefaultStreamDeadline when
// omitted) after acceptance.
func (s Service) Start(ctx context.Context, req model.StartStream) (model.Acceptance, error) {
	e := s.c.Engine()
	if e.IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if !model.IsStreamMode(req.Mode) {
		return model.Acceptance{}, model.NewError(model.Validation, "mode: must be native, edit or buffer")
	}
	if req.Conversation.Inbox.Tenant != s.c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	var rt route.Route
	err := e.Store().View(ctx, s.c.Principal().Tenant, func(r store.Reader) error {
		var err error
		if rt, err = route.Load(ctx, r, req.Conversation); err != nil {
			return err
		}
		return route.Declares(e, rt.Channel.Type, modeCapabilities(req.Mode)...)
	})
	if err != nil {
		return model.Acceptance{}, route.NotFound(err)
	}
	id, err := operations.NewID(ctx, e)
	if err != nil {
		return model.Acceptance{}, err
	}
	ref := model.StreamRef{Conversation: req.Conversation, ID: id}
	res := ref.Ref()
	native := req.Mode == model.StreamNative
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str("stream.start"))
	f.Set("target", operations.Field().Ref(req.Conversation.Ref()))
	f.Set("mode", operations.Field().Str(req.Mode))
	f.Set("audience", operations.Field().Audience(req.Audience))
	f.Set("deadline_ms", operations.Field().Str(strconv.FormatInt(req.Deadline.Milliseconds(), 10)))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	deadline := model.StreamDeadline(req.Deadline)
	return submit(ctx, s.c, operations.Submission{
		Kind: model.ActionStreamWrite, Target: req.Conversation.Ref(), Owner: rt.Conv.Ref.Ref(),
		Command: req.Command, Audience: req.Audience, Fingerprint: f.Sum(),
		Result: &res, ResultStream: &ref, Local: !native,
		Build: func(context.Context, store.Tx) (model.Extension, error) {
			return streamIntent{Stream: ref, Step: stepStart, Mode: req.Mode, Audience: req.Audience}.extension()
		},
		Apply: func(ctx context.Context, tx store.Tx, op model.OperationRef, rev model.Revision) error {
			state := model.StreamOpen
			if native {
				state = model.StreamOpening
			}
			return putStream(ctx, tx, model.Stream{
				Ref: ref, Mode: req.Mode, State: state, Audience: req.Audience, NextSequence: 1,
				LastOperation: &op, Deadline: e.Clock().Now().Add(deadline), Revision: rev,
			}, "")
		},
	})
}

// Update submits the stream's next content. Each update carries the whole
// response so far, replacing the previous one; its sequence must be the
// stream's NextSequence, so a stale or reordered update conflicts. The
// stream must be opening or open. A buffered update commits locally; the
// others are queued and execute in sequence order.
func (s Service) Update(ctx context.Context, req model.StreamUpdate) (model.Acceptance, error) {
	if s.c.Engine().IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	st, err := s.loadStream(ctx, req.Stream)
	if err != nil {
		return model.Acceptance{}, err
	}
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str("stream.update"))
	f.Set("target", operations.Field().Ref(req.Stream.Ref()))
	f.Set("sequence", operations.Field().Str(strconv.FormatUint(req.Sequence, 10)))
	f.Set("content", operations.Field().Content(req.Content))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	ref := req.Stream
	res := ref.Ref()
	return submit(ctx, s.c, operations.Submission{
		Kind: model.ActionStreamWrite, Target: res, Owner: st.Ref.Ref(),
		Command: req.Command, Audience: st.Audience, Fingerprint: f.Sum(),
		Result: &res, ResultStream: &ref, Local: st.Mode == model.StreamBuffer,
		Build: func(ctx context.Context, tx store.Tx) (model.Extension, error) {
			cur, err := tx.Stream(ctx, ref)
			if err != nil {
				return model.Extension{}, err
			}
			return streamIntent{
				Stream: ref, Step: stepUpdate, Mode: cur.Mode, Audience: cur.Audience,
				Sequence: req.Sequence, Content: req.Content.Clone(), Previous: cur.LastOperation,
			}.extension()
		},
		Apply: func(ctx context.Context, tx store.Tx, op model.OperationRef, rev model.Revision) error {
			cur, err := tx.Stream(ctx, ref)
			if err != nil {
				return err
			}
			switch {
			case req.ExpectedRevision != "" && cur.Revision != req.ExpectedRevision:
				return model.NewError(model.Conflict, "stream revision changed")
			case !model.IsStreamLive(cur.State):
				return model.NewError(model.Conflict, "stream is not open")
			case req.Sequence != cur.NextSequence:
				return model.NewError(model.Conflict, "sequence out of order: want "+strconv.FormatUint(cur.NextSequence, 10))
			}
			prev := cur.Revision
			cur.NextSequence++
			cur.LastOperation, cur.Revision = &op, rev
			return putStream(ctx, tx, cur, prev)
		},
	})
}

// End finishes or cancels the stream after its last update. FinalSequence
// must name that update (0 when there was none). A stream ends once: a
// second end with another operation ID conflicts, while a retransmitted end
// with the same operation ID and payload returns the stored result. The end
// is queued and executes after every earlier command; its outcome decides
// the stream's terminal state, including partial and unknown outcomes.
func (s Service) End(ctx context.Context, req model.EndStream) (model.Acceptance, error) {
	if s.c.Engine().IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	st, err := s.loadStream(ctx, req.Stream)
	if err != nil {
		return model.Acceptance{}, err
	}
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str("stream.end"))
	f.Set("target", operations.Field().Ref(req.Stream.Ref()))
	f.Set("final_sequence", operations.Field().Str(strconv.FormatUint(req.FinalSequence, 10)))
	f.Set("cancel", operations.Field().Present(req.Cancel))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	ref := req.Stream
	res := ref.Ref()
	return submit(ctx, s.c, operations.Submission{
		Kind: model.ActionStreamWrite, Target: res, Owner: st.Ref.Ref(),
		Command: req.Command, Audience: st.Audience, Fingerprint: f.Sum(),
		Result: &res, ResultStream: &ref,
		Build: func(ctx context.Context, tx store.Tx) (model.Extension, error) {
			cur, err := tx.Stream(ctx, ref)
			if err != nil {
				return model.Extension{}, err
			}
			return streamIntent{
				Stream: ref, Step: stepEnd, Mode: cur.Mode, Audience: cur.Audience,
				Sequence: req.FinalSequence, Cancel: req.Cancel, Previous: cur.LastOperation,
			}.extension()
		},
		Apply: func(ctx context.Context, tx store.Tx, op model.OperationRef, rev model.Revision) error {
			cur, err := tx.Stream(ctx, ref)
			if err != nil {
				return err
			}
			switch {
			case req.ExpectedRevision != "" && cur.Revision != req.ExpectedRevision:
				return model.NewError(model.Conflict, "stream revision changed")
			case !model.IsStreamLive(cur.State):
				return model.NewError(model.Conflict, "stream already ended")
			case req.FinalSequence+1 != cur.NextSequence:
				return model.NewError(model.Conflict, "final sequence is not the last update: want "+strconv.FormatUint(cur.NextSequence-1, 10))
			}
			prev := cur.Revision
			cur.State = model.StreamEnding
			cur.LastOperation, cur.Revision = &op, rev
			return putStream(ctx, tx, cur, prev)
		},
	})
}

// loadStream reads a stream for a command, with the capabilities its mode
// needs. A foreign or unknown stream is not found.
func (s Service) loadStream(ctx context.Context, ref model.StreamRef) (model.Stream, error) {
	e := s.c.Engine()
	if ref.Scope() != s.c.Principal().Tenant {
		return model.Stream{}, engine.ErrNotFound()
	}
	var st model.Stream
	err := e.Store().View(ctx, ref.Scope(), func(r store.Reader) error {
		var err error
		if st, err = r.Stream(ctx, ref); err != nil {
			return err
		}
		rt, err := route.Load(ctx, r, ref.Conversation)
		if err != nil {
			return err
		}
		return route.Declares(e, rt.Channel.Type, modeCapabilities(st.Mode)...)
	})
	return st, route.NotFound(err)
}

// putStream writes st over expected. A revision conflict means a
// concurrent unit changed the stream; it is transient so the whole unit
// reruns against the new state.
func putStream(ctx context.Context, tx store.Tx, st model.Stream, expected model.Revision) error {
	err := tx.PutStream(ctx, st, expected)
	if model.IsCode(err, model.Conflict) {
		return model.NewError(model.Transient, "stream changed concurrently")
	}
	return err
}
