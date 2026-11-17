package runtime

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// maxProtocolKey bounds a protocol state key, in bytes.
const maxProtocolKey = 256

// reservedProtocolPrefix starts the protocol keys the engine keeps for
// itself on a channel, such as the receiver cursor. An adapter can neither
// read nor write them.
const reservedProtocolPrefix = model.CoreNamespace + "."

// protocolState is the adapter.ProtocolState an instance receives: the
// store's protocol records of its own channel, one key per unit of work.
// Every revision is the engine's; the adapter only compares them.
type protocolState struct {
	e       *engine.Engine
	channel model.ChannelRef
}

func newProtocolState(e *engine.Engine, ch model.ChannelRef) adapter.ProtocolState {
	return protocolState{e: e, channel: ch}
}

func (p protocolState) Load(ctx context.Context, key string) (model.Extension, model.Revision, error) {
	if err := checkProtocolKey(key); err != nil {
		return model.Extension{}, "", err
	}
	var st store.ProtocolState
	err := p.e.Store().View(ctx, p.channel.Tenant, func(r store.Reader) error {
		var err error
		st, err = r.Protocol(ctx, p.channel, key)
		return err
	})
	if err != nil {
		return model.Extension{}, "", protocolError(ctx, err)
	}
	return st.Value.Clone(), st.Revision, nil
}

func (p protocolState) CompareAndSwap(ctx context.Context, key string, expected model.Revision, value model.Extension) error {
	if err := checkProtocolKey(key); err != nil {
		return err
	}
	if err := value.Validate(); err != nil {
		return err
	}
	rev, err := operations.NewRevision(ctx, p.e)
	if err != nil {
		return err
	}
	next := store.ProtocolState{Channel: p.channel, Key: key, Value: value.Clone(), Revision: rev}
	err = operations.Transact(ctx, p.e, p.channel.Tenant, func(tx store.Tx) error {
		return tx.PutProtocol(ctx, next, expected)
	})
	return protocolError(ctx, err)
}

// checkProtocolKey requires a non-empty, bounded UTF-8 key outside the
// engine's reserved prefix.
func checkProtocolKey(key string) error {
	switch {
	case key == "":
		return model.NewError(model.Validation, "protocol key: required")
	case len(key) > maxProtocolKey || !utf8.ValidString(key):
		return model.NewError(model.Validation, "protocol key: must be UTF-8 within 256 bytes")
	case strings.HasPrefix(key, reservedProtocolPrefix):
		return &model.Error{Code: model.Unauthorized, SafeMessage: "protocol key reserved by the engine", Retry: model.NoRetry}
	}
	return nil
}

// protocolError keeps nil, context errors and typed store errors, and
// hides anything else.
func protocolError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	var me *model.Error
	if errors.As(err, &me) && me.Code.Valid() {
		return cloneError(me)
	}
	return model.NewError(model.Unavailable, "protocol state unavailable")
}
