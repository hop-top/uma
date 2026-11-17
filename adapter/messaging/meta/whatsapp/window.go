package whatsapp

import (
	"context"
	"encoding/json"
	"time"

	"hop.top/uma/model"
)

// window is the protocol state of one user: the time of the latest
// verified inbound message, as Meta stamped it.
type window struct {
	LastInbound int64 `json:"last_inbound"`
}

func windowKey(user string) string { return "window/" + user }

// windowOpen reports whether a free-form message to user is inside the
// customer service window. known is false when the instance has no
// protocol state or has recorded no inbound message of the user, which
// may predate the recording, so only Meta can tell.
func (x *Instance) windowOpen(ctx context.Context, user string) (open, known bool, err error) {
	if x.protocol == nil {
		return false, false, nil
	}
	w, _, err := x.loadWindow(ctx, user)
	if err != nil {
		return false, true, err
	}
	if w.LastInbound == 0 {
		return false, false, nil
	}
	return x.now().Sub(time.Unix(w.LastInbound, 0)) < ServiceWindow, true, nil
}

func (x *Instance) loadWindow(ctx context.Context, user string) (window, model.Revision, error) {
	ext, rev, err := x.protocol.Load(ctx, windowKey(user))
	switch {
	case model.IsCode(err, model.NotFound):
		return window{}, "", nil
	case err != nil:
		return window{}, "", err
	case ext.Name == "":
		return window{}, rev, nil
	case ext.Name != WindowSchema || ext.Version != WindowVersion:
		// Unreadable state counts as no window: Meta remains the backstop.
		return window{}, rev, nil
	}
	var w window
	if json.Unmarshal(ext.Data, &w) != nil {
		return window{}, rev, nil
	}
	return w, rev, nil
}

// recordInbound moves user's window forward to at. It never moves it back.
func (x *Instance) recordInbound(ctx context.Context, user string, at time.Time) error {
	if x.protocol == nil {
		return nil
	}
	var err error
	for range 4 {
		var w window
		var rev model.Revision
		if w, rev, err = x.loadWindow(ctx, user); err != nil {
			return err
		}
		if w.LastInbound >= at.Unix() {
			return nil
		}
		data, _ := json.Marshal(window{LastInbound: at.Unix()})
		err = x.protocol.CompareAndSwap(ctx, windowKey(user), rev, model.Extension{Name: WindowSchema, Version: WindowVersion, Data: data})
		if !model.IsCode(err, model.Conflict) {
			return err
		}
	}
	return err
}
