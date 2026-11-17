package ingress

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// CursorKey is the protocol state key under which the engine keeps a
// channel's receiver cursor. Adapters cannot reach engine keys.
const CursorKey = model.CoreNamespace + ".receiver.cursor"

// maxCursorBytes bounds a receiver cursor.
const maxCursorBytes = 1024

// cursorSchema is the value of CursorKey.
const cursorSchema = model.CoreNamespace + ".receiver.cursor"

type cursorValue struct {
	Cursor string `json:"cursor"`
}

func checkCursor(c string) error {
	if len(c) > maxCursorBytes || !utf8.ValidString(c) {
		return model.NewError(model.Validation, "receiver_cursor: must be UTF-8 within 1024 bytes")
	}
	return nil
}

// ReceiverCursor returns the cursor the latest accepted ingress of the
// bound channel carried, or "" before any.
func (s *Sink) ReceiverCursor(ctx context.Context) (string, error) {
	done, err := s.e.Begin()
	if err != nil {
		return "", err
	}
	defer done()
	var st store.ProtocolState
	err = s.e.Store().View(ctx, s.channel.Ref.Tenant, func(r store.Reader) error {
		var err error
		st, err = r.Protocol(ctx, s.channel.Ref, CursorKey)
		return err
	})
	switch {
	case model.IsCode(err, model.NotFound):
		return "", nil
	case err != nil:
		return "", err
	}
	var v cursorValue
	if json.Unmarshal(st.Value.Data, &v) != nil {
		return "", nil
	}
	return v.Cursor, nil
}

// saveCursor records the ingress's receiver cursor in the unit, when it
// carries one.
func (u *ingressUnit) saveCursor() error {
	if u.in.ReceiverCursor == "" {
		return nil
	}
	var prev model.Revision
	cur, err := u.tx.Protocol(u.ctx, u.s.channel.Ref, CursorKey)
	switch {
	case err == nil:
		prev = cur.Revision
	case !model.IsCode(err, model.NotFound):
		return err
	}
	rev, err := u.newRevision()
	if err != nil {
		return err
	}
	data, _ := json.Marshal(cursorValue{Cursor: u.in.ReceiverCursor})
	return u.tx.PutProtocol(u.ctx, store.ProtocolState{
		Channel: u.s.channel.Ref, Key: CursorKey,
		Value:    model.Extension{Name: cursorSchema, Version: 1, Data: data},
		Revision: rev,
	}, prev)
}

var _ adapter.CursorSink = (*Sink)(nil)
