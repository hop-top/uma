package twilio

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
	"hop.top/uma/ports"
)

// ErrAmbiguous: a listing cannot single out the message, because several
// match or the listing could not be read to its end.
var ErrAmbiguous = errors.New("twilio: sent message ambiguous")

// Listing bounds: Twilio's maximum page size, and the pages read before
// giving up as ambiguous.
const (
	listPageSize = 1000
	listMaxPages = 10
)

// SentMessage describes a message an attempt created, to find it again
// when the create answer was lost.
type SentMessage struct {
	To   string
	From string
	// MessagingServiceSID identifies a Messaging Service send when From is
	// empty: the service picks the number.
	MessagingServiceSID string
	Body                string
	// Since is when the attempt started; messages created earlier are
	// someone else's.
	Since time.Time
}

// FindSent looks for the message s describes: an outbound message to s.To
// from s.From (or through s.MessagingServiceSID) with exactly s.Body,
// created at or after s.Since. found is false when none matches. More than
// one match, an unreadable creation date on a candidate, or more pages than
// it reads are ErrAmbiguous: the caller must not guess.
//
// The listing filters by To and From only, newest first. It never filters
// on DateSent: a queued or accepted message has no date_sent yet, so the
// filter would hide the very send being looked for. Creation time is
// checked here on date_created, and paging stops after the first page
// holding a message created before Since: later pages are older still.
func (c *Client) FindSent(ctx context.Context, accountSID string, cred ports.Credential, s SentMessage) (Message, bool, error) {
	q := url.Values{}
	q.Set("To", s.To)
	if s.From != "" {
		q.Set("From", s.From)
	}
	q.Set("PageSize", strconv.Itoa(listPageSize))
	path := "/Messages.json?" + q.Encode()

	var match Message
	var n int
	for range listMaxPages {
		var page struct {
			Messages    []Message `json:"messages"`
			NextPageURI string    `json:"next_page_uri"`
		}
		if err := c.do(ctx, http.MethodGet, accountSID, path, cred, nil, &page); err != nil {
			return Message{}, false, err
		}
		older := false
		for _, m := range page.Messages {
			ok, err := s.matches(m)
			if err != nil {
				return Message{}, false, err
			}
			if ok {
				match, n = m, n+1
			}
			older = older || s.predates(m)
		}
		if n > 1 {
			return Message{}, false, ErrAmbiguous
		}
		if older || page.NextPageURI == "" {
			return match, n == 1, nil
		}
		next, ok := c.nextPage(accountSID, page.NextPageURI)
		if !ok {
			return Message{}, false, ErrAmbiguous
		}
		path = next
	}
	return Message{}, false, ErrAmbiguous
}

// predates reports whether m was created before the attempt; an
// unreadable creation date does not.
func (s SentMessage) predates(m Message) bool {
	created, err := time.Parse(time.RFC1123Z, m.DateCreated)
	return err == nil && created.Before(s.Since.Truncate(time.Second))
}

func (s SentMessage) matches(m Message) (bool, error) {
	if m.To != s.To || m.Body != s.Body || !strings.HasPrefix(m.Direction, "outbound") {
		return false, nil
	}
	if s.From != "" && m.From != s.From {
		return false, nil
	}
	if s.From == "" && m.MessagingServiceSID != s.MessagingServiceSID {
		return false, nil
	}
	created, err := time.Parse(time.RFC1123Z, m.DateCreated)
	if err != nil {
		// A same-body candidate of unknown age: neither a match nor safely
		// excluded.
		return false, ErrAmbiguous
	}
	return !created.Before(s.Since.Truncate(time.Second)), nil
}

// nextPage returns the account-relative path of a next_page_uri, which
// must stay on this account's Messages list.
func (c *Client) nextPage(accountSID, uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.IsAbs() || u.Host != "" {
		return "", false
	}
	rest, ok := strings.CutPrefix(u.Path, "/2010-04-01/Accounts/"+accountSID)
	if !ok || rest != "/Messages.json" {
		return "", false
	}
	return rest + "?" + u.RawQuery, true
}

// ReconcileOutcome maps a FindSent result on channel ch to a reconcile
// outcome: the found message's state and SID; failed when none exists,
// so a new attempt is safe; outcome_unknown (inconclusive) when ambiguous.
// Read failures are retryable except refused credentials: listing is safe
// to repeat.
//
// A found message Twilio failed is rejected, final: the engine reads
// failed from a reconciliation as "nothing applied". Twilio keeps a create
// it refuses (such as error 21266, To equal to From) as a failed message,
// so the lost answer of a refusal is found this way, as is a message it
// could not deliver.
func ReconcileOutcome(ch model.ChannelRef, m Message, found bool, err error) (adapter.Outcome, error) {
	switch {
	case errors.Is(err, ErrAmbiguous):
		return adapter.Outcome{State: model.Unknown}, nil
	case err != nil:
		return adapter.Outcome{}, readFailure(err)
	case !found:
		return adapter.Outcome{State: model.Failed, Failure: &model.Error{
			Code: model.Transient, SafeMessage: "Twilio holds no matching message", Retry: model.RetrySameOperation,
		}}, nil
	}
	out, err := SendOutcome(ch, m, nil)
	if err == nil && out.State == model.Failed {
		out.State = model.Rejected
	}
	return out, err
}

func readFailure(err error) error {
	var me *model.Error
	if errors.As(err, &me) {
		return me
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == http.StatusTooManyRequests || ae.Code == CodeTooManyRequests:
			return &model.Error{Code: model.RateLimited, SafeMessage: "Twilio rate limit", Retry: model.RetrySameOperation, RetryAfter: ae.RetryAfter}
		case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden || ae.Code == CodePermissionDenied:
			return &model.Error{Code: model.Unauthorized, SafeMessage: "Twilio refused the credentials", Retry: model.NoRetry}
		case ae.Status < 500:
			return &model.Error{Code: model.Permanent, SafeMessage: "Twilio refused the listing (HTTP " + strconv.Itoa(ae.Status) + ")", Retry: model.NoRetry}
		}
	}
	return model.NewError(model.Transient, "Twilio listing failed")
}
