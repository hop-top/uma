package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"hop.top/uma"
	"hop.top/uma/model"
)

// OpenRequest is the part of an Open request that identifies it: uma
// fingerprints these fields, so a request that differs in any of them is a
// different command.
type OpenRequest struct {
	Start        string       `json:"start"`
	After        model.Cursor `json:"after,omitempty"`
	Filter       model.Filter `json:"filter"`
	PayloadMajor uint32       `json:"payload_major"`
}

// Marker is what a host persists after Prepare: the identity of the
// subscription and the exact request that created it. Resume needs nothing
// else.
type Marker struct {
	Tenant   model.TenantID        `json:"tenant"`
	Actor    model.ActorID         `json:"actor"`
	Consumer string                `json:"consumer"`
	Ref      model.SubscriptionRef `json:"ref"`
	Open     OpenRequest           `json:"open"` // the exact request that created the subscription
	// Operation is the operation uma recorded for that request. Resume
	// requires the replay to name it: a replay of some other operation is
	// not a replay of this one.
	Operation model.OperationRef `json:"operation"`
}

type intentKind int

const (
	intentNone intentKind = iota
	intentBootstrap
	intentResume
)

// Intent says whether Prepare creates the subscription (Bootstrap) or
// continues one a Marker records (Resume). The zero Intent is refused: a
// missing marker is never taken as a first run.
type Intent struct {
	kind   intentKind
	start  string
	after  model.Cursor
	marker Marker
}

// Bootstrap opens the subscription for the first time at start (one of the
// model.Start constants); after is the cursor for model.StartCheckpoint.
func Bootstrap(start string, after model.Cursor) Intent {
	return Intent{kind: intentBootstrap, start: start, after: after}
}

// Resume continues the subscription m records.
func Resume(m Marker) Intent { return Intent{kind: intentResume, marker: m} }

// PrepareConfig configures Prepare.
type PrepareConfig struct {
	Client       uma.Client
	Principal    model.Principal // the bot's principal; Client is bound to it (not verifiable here)
	Consumer     string
	Filter       model.Filter // Resume: must equal the marker's, or Validation
	PayloadMajor uint32       // default model.PayloadVersion1; Resume: zero or the marker's
	Intent       Intent
}

// Prepared is the outcome of Prepare. Persist Marker before consuming.
type Prepared struct {
	Marker       Marker
	Subscription model.Subscription // Ref and Checkpoint as Open returned them
	Replayed     bool               // Open replayed a recorded acceptance
}

// ErrSubscriptionContinuityLost matches *SubscriptionContinuityLost with
// errors.Is.
var ErrSubscriptionContinuityLost = errors.New("host: subscription continuity lost")

// SubscriptionContinuityLost is returned by a Resume whose Open is not a
// replay of the operation the marker records: uma no longer holds it (not
// replayed), or replayed another operation (a repeat after a lost Resume).
// Nothing was polled or acknowledged.
type SubscriptionContinuityLost struct {
	Consumer string
	// Expected is the operation the marker records; Observed is the one the
	// Open was accepted as. Replayed says whether uma replayed it.
	Expected, Observed model.OperationRef
	Replayed           bool
}

func (e *SubscriptionContinuityLost) Error() string {
	return fmt.Sprintf("host: subscription continuity lost for consumer %q: Open did not replay the marker's operation %s (observed %s, replayed %t)",
		e.Consumer, e.Expected.ID, e.Observed.ID, e.Replayed)
}

// Is reports a match with ErrSubscriptionContinuityLost.
func (e *SubscriptionContinuityLost) Is(target error) bool {
	return target == ErrSubscriptionContinuityLost
}

// Prepare opens the bot's subscription. Bootstrap sends a new Open request;
// Resume sends the marker's request again and requires uma to replay the
// operation the marker records, else it returns *SubscriptionContinuityLost. See the package comment for
// what that does and does not prove.
func Prepare(ctx context.Context, cfg PrepareConfig) (Prepared, error) {
	invalid := func(msg string) (Prepared, error) { return Prepared{}, model.NewError(model.Validation, "host: "+msg) }
	switch {
	case cfg.Client == nil:
		return invalid("client required")
	case cfg.Principal.Validate() != nil:
		return invalid("principal required")
	case cfg.Consumer == "":
		return invalid("consumer required")
	case cfg.Intent.kind == intentNone:
		return invalid("intent required: Bootstrap or Resume")
	}
	in := cfg.Intent
	var req OpenRequest
	if in.kind == intentBootstrap {
		major := cfg.PayloadMajor
		if major == 0 {
			major = model.PayloadVersion1
		}
		req = OpenRequest{Start: in.start, After: in.after, Filter: cfg.Filter, PayloadMajor: major}
	} else {
		m := in.marker
		switch {
		case m.Ref == (model.SubscriptionRef{}):
			return invalid("resume requires a recorded subscription ref")
		case m.Tenant != cfg.Principal.Tenant || m.Actor != cfg.Principal.Actor || m.Consumer != cfg.Consumer:
			return invalid("marker belongs to another tenant, actor or consumer")
		case m.Operation == (model.OperationRef{}):
			return invalid("resume requires the recorded open operation")
		case m.Open.Start == "":
			return invalid("marker records no open request")
		case !sameJSON(m.Open.Filter, cfg.Filter):
			return invalid("filter differs from the marker's; a changed filter needs a new consumer")
		case cfg.PayloadMajor != 0 && cfg.PayloadMajor != m.Open.PayloadMajor:
			return invalid("payload major differs from the marker's")
		}
		req = m.Open
	}
	open := model.OpenSubscription{
		Command:  model.Command{Call: model.Call{Correlation: "host.open"}, Operation: openOperation(cfg.Principal, cfg.Consumer, req)},
		Consumer: cfg.Consumer, Filter: req.Filter, Start: req.Start, After: req.After, PayloadMajor: req.PayloadMajor,
	}
	if err := open.Validate(); err != nil {
		return Prepared{}, err
	}
	res, err := cfg.Client.Events().Open(ctx, open)
	if err != nil {
		return Prepared{}, err
	}
	if in.kind == intentResume && (!res.Acceptance.Replayed || res.Acceptance.Operation.Ref != in.marker.Operation) {
		return Prepared{}, &SubscriptionContinuityLost{
			Consumer: cfg.Consumer, Expected: in.marker.Operation,
			Observed: res.Acceptance.Operation.Ref, Replayed: res.Acceptance.Replayed,
		}
	}
	return Prepared{
		Marker: Marker{
			Tenant: cfg.Principal.Tenant, Actor: cfg.Principal.Actor, Consumer: cfg.Consumer,
			Ref: res.Subscription.Ref, Open: req, Operation: res.Acceptance.Operation.Ref,
		},
		Subscription: res.Subscription,
		Replayed:     res.Acceptance.Replayed,
	}, nil
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// openOperation derives the Open command ID from the whole request, not the
// identity alone: uma fingerprints Start, After, Filter and PayloadMajor, so
// an ID shared by two different requests would be refused as a payload
// conflict. Bootstrap and Resume of one marker therefore send one command.
func openOperation(p model.Principal, consumer string, req OpenRequest) model.OperationID {
	body, _ := json.Marshal(req)
	h := sha256.Sum256(fmt.Appendf(nil, "%d:%s|%d:%s|%d:%s|%s", len(p.Tenant), p.Tenant, len(p.Actor), p.Actor, len(consumer), consumer, body))
	return model.OperationID("host.open." + hex.EncodeToString(h[:]))
}
