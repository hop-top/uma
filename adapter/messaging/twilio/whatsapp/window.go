package whatsapp

import (
	"context"
	"encoding/json"
	"time"

	"hop.top/uma/model"
)

// window is the protocol state of one user: when the latest verified
// inbound message reached the adapter. Twilio's incoming webhook carries no
// send time, so receipt time stands in for it.
type window struct {
	LastInbound int64 `json:"last_inbound"`
}

func windowKey(user string) string { return "window/" + user }

// windowOpen reports whether a free-form message to user is inside the
// customer service window. known is false when the instance has no
// protocol state or has recorded no inbound message of the user, which may
// predate the recording, so only Twilio can tell.
func (i *Instance) windowOpen(ctx context.Context, user string) (open, known bool, err error) {
	if i.protocol == nil {
		return false, false, nil
	}
	w, _, err := i.loadWindow(ctx, user)
	if err != nil {
		return false, true, err
	}
	if w.LastInbound == 0 {
		return false, false, nil
	}
	return i.clock.Now().Sub(time.Unix(w.LastInbound, 0)) < ServiceWindow, true, nil
}

func (i *Instance) loadWindow(ctx context.Context, user string) (window, model.Revision, error) {
	ext, rev, err := i.protocol.Load(ctx, windowKey(user))
	switch {
	case model.IsCode(err, model.NotFound):
		return window{}, "", nil
	case err != nil:
		return window{}, "", err
	case ext.Name != WindowSchema || ext.Version != WindowVersion:
		// Absent or unreadable state counts as no window: Twilio remains
		// the backstop.
		return window{}, rev, nil
	}
	var w window
	if json.Unmarshal(ext.Data, &w) != nil {
		return window{}, rev, nil
	}
	return w, rev, nil
}

// recordInbound moves user's window forward to at. It never moves it back.
func (i *Instance) recordInbound(ctx context.Context, user string, at time.Time) error {
	if i.protocol == nil {
		return nil
	}
	var err error
	for range 4 {
		var w window
		var rev model.Revision
		if w, rev, err = i.loadWindow(ctx, user); err != nil {
			return err
		}
		if w.LastInbound >= at.Unix() {
			return nil
		}
		data, _ := json.Marshal(window{LastInbound: at.Unix()})
		err = i.protocol.CompareAndSwap(ctx, windowKey(user), rev, model.Extension{Name: WindowSchema, Version: WindowVersion, Data: data})
		if !model.IsCode(err, model.Conflict) {
			return err
		}
	}
	return err
}

// windowClosed is the failure of a free-form message outside the customer
// service window, whether the adapter or Twilio refused it.
func windowClosed() *model.Error {
	return &model.Error{Code: model.Permanent, SafeMessage: "outside the 24-hour customer service window: send a template message", Retry: model.NoRetry}
}
