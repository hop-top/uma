package twilio

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/staging"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// MaxMedia bounds the media entries read from one incoming message.
const MaxMedia = 10

// MediaRequest describes the media of one incoming message to stage.
type MediaRequest struct {
	// AccountSID is the channel's account; MessageSID the incoming
	// message's SID, whose Media resources the URLs must name.
	AccountSID, MessageSID string
	// Destination and Audience are those of the message's envelope.
	Destination string
	Audience    model.Audience
	// Form holds NumMedia, MediaUrl{n} and MediaContentType{n}.
	Form url.Values
	// Credential resolves the REST credential. It is called once, before
	// the first download, and not at all when no URL qualifies.
	Credential func(context.Context) (ports.Credential, error)
	// Skipped, when set, is told of each media whose download failed for
	// good, so the message can say it is unavailable and why.
	Skipped func(MediaGap)
}

// StageMedia stages the media of an incoming MMS or WhatsApp message
// through stager and returns one attachment per staged item, in order.
// Each item is downloaded from its Media resource on the API origin with
// the REST credential and staged under its media SID, so a redelivery
// restages the same attachment. At most MaxMedia items are read.
//
// It returns an error only for a failure worth a redelivery (Twilio
// unreachable, a rate limit or server fault, the credential or the staging
// sink failing transiently); then the attachments staged so far are
// dropped with it. Media that cannot be staged for good (refused by the
// engine's checks, no blob store, a URL off the account's Media resources,
// gone at Twilio, a credential Twilio refuses, a redirect refused) is
// skipped; a download that failed for good is also reported to r.Skipped.
func (c *Client) StageMedia(ctx context.Context, stager adapter.AttachmentSink, r MediaRequest) ([]model.AttachmentRef, error) {
	n, err := strconv.Atoi(r.Form.Get("NumMedia"))
	if err != nil || n <= 0 {
		return nil, nil
	}
	var (
		refs []model.AttachmentRef
		cred ports.Credential
		// denied: the credential is unusable, so no further media of this
		// message is tried.
		denied bool
	)
	for k := 0; k < n && k < MaxMedia; k++ {
		key := strconv.Itoa(k)
		sid, ok := c.MediaSID(r.AccountSID, r.MessageSID, r.Form.Get("MediaUrl"+key))
		if !ok {
			continue
		}
		if cred == nil && !denied {
			if cred, err = r.Credential(ctx); err != nil {
				if err := retryOnly(err); err != nil {
					return nil, err
				}
				denied = true
			}
		}
		if denied {
			r.skipped(k, GapUnauthorized)
			continue
		}
		body, typ, err := c.Media(ctx, r.AccountSID, cred, r.MessageSID, sid)
		if err != nil {
			if err := retryOnly(mediaFailure(err)); err != nil {
				return nil, err
			}
			reason := gapReason(err)
			denied = reason == GapUnauthorized
			r.skipped(k, reason)
			continue
		}
		if t := r.Form.Get("MediaContentType" + key); t != "" {
			typ = t
		}
		ref, outcome, err := staging.Stage(ctx, stager, adapter.StageRequest{
			Key: sid, Destination: r.Destination,
			Metadata: model.AttachmentMetadata{Name: "media-" + key, MediaType: typ, Audience: r.Audience},
		}, body)
		_ = body.Close()
		switch outcome {
		case staging.Staged:
			refs = append(refs, ref)
		case staging.Retry:
			return nil, err
		}
	}
	return refs, nil
}

func (r MediaRequest) skipped(k int, reason string) {
	if r.Skipped != nil {
		r.Skipped(MediaGap{Index: k, Reason: reason})
	}
}

// mediaFailure maps a failed download: no answer, a rate limit or a server
// fault is transient; anything else is permanent.
func mediaFailure(err error) error {
	var ae *APIError
	var re *RequestError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.As(err, &re):
		return model.NewError(model.Transient, "twilio media unreachable")
	case errors.As(err, &ae) && (ae.Status >= 500 || ae.Status == 429 || ae.Status == 408):
		return model.NewError(model.Transient, "twilio media temporarily unavailable")
	}
	return model.NewError(model.Permanent, "twilio media unavailable")
}

// retryOnly returns err when a redelivery may fix it, nil otherwise.
func retryOnly(err error) error {
	if staging.Retryable(err) {
		return err
	}
	return nil
}
