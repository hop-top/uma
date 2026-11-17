package ingress_test

import (
	"testing"
	"time"

	"hop.top/uma/model"
)

// A delivery report rides on a status envelope only, with a reportable
// state and, for a failure summary, a valid code and safe message.
func TestDeliveryReportValidation(t *testing.T) {
	h := newHarness(t, chatType)
	h.Configure(perContact(false), dest)
	readTime := t0
	status := func(d *model.DeliveryReport) model.Envelope {
		return model.Envelope{EventID: "s1", MessageID: "m1", Destination: dest, Kind: model.EnvelopeStatus, Delivery: d}
	}
	onMessage := h.Inbound("e1", "m1", "u1", dest)
	onMessage.Delivery = &model.DeliveryReport{State: model.Delivered}
	for name, env := range map[string]model.Envelope{
		"on a message":    onMessage,
		"queued":          status(&model.DeliveryReport{State: model.Queued}),
		"outcome_unknown": status(&model.DeliveryReport{State: model.Unknown}),
		"unknown code":    status(&model.DeliveryReport{State: model.Failed, Failure: &model.DeliveryFailure{Code: "nope", SafeMessage: "x"}}),
		"no safe message": status(&model.DeliveryReport{State: model.Failed, Failure: &model.DeliveryFailure{Code: model.Permanent}}),
		"read when sent":  status(&model.DeliveryReport{State: model.ProviderAccepted, ReadAt: &readTime}),
		"read when failed": status(&model.DeliveryReport{
			State: model.Failed, ReadAt: &readTime, Failure: &model.DeliveryFailure{Code: model.Permanent, SafeMessage: "x"},
		}),
		"read at no time": status(&model.DeliveryReport{State: model.Delivered, ReadAt: &time.Time{}}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.Accept(env)
			requireCode(t, err, model.Validation, "envelope.delivery")
		})
	}
	// A report naming no attempt is kept as a status and settles nothing,
	// a delivered one that was read included.
	if _, err := h.Accept(status(&model.DeliveryReport{State: model.Delivered})); err != nil {
		t.Fatalf("untagged report: %v", err)
	}
	read := status(&model.DeliveryReport{State: model.Delivered, ReadAt: &readTime})
	read.EventID = "s2"
	if _, err := h.Accept(read); err != nil {
		t.Fatalf("untagged read report: %v", err)
	}
}
