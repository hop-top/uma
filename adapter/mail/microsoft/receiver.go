package microsoft

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Run is the channel's receiver. Each round it keeps the change
// notification subscription alive (when a notification URL is configured)
// and then runs delta tracking on the inbox from the stored delta link, so
// messages whose notifications were missed, throttled or dropped still
// arrive. Rounds repeat after the poll interval (+-10%, so channels do not
// poll in phase), before the subscription's
// renewal time, or as soon as the webhook signals a lifecycle event.
//
// The delta link advances only after every message of a page was accepted:
// a message the sink refuses is fetched again next round. A sink error ends
// Run with that error.
func (i *instance) Run(ctx context.Context, sink adapter.Sink) error {
	for {
		next, err := i.round(ctx, sink)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-i.state.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// round runs one subscription check and one delta round and returns the
// wait before the next round.
func (i *instance) round(ctx context.Context, sink adapter.Sink) (time.Duration, error) {
	s := i.c.session()
	st, err := i.loadState(ctx)
	if err != nil {
		return 0, err
	}
	wait := i.spread(i.cfg.pollInterval())
	if i.cfg.NotificationURL != "" {
		expires := st.SubscriptionExpires
		next, err := i.maintainSubscription(ctx, s, &st, i.state.takeLifecycle())
		switch {
		case err != nil && ctx.Err() != nil:
			return 0, ctx.Err()
		case err != nil:
			// Delta tracking still covers the mailbox; the subscription is
			// retried next round. The host is told push is failing.
			i.health.fail(subscriptionFailureCode(err), permanentFailure(err), expiresBefore(expires, i.now().Add(wait)))
		default:
			i.health.ok()
			if err := i.saveState(ctx, st); err != nil {
				return 0, err
			}
			if d := next.Sub(i.now()); d < wait {
				wait = max(d, time.Second)
			}
		}
	}
	fresh := map[string]bool{}
	if err := i.syncDelta(ctx, s, sink, &st, fresh); err != nil {
		return wait, err
	}
	err = i.syncFolders(ctx, s, sink, &st, fresh)
	if err == nil {
		i.link.set(true)
	}
	return wait, err
}

// clientStateError is a failure to derive the subscription's client state.
type clientStateError struct{ err error }

func (e clientStateError) Error() string { return e.err.Error() }
func (e clientStateError) Unwrap() error { return e.err }

// subscriptionFailureCode is the degradation code of a failed subscription
// create or renewal: transient, a missing notification secret, or Graph's
// refusal.
func subscriptionFailureCode(err error) string {
	var cs clientStateError
	switch {
	case !permanentFailure(err):
		return CodeSubscriptionUnavailable
	case errors.As(err, &cs):
		return CodeClientStateMissing
	}
	return CodeSubscriptionRejected
}

// syncDelta runs delta rounds until Graph returns a delta link. fresh
// collects the IDs of the messages the round listed.
func (i *instance) syncDelta(ctx context.Context, s *session, sink adapter.Sink, st *receiverState, fresh map[string]bool) error {
	link := st.DeltaLink
	if link == "" || !i.c.sameOrigin(link) {
		link = i.initialDelta()
	}
	restarted := false
	for {
		resp, err := s.do(ctx, request{
			method: "GET", url: link,
			prefer: []string{preferImmutableID, "odata.maxpagesize=" + strconv.Itoa(i.f.opts.PageSize)},
		})
		if err != nil && resp.status == http.StatusGone && !restarted {
			// The delta token expired: start a new bounded round.
			link, restarted = i.initialDelta(), true
			continue
		}
		if err != nil {
			return transient(err)
		}
		var page gDeltaPage
		if err := decode(resp.body, &page); err != nil {
			return err
		}
		for _, item := range page.Value {
			if item.Removed != nil || item.ID == "" {
				continue
			}
			fresh[item.ID] = true
			in, skip, err := i.fetchIngress(ctx, s, sink, item.ID)
			if err != nil {
				return err
			}
			if skip {
				continue
			}
			if _, err := sink.Accept(ctx, in); err != nil {
				return err
			}
		}
		next := page.NextLink
		if next == "" {
			next = page.DeltaLink
		}
		if next == "" || !i.c.sameOrigin(next) {
			return &model.Error{Code: model.Permanent, SafeMessage: "graph delta page without a usable link", Retry: model.NoRetry}
		}
		st.DeltaLink = next
		if err := i.saveState(ctx, *st); err != nil {
			return err
		}
		if page.NextLink == "" {
			return nil
		}
		link = next
	}
}

// initialDelta is the first delta request of a round with no stored link:
// created messages of the inbox received within the initial sync window.
// Graph encodes these options into the links it returns.
func (i *instance) initialDelta() string {
	since := i.now().Add(-i.cfg.initialSyncWindow()).UTC().Format(time.RFC3339)
	q := url.Values{
		"changeType": {"created"},
		"$select":    {"id"},
		"$filter":    {"receivedDateTime ge " + since},
	}
	return i.c.api(i.cfg.mailboxPath()+"/mailFolders/inbox/messages/delta", q)
}

// renewMargin is how long before expiry a subscription is renewed: a third
// of its lifetime, between 15 minutes and a day.
func renewMargin(lifetime time.Duration) time.Duration {
	return min(max(lifetime/3, 15*time.Minute), 24*time.Hour)
}

// maintainSubscription creates the inbox subscription when there is none,
// it expired or Graph removed it, and renews it once inside the renewal
// margin or when Graph asked for reauthorization (a renewal reauthorizes
// too). It returns when the subscription next needs renewing.
func (i *instance) maintainSubscription(ctx context.Context, s *session, st *receiverState, work int) (time.Time, error) {
	now := i.now()
	lifetime := i.cfg.subscriptionLifetime()
	margin := renewMargin(lifetime)
	switch {
	case work&lifecycleRecreate != 0 || st.SubscriptionID == "" || !now.Before(st.SubscriptionExpires):
		if err := i.createSubscription(ctx, s, st, now.Add(lifetime)); err != nil {
			return time.Time{}, err
		}
		st.SubscriptionRenewAt = st.SubscriptionExpires.Add(-i.spread(margin))
	case work&lifecycleRenew != 0 || !now.Before(i.renewDue(*st, margin)):
		err := i.renewSubscription(ctx, s, st, now.Add(lifetime))
		if model.IsCode(err, model.NotFound) {
			err = i.createSubscription(ctx, s, st, now.Add(lifetime))
		}
		if err != nil {
			return time.Time{}, err
		}
		st.SubscriptionRenewAt = st.SubscriptionExpires.Add(-i.spread(margin))
	}
	return i.renewDue(*st, margin), nil
}

// jitterFraction is the spread of the poll wait and of the renewal margin:
// +-10%.
const jitterFraction = 0.1

// spread is d +-10% (adapter.Spread) from the factory's jitter source.
func (i *instance) spread(d time.Duration) time.Duration {
	return adapter.Spread(d, jitterFraction, i.f.opts.Jitter)
}

// renewDue is when the subscription in st is renewed: its SubscriptionRenewAt,
// or expiry less margin when none is stored.
func (i *instance) renewDue(st receiverState, margin time.Duration) time.Time {
	if !st.SubscriptionRenewAt.IsZero() {
		return st.SubscriptionRenewAt
	}
	return st.SubscriptionExpires.Add(-margin)
}

func (i *instance) createSubscription(ctx context.Context, s *session, st *receiverState, expires time.Time) error {
	cs, err := i.clientState(ctx)
	if err != nil {
		return clientStateError{err}
	}
	if st.SubscriptionID != "" {
		i.deleteSubscription(ctx, s, st.SubscriptionID)
		st.SubscriptionID, st.SubscriptionExpires = "", time.Time{}
	}
	body := encode(gSubscription{
		ChangeType: "created", NotificationURL: i.cfg.NotificationURL, LifecycleNotificationURL: i.cfg.lifecycleURL(),
		Resource: i.cfg.subscriptionResource(), ExpirationDateTime: expires.UTC(), ClientState: cs,
	})
	post := request{method: "POST", url: i.c.api("/subscriptions", nil), body: body, contentType: "application/json", prefer: []string{preferImmutableID}}
	resp, err := s.do(ctx, post)
	if model.IsCode(err, model.Conflict) {
		// Graph refuses a second subscription on the same resource and
		// change type: drop the one a previous receiver left (its client
		// state and lifecycle URL are unknown) and subscribe again.
		if err := i.dropDuplicates(ctx, s); err != nil {
			return err
		}
		resp, err = s.do(ctx, post)
	}
	if err != nil {
		return transient(err)
	}
	var sub gSubscription
	if err := decode(resp.body, &sub); err != nil {
		return err
	}
	if sub.ID == "" {
		return &model.Error{Code: model.Permanent, SafeMessage: "graph created a subscription without an ID", Retry: model.NoRetry}
	}
	st.SubscriptionID, st.SubscriptionExpires = sub.ID, expiryOr(sub.ExpirationDateTime, expires)
	return nil
}

func (i *instance) renewSubscription(ctx context.Context, s *session, st *receiverState, expires time.Time) error {
	resp, err := s.do(ctx, request{
		method: "PATCH", url: i.c.api("/subscriptions/"+url.PathEscape(st.SubscriptionID), nil),
		body: encode(gSubscription{ExpirationDateTime: expires.UTC()}), contentType: "application/json",
	})
	if err != nil {
		return transient(err)
	}
	var sub gSubscription
	if err := decode(resp.body, &sub); err != nil {
		return err
	}
	st.SubscriptionExpires = expiryOr(sub.ExpirationDateTime, expires)
	return nil
}

// dropDuplicates deletes this application's subscriptions on the inbox
// resource with the configured notification URL.
func (i *instance) dropDuplicates(ctx context.Context, s *session) error {
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/subscriptions", nil)})
	if err != nil {
		return transient(err)
	}
	var page gSubscriptionPage
	if err := decode(resp.body, &page); err != nil {
		return err
	}
	want := strings.TrimPrefix(i.cfg.subscriptionResource(), "/")
	for _, sub := range page.Value {
		if strings.EqualFold(strings.TrimPrefix(sub.Resource, "/"), want) && sub.NotificationURL == i.cfg.NotificationURL {
			i.deleteSubscription(ctx, s, sub.ID)
		}
	}
	return nil
}

// deleteSubscription removes a subscription, best effort.
func (i *instance) deleteSubscription(ctx context.Context, s *session, id string) {
	if id == "" {
		return
	}
	_, _ = s.do(ctx, request{method: "DELETE", url: i.c.api("/subscriptions/"+url.PathEscape(id), nil)})
}

func expiryOr(got, want time.Time) time.Time {
	if got.IsZero() {
		return want
	}
	return got
}
