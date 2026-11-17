package relay_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

func statusOn(c *relay.Config) { c.Inbound.Status.Enabled = true }

// post sends one authenticated body to the channel's webhook handler.
func (r *engineRig) post(body string) *httptest.ResponseRecorder {
	r.t.Helper()
	h, err := r.engine.Runtime().Webhook(r.t.Context(), uma.RuntimeCall{
		Principal: model.Principal{Tenant: testTenant, Actor: "host"}, Call: model.Call{Correlation: "relay-webhook"},
	}, testChannel)
	if err != nil {
		r.t.Fatalf("Webhook: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/hooks/relay", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+webhookSecret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// update is a status update for the reply the gateway's latest callback
// carried.
func (r *engineRig) update(status string) string {
	tag := r.gw.lastTag()
	return fmt.Sprintf(`{"status":%q,"from":%q,"operation_id":%q,"attempt_id":%q,"message_id":"gw-x"}`,
		status, gatewayNumber, tag.Operation, tag.Attempt)
}

// stranded opens a rig whose first callback was applied by the gateway but
// whose answer was lost: the operation is outcome_unknown, the channel has
// no lookup, and so no reconcile pass can settle it. A status update is how
// the gateway settles it.
func stranded(t *testing.T, mutate func(*relay.Config)) (*engineRig, model.OperationRef) {
	t.Helper()
	r := newEngineRigWith(t, newLossyGateway(t, true), false, mutate)
	ref := r.start()
	r.step()
	wantState(t, r.op(ref), model.Unknown)
	return r, ref
}

// accepted posts an update and requires the webhook to take it.
func (r *engineRig) accepted(body string) {
	r.t.Helper()
	if rec := r.post(body); rec.Code != http.StatusOK {
		r.t.Fatalf("webhook answered %d for %s", rec.Code, body)
	}
}

// A status update settles an outcome_unknown reply the way the lookup would
// have: from the report of the gateway that applied it, without sending it
// again.
func TestEngineStatusSettlesALostAnswer(t *testing.T) {
	for word, want := range map[string]model.DeliveryState{
		"accepted":  model.ProviderAccepted,
		"sent":      model.ProviderAccepted,
		"delivered": model.Delivered,
		"read":      model.Delivered,
		"failed":    model.Failed,
	} {
		t.Run(word, func(t *testing.T) {
			r, ref := stranded(t, statusOn)
			r.accepted(r.update(word))
			r.step()
			op := r.op(ref)
			wantState(t, op, want)
			if want == model.Failed {
				// A report that the message did not arrive is final: the
				// reply is not sent again.
				if f := op.Failure; f == nil || f.Retry != model.NoRetry || f.Code != model.Permanent {
					t.Errorf("failure %+v", f)
				}
			}
			if posts, lookups := r.gw.counts(); posts != 1 || lookups != 0 {
				t.Errorf("%d callbacks and %d lookups, want the one, never resent", posts, lookups)
			}
		})
	}
}

// Without the route a status update is just a body the message contract
// does not accept, and the operation waits as before.
func TestEngineStatusRouteIsOptIn(t *testing.T) {
	r, ref := stranded(t, nil)
	if rec := r.post(r.update("delivered")); rec.Code != http.StatusBadRequest {
		t.Fatalf("answered %d", rec.Code)
	}
	r.settle()
	wantState(t, r.op(ref), model.Unknown)
}

// The engine applies a report to the attempt it names and guesses no other:
// an update that names no operation, an operation or attempt the engine does
// not hold, or one that is not this reply's, is taken and settles nothing.
// A line no inbox binds is refused outright.
func TestEngineStatusSettlesOnlyTheAttemptItNames(t *testing.T) {
	r, ref := stranded(t, statusOn)
	tag := r.gw.lastTag()
	const other = "00000000-0000-7000-8000-0000000000ff"
	line := `"from":"` + gatewayNumber + `"`
	for name, body := range map[string]string{
		"message id only":   `{"status":"delivered",` + line + `,"message_id":"gw-x"}`,
		"unknown operation": `{"status":"delivered",` + line + `,"operation_id":"` + other + `","attempt_id":"` + tag.Attempt + `"}`,
		"unknown attempt":   `{"status":"delivered",` + line + `,"operation_id":"` + tag.Operation + `","attempt_id":"` + other + `"}`,
	} {
		r.accepted(body)
		r.settle()
		if op := r.op(ref); op.State != model.Unknown {
			t.Fatalf("%s settled the operation: %s", name, op.State)
		}
	}
	rec := r.post(strings.Replace(r.update("delivered"), gatewayNumber, "+15005550999", 1))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a line no inbox binds: %d", rec.Code)
	}
	r.settle()
	wantState(t, r.op(ref), model.Unknown)
	// The reply it does name settles.
	r.accepted(r.update("delivered"))
	r.step()
	wantState(t, r.op(ref), model.Delivered)
}

// Once a reply has settled, later updates only move it up the order
// accepted, sent < failed < delivered < read: a late sent never undoes
// delivered or failed, a late failure never undoes a delivery, and a
// delivery that follows a failure outranks it (see Delivery status).
func TestEngineSettledOperationOnlyMovesUp(t *testing.T) {
	for _, tt := range []struct {
		first string
		want  model.DeliveryState
		later []string
		final model.DeliveryState
	}{
		{"delivered", model.Delivered, []string{"sent", "accepted", "failed", "delivered"}, model.Delivered},
		{"read", model.Delivered, []string{"sent", "failed", "delivered", "read"}, model.Delivered},
		{"failed", model.Failed, []string{"sent", "accepted", "failed"}, model.Failed},
		{"failed", model.Failed, []string{"sent", "delivered"}, model.Delivered},
		{"failed", model.Failed, []string{"read"}, model.Delivered},
		{"sent", model.ProviderAccepted, []string{"accepted", "sent"}, model.ProviderAccepted},
	} {
		t.Run(tt.first+" then "+strings.Join(tt.later, ","), func(t *testing.T) {
			r, ref := stranded(t, statusOn)
			r.accepted(r.update(tt.first))
			r.step()
			wantState(t, r.op(ref), tt.want)
			for _, word := range tt.later {
				r.accepted(r.update(word))
				r.step()
			}
			r.settle()
			wantState(t, r.op(ref), tt.final)
			if posts, _ := r.gw.counts(); posts != 1 {
				t.Errorf("%d callbacks, want the one: a report never sends again", posts)
			}
		})
	}
}

// moves are the delivery.updated events of an operation, in order.
func (r *engineRig) moves(ref model.OperationRef) []model.DeliveryUpdatedPayload {
	r.t.Helper()
	var out []model.DeliveryUpdatedPayload
	req := model.ListEvents{Call: model.Call{Correlation: "relay-events"}, Start: model.StartBeginning, Limit: 100}
	for {
		page, err := r.client.Events().List(r.t.Context(), req)
		if err != nil {
			r.t.Fatalf("Events.List: %v", err)
		}
		for _, ev := range page.Items {
			if ev.Kind != model.EventDeliveryUpdated || ev.Resource != ref.Ref() {
				continue
			}
			var p model.DeliveryUpdatedPayload
			if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
				r.t.Fatal(err)
			}
			out = append(out, p)
		}
		if page.Complete {
			return out
		}
		req.Start, req.After = model.StartCheckpoint, page.Next
	}
}

func states(ps []model.DeliveryUpdatedPayload) []model.DeliveryState {
	var out []model.DeliveryState
	for _, p := range ps {
		out = append(out, p.State)
	}
	return out
}

// answered opens a rig whose gateway took the reply and answered: the
// operation is provider_accepted, and the status route is on. Only a status
// update can say more.
func answered(t *testing.T) (*engineRig, model.OperationRef) {
	t.Helper()
	r := newEngineRigWith(t, newAnsweringGateway(t), false, statusOn)
	ref := r.start()
	r.step()
	op := r.op(ref)
	wantState(t, op, model.ProviderAccepted)
	if got := states(r.moves(ref)); !slices.Equal(got, []model.DeliveryState{model.ProviderAccepted}) {
		t.Fatalf("delivery.updated = %v after the send, want the one", got)
	}
	return r, ref
}

// A delivered update moves a reply the gateway accepted to delivered, with
// one delivery.updated event; the same update again, and a late sent, move
// nothing and announce nothing, and queue no work.
func TestEngineStatusDeliveredAdvancesAnAcceptedReply(t *testing.T) {
	r, ref := answered(t)
	body := r.update("delivered")
	r.accepted(body)
	wantState(t, r.op(ref), model.ProviderAccepted) // evidence until the worker weighs it
	r.step()
	op := r.op(ref)
	wantState(t, op, model.Delivered)
	if op.Failure != nil || op.ReadConfirmedAt != nil {
		t.Fatalf("operation %+v, want delivered, not read, no failure", op)
	}
	if got := r.moves(ref); len(got) != 2 || got[1].State != model.Delivered || got[1].PreviousState != model.ProviderAccepted ||
		got[1].Trigger != model.TriggerProvider {
		t.Fatalf("delivery.updated = %+v, want accepted then one provider-triggered delivered", got)
	}

	r.accepted(body) // the very update again: one event, claimed once
	r.accepted(r.update("sent"))
	r.accepted(r.update("accepted"))
	r.accepted(strings.Replace(body, `"message_id":"gw-x"`, `"message_id":"gw-y"`, 1)) // another event, same news
	if n := r.claimed(); n != 0 {
		t.Fatalf("%d work items for updates the reply already outranks", n)
	}
	wantState(t, r.op(ref), model.Delivered)
	if got := r.moves(ref); len(got) != 2 {
		t.Fatalf("delivery.updated = %d, want the two", len(got))
	}
	if posts, lookups := r.gw.counts(); posts != 1 || lookups != 0 {
		t.Errorf("%d callbacks and %d lookups, want the one send alone", posts, lookups)
	}
}

// read records when the message was read and keeps the reply delivered; the
// first read is the one recorded.
func TestEngineStatusReadIsRecorded(t *testing.T) {
	r, ref := answered(t)
	r.accepted(r.update("delivered"))
	r.step()
	r.clock.Advance(time.Minute)
	first := r.clock.Now()
	r.accepted(r.update("read"))
	r.step()
	op := r.op(ref)
	wantState(t, op, model.Delivered)
	if op.ReadConfirmedAt == nil || !op.ReadConfirmedAt.Equal(first) {
		t.Fatalf("read confirmed at %v, want %v", op.ReadConfirmedAt, first)
	}
	got := r.moves(ref)
	if len(got) != 3 || got[2].State != model.Delivered || got[2].PreviousState != model.Delivered ||
		got[2].ReadConfirmedAt == nil || !got[2].ReadConfirmedAt.Equal(first) {
		t.Fatalf("delivery.updated = %+v, want the read announced once, with its time", got)
	}

	r.clock.Advance(time.Hour)
	r.accepted(strings.Replace(r.update("read"), `"message_id":"gw-x"`, `"message_id":"gw-z"`, 1))
	r.step()
	if op := r.op(ref); op.ReadConfirmedAt == nil || !op.ReadConfirmedAt.Equal(first) {
		t.Fatalf("a later read moved the read time to %v", op.ReadConfirmedAt)
	}
	if n := len(r.moves(ref)); n != 3 {
		t.Fatalf("delivery.updated = %d after a second read, want the three", n)
	}
}

// A read update on its own delivers and reads in one event.
func TestEngineStatusReadAloneDeliversAndReads(t *testing.T) {
	r, ref := answered(t)
	r.accepted(r.update("read"))
	r.step()
	op := r.op(ref)
	wantState(t, op, model.Delivered)
	if op.ReadConfirmedAt == nil {
		t.Fatal("read not recorded")
	}
	if got := r.moves(ref); len(got) != 2 || got[1].ReadConfirmedAt == nil {
		t.Fatalf("delivery.updated = %+v, want accepted then one delivered with the read time", got)
	}
}

// A failed update fails a reply the gateway accepted, finally and with a
// safe summary: the gateway's code is named, its words are not, and the
// reply is not sent again.
func TestEngineStatusFailedFailsAnAcceptedReply(t *testing.T) {
	r, ref := answered(t)
	r.accepted(strings.Replace(r.update("failed"), `"message_id"`, `"code":"4012","reason":"carrier said: number is on a blocklist","message_id"`, 1))
	r.step()
	op := r.op(ref)
	wantState(t, op, model.Failed)
	f := op.Failure
	if f == nil || f.Code != model.Permanent || f.Retry != model.NoRetry ||
		f.SafeMessage != "relay reported the delivery failed (code 4012)" {
		t.Fatalf("failure %+v, want a final failure with the safe summary and the code", f)
	}
	if got := r.moves(ref); len(got) != 2 || got[1].State != model.Failed || got[1].Failure == nil || strings.Contains(got[1].Failure.SafeMessage, "blocklist") {
		t.Fatalf("delivery.updated = %+v", got)
	}
	r.settle()
	if posts, _ := r.gw.counts(); posts != 1 {
		t.Fatalf("%d callbacks, want the reply never sent again", posts)
	}
	// A delivery after a failure outranks it: the failure is cleared.
	r.accepted(r.update("delivered"))
	r.step()
	op = r.op(ref)
	wantState(t, op, model.Delivered)
	if op.Failure != nil {
		t.Errorf("failure %+v kept on a delivered reply", op.Failure)
	}
	if got := states(r.moves(ref)); !slices.Equal(got, []model.DeliveryState{model.ProviderAccepted, model.Failed, model.Delivered}) {
		t.Errorf("delivery.updated = %v", got)
	}
}

// The status route survives a restart: the update is evidence in the store,
// and a new process (here a new worker pass over the same store) moves the
// reply from it exactly once.
func TestEngineStatusEvidenceOutlivesTheProcess(t *testing.T) {
	r, ref := answered(t)
	r.accepted(r.update("delivered"))
	// A worker claims the item and dies without running it.
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: testTenant, Actor: "worker"}, Call: model.Call{Correlation: "relay-worker"}}
	if n, err := r.engine.Runtime().Step(r.t.Context(), call, uma.WorkerOptions{Owner: "gone", Limit: 10, Lease: time.Minute}); err != nil || n == 0 {
		t.Fatalf("Step = %d, %v", n, err)
	}
	r.clock.Advance(time.Hour)
	r.step()
	wantState(t, r.op(ref), model.Delivered)
	if n := len(r.moves(ref)); n != 2 {
		t.Fatalf("delivery.updated = %d, want the two", n)
	}
}

// Reports that arrive before the worker has settled the operation are
// weighed together: a delivery is proof the message arrived, so it
// outweighs a failure report whichever came first.
func TestEngineDeliveryOutweighsFailureBeforeSettling(t *testing.T) {
	for _, order := range [][]string{{"failed", "delivered"}, {"delivered", "failed"}, {"failed", "read"}} {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			r, ref := stranded(t, statusOn)
			for _, word := range order {
				r.accepted(r.update(word))
			}
			r.step()
			wantState(t, r.op(ref), model.Delivered)
		})
	}
}

// A report sent twice is one report: the second is taken and changes
// nothing.
func TestEngineStatusDuplicateIsHarmless(t *testing.T) {
	r, ref := stranded(t, statusOn)
	r.accepted(r.update("delivered"))
	r.accepted(r.update("delivered"))
	r.step()
	wantState(t, r.op(ref), model.Delivered)
	r.accepted(r.update("delivered"))
	r.step()
	wantState(t, r.op(ref), model.Delivered)
}

// Reports that arrive before the worker has settled the operation are
// ranked, not applied in the order they came: a late sent after delivered
// does not lower it, a sent after failed does not hide it, and a read is
// kept with the delivery it belongs to.
func TestEngineReportsBeforeSettlingAreRanked(t *testing.T) {
	for _, tt := range []struct {
		order []string
		want  model.DeliveryState
		read  bool
	}{
		{[]string{"delivered", "sent"}, model.Delivered, false},
		{[]string{"sent", "delivered"}, model.Delivered, false},
		{[]string{"read", "sent"}, model.Delivered, true},
		{[]string{"sent", "read", "delivered"}, model.Delivered, true},
		{[]string{"failed", "sent"}, model.Failed, false},
		{[]string{"sent", "failed"}, model.Failed, false},
		{[]string{"accepted", "sent"}, model.ProviderAccepted, false},
		{[]string{"delivered", "sent", "failed", "accepted"}, model.Delivered, false},
	} {
		t.Run(strings.Join(tt.order, ","), func(t *testing.T) {
			r, ref := stranded(t, statusOn)
			for _, word := range tt.order {
				r.accepted(r.update(word))
			}
			r.step()
			op := r.op(ref)
			wantState(t, op, tt.want)
			if (op.ReadConfirmedAt != nil) != tt.read {
				t.Errorf("read confirmed at %v, want read %v", op.ReadConfirmedAt, tt.read)
			}
			if got := states(r.moves(ref)); !slices.Equal(got, []model.DeliveryState{model.Unknown, tt.want}) {
				t.Errorf("delivery.updated = %v, want outcome_unknown then the one settle", got)
			}
		})
	}
}

// The engine tells the webhook whether an update reached an attempt: one
// naming no operation, or an operation or attempt the engine does not hold,
// is answered 200 matched:false, so a gateway with a mistyped id learns it
// without being asked to retry; an update that did reach one, and its
// duplicate, is answered as before.
func TestEngineStatusAnswerSaysWhetherItWasApplied(t *testing.T) {
	r, _ := stranded(t, statusOn)
	tag := r.gw.lastTag()
	const other = "00000000-0000-7000-8000-0000000000ff"
	line := `"from":"` + gatewayNumber + `"`
	body := func(rec *httptest.ResponseRecorder) string {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("answered %d", rec.Code)
		}
		return strings.TrimSpace(rec.Body.String())
	}
	const unmatched = `{"status":"accepted","matched":false}`
	for name, b := range map[string]string{
		"message id only":   `{"status":"delivered",` + line + `,"message_id":"gw-x"}`,
		"unknown operation": `{"status":"delivered",` + line + `,"operation_id":"` + other + `","attempt_id":"` + tag.Attempt + `"}`,
		"unknown attempt":   `{"status":"delivered",` + line + `,"operation_id":"` + tag.Operation + `","attempt_id":"` + other + `"}`,
	} {
		if got := body(r.post(b)); got != unmatched {
			t.Errorf("%s: answered %s, want %s", name, got, unmatched)
		}
		// Sent again, it is the same event and the same answer.
		if got := body(r.post(b)); got != unmatched {
			t.Errorf("%s again: answered %s, want %s", name, got, unmatched)
		}
	}
	const applied = `{"status":"accepted"}`
	if got := body(r.post(r.update("delivered"))); got != applied {
		t.Errorf("a named attempt: answered %s, want %s", got, applied)
	}
	if got := body(r.post(r.update("delivered"))); got != applied {
		t.Errorf("the same update again: answered %s, want %s", got, applied)
	}
	// A report the reply already outranks still reached its attempt.
	r.step()
	if got := body(r.post(r.update("sent"))); got != applied {
		t.Errorf("a late sent: answered %s, want %s", got, applied)
	}
}
