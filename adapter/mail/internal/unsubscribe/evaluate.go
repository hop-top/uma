package unsubscribe

import (
	"bytes"
	"context"
	"errors"

	"hop.top/uma/adapter/mail/mime"
)

// Options are the seams of Evaluate; the zero value is production.
type Options struct {
	// Public judges the host of an https URI; PublicHost when nil.
	Public func(host string) bool
	// Resolver looks up DKIM keys; SystemResolver when nil.
	Resolver Resolver
	// AnyPort lifts the default-port rule of the https URI, for a test
	// stand-in that listens elsewhere.
	AnyPort bool
}

// Evaluate decides the method for the raw message and, for one-click,
// checks its DKIM signature, which is the only step that reaches the network
// (DNS). It returns the plan, or the final refusal; temporary reports a
// refusal that was a DNS failure, which a later attempt may not meet.
func Evaluate(ctx context.Context, raw []byte, a Action, o Options) (plan Plan, ref *Refusal, temporary bool) {
	msg, err := mime.Parse(bytes.NewReader(raw), mime.Limits{})
	if err != nil && !errors.Is(err, mime.ErrLimit) {
		return Plan{}, refuse("the message cannot be read"), false
	}
	h := Headers{
		Unsubscribe: msg.Header.Values(HeaderUnsubscribe),
		Post:        msg.Header.Values(HeaderPost),
	}
	for _, f := range msg.Header.From {
		h.From = append(h.From, f.Address)
	}
	plan, ref = Decide(h, a, o)
	if ref != nil || plan.Method != MethodOneClick {
		return plan, ref, false
	}
	from, ref := senderDomain(h.From)
	if ref != nil {
		return Plan{}, ref, false
	}
	if ref, temporary = CheckDKIM(ctx, raw, from, o.Resolver); ref != nil {
		return Plan{}, ref, temporary
	}
	return plan, nil, false
}
