package github_test

import (
	"strconv"
	"strings"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

var _ conformance.HistoryProvider = (*provider)(nil)

// HoldHistory makes ev a comment on its sender's issue, created and last
// updated at at: the comment of the same ID when the webhook delivered it
// already. The issue is opened, without a description, a week before, so
// only its comments fall in the suite's window.
func (p *provider) HoldHistory(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	id, ok := strings.CutPrefix(ev.MessageID, "comment:")
	cid, err := strconv.ParseInt(id, 10, 64)
	author, err2 := strconv.ParseInt(ev.Sender, 10, 64)
	if !ok || err != nil || err2 != nil || ev.Kind != conformance.InboundMessage {
		return conformance.ErrNotExpressible
	}
	pre := p.prefix(ch)
	n, _ := strconv.Atoi(p.Route(ch, ev.Sender).ID)
	if !p.gh.hasIssue(pre, sandboxRepo, n) {
		p.gh.putIssue(pre, sandboxRepo, n, 5000001, "", at.Add(-7*24*time.Hour), false)
	}
	p.gh.putComment(pre, fakeComment{
		ID: cid, Issue: sandboxRepo + "#" + strconv.Itoa(n), Body: ev.Text, UserID: author, Login: "user-" + ev.Sender,
		CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	})
	return nil
}
