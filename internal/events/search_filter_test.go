package events_test

import (
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// Subscriptions reject the message search fields with Validation before
// anything is recorded; they select events, not messages.
func TestOpenRejectsSearchFields(t *testing.T) {
	h := newEvHarness(t, nil)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	yes := true
	for field, f := range map[string]model.Filter{
		"text":        {Text: "invoice"},
		"since":       {Since: &t0},
		"before":      {Before: &t0},
		"attachments": {Attachments: &model.AttachmentFilter{Has: &yes}},
	} {
		t.Run(field, func(t *testing.T) {
			_, err := h.bot.Events().Open(h.ctx, h.openReq("bot-"+field, model.StartTail, f))
			enginetest.RequireCode(t, err, model.Validation, "filter."+field+": not supported by subscriptions")
		})
	}
}
