package conformance

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Replay scenarios commit state, call cross, then replay claims and reads
// against it. The logical suite runs them with a no-op cross; the Restart
// group runs the same scenarios with cross restarting the store, so committed
// state must survive the restart unchanged.
var replayCases = []durableCase{
	{"CommittedClaimsReplay", committedClaimsReplay},
	{"SnoozeAcrossRestart", snoozeAcrossRestart},
	{"OperationClaimReplay", operationClaimReplay},
	{"LeaseGenerationsGrow", leaseGenerationsGrow},
	{"CheckpointAndPendingBatch", checkpointAndPendingBatch},
	{"RetentionAndQuarantine", retentionAndQuarantine},
	{"InteractionResponseClaimReplay", interactionResponseClaimReplay},
}

// restartCases run only under RunDurableSuite.
var restartCases = slices.Concat(replayCases, heldExpiryCases, heldDecisionCases,
	[]durableCase{{"InFlightUnitIsLost", inFlightUnitIsLost}},
	faultRestartCases, reconcileCases,
)

func committedClaimsReplay(t *testing.T, e *env, cross func(*testing.T)) {
	ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
	key := e.identityKey("U-replay")
	ident := e.identity(t, key)
	sel := store.SelectionKey{Inbox: e.inbox, Mode: model.ModePerContact, Key: "replay"}
	conv := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
	in := e.ingressRecord(t, "evt-replay")
	receipt := model.IngressReceipt{ID: in.ID, Durable: true, Committed: true}
	evs := []model.Event{e.event(t, "conformance.one"), e.event(t, "conformance.two")}
	works := []store.Work{e.work(t), e.work(t)}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), "") })
	e.mustTx(t, func(tx store.Tx) error {
		next := e.channelRecord(ch, "r2")
		if err := tx.PutChannel(e.ctx, next, "r1"); err != nil {
			return err
		}
		if _, _, err := tx.ClaimIdentity(e.ctx, key, ident); err != nil {
			return err
		}
		if err := tx.PutContact(e.ctx, model.Contact{Ref: ident.Contact, Kind: model.ContactKindPerson, Revision: "c1"}, ""); err != nil {
			return err
		}
		if _, _, err := tx.ClaimConversation(e.ctx, store.ConversationClaim{Selection: sel, CreateKey: "first", Conversation: conv}); err != nil {
			return err
		}
		if _, _, err := tx.ClaimIngress(e.ctx, in); err != nil {
			return err
		}
		if err := tx.CompleteIngress(e.ctx, in.ID, receipt); err != nil {
			return err
		}
		if err := tx.AppendEvents(e.ctx, evs); err != nil {
			return err
		}
		for _, w := range works {
			if err := tx.EnqueueWork(e.ctx, w); err != nil {
				return err
			}
		}
		return nil
	})

	cross(t)

	e.mustView(t, func(r store.Reader) error {
		if _, err := r.GetInbox(e.ctx, e.inbox); err != nil {
			return err
		}
		got, err := r.GetChannel(e.ctx, ch)
		if err != nil {
			return err
		}
		if got.Revision != "r2" {
			t.Errorf("channel revision = %q, want r2", got.Revision)
		}
		stored, err := r.Ingress(e.ctx, in.ID)
		if err == nil && stored.Receipt != receipt {
			t.Errorf("ingress receipt = %+v, want %+v", stored.Receipt, receipt)
		}
		return err
	})
	e.mustTx(t, func(tx store.Tx) error {
		gotID, created, err := tx.ClaimIdentity(e.ctx, key, e.identity(t, key))
		if err != nil {
			return err
		}
		if created || gotID.Ref != ident.Ref || gotID.Contact != ident.Contact {
			t.Errorf("identity replay = %v created=%v, want stored %v", gotID.Ref, created, ident.Ref)
		}
		other := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
		gotConv, created, err := tx.ClaimConversation(e.ctx, store.ConversationClaim{Selection: sel, CreateKey: "first", Conversation: other})
		if err != nil {
			return err
		}
		if created || gotConv.Ref != conv.Ref {
			t.Errorf("conversation replay = %v created=%v, want stored %v", gotConv.Ref, created, conv.Ref)
		}
		gotIn, created, err := tx.ClaimIngress(e.ctx, e.ingressRecord(t, "evt-replay"))
		if err != nil {
			return err
		}
		if created || gotIn.ID != in.ID {
			t.Errorf("ingress replay = %s created=%v, want stored %s", gotIn.ID, created, in.ID)
		}
		return tx.CompleteIngress(e.ctx, in.ID, receipt)
	})
	if got := eventIDs(e.allEvents(t, model.Filter{}, 1)); !slices.Equal(got, eventIDs(evs)) {
		t.Errorf("events = %v, want %v in append order", got, eventIDs(evs))
	}
	if got := e.claimWork(t, "worker-a", 10); !slices.Equal(workIDs(got), workIDs(works)) {
		t.Errorf("work = %v, want %v in enqueue order", workIDs(got), workIDs(works))
	}
}

func operationClaimReplay(t *testing.T, e *env, cross func(*testing.T)) {
	rec := e.operation(t, "op-replay", []byte{7, 7})
	// A claim in a rolled-back unit leaves no trace: it must not block the
	// real claim or be returned by a replay.
	if err := e.txErr(func(tx store.Tx) error {
		if _, _, err := tx.ClaimOperation(e.ctx, e.operation(t, "op-replay", []byte{9})); err != nil {
			return err
		}
		return errAbort
	}); !errors.Is(err, errAbort) {
		t.Fatalf("rolled-back claim: %v", err)
	}
	e.mustTx(t, func(tx store.Tx) error {
		_, created, err := tx.ClaimOperation(e.ctx, rec)
		if err == nil && !created {
			t.Error("claim after a rolled-back claim did not create the operation")
		}
		return err
	})
	res := rec.Operation.Ref.Ref()
	var l store.Lease
	e.mustTx(t, func(tx store.Tx) error {
		var err error
		l, err = tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: res, Owner: "worker-a", Until: e.fx.Now().Add(time.Minute)})
		return err
	})
	attempt := e.id(t)
	ref := model.ExternalRef{Channel: e.channel, Namespace: "conformance", Kind: "message", ID: "ext-1"}
	next := rec.Operation
	next.State, next.Revision = model.Unknown, "o2"
	e.mustTx(t, func(tx store.Tx) error {
		if err := tx.AppendAttemptEvidence(e.ctx, store.AttemptEvidence{
			Operation: rec.Operation.Ref, Attempt: attempt, Fence: l.Fence, State: model.Unknown,
			ProviderReferences: []model.ExternalRef{ref}, ObservedAt: e.fx.Now(),
		}); err != nil {
			return err
		}
		if err := tx.UpdateOperation(e.ctx, next, "o1", l.Fence); err != nil {
			return err
		}
		return tx.ReleaseLease(e.ctx, l)
	})

	cross(t)

	e.mustTx(t, func(tx store.Tx) error {
		got, created, err := tx.ClaimOperation(e.ctx, e.operation(t, "op-replay", []byte{7, 7}))
		if err != nil {
			return err
		}
		switch {
		case created:
			t.Error("replay created a second operation")
		case got.Operation.Ref != rec.Operation.Ref:
			t.Errorf("replay = %v, want stored %v", got.Operation.Ref, rec.Operation.Ref)
		case got.Operation.State != model.Unknown || got.Operation.Revision != "o2":
			t.Errorf("replay state = %s@%s, want the current state %s@o2", got.Operation.State, got.Operation.Revision, model.Unknown)
		case got.FingerprintVersion != rec.FingerprintVersion || !bytes.Equal(got.Fingerprint, rec.Fingerprint):
			t.Errorf("replay fingerprint = %d/%x, want %d/%x", got.FingerprintVersion, got.Fingerprint, rec.FingerprintVersion, rec.Fingerprint)
		}
		return nil
	})
	wantCode(t, e.txErr(func(tx store.Tx) error {
		_, _, err := tx.ClaimOperation(e.ctx, e.operation(t, "op-replay", []byte{9}))
		return err
	}), model.Conflict)
	bumped := e.operation(t, "op-replay", []byte{7, 7})
	bumped.FingerprintVersion++
	wantCode(t, e.txErr(func(tx store.Tx) error {
		_, _, err := tx.ClaimOperation(e.ctx, bumped)
		return err
	}), model.Conflict)
	stale := next
	stale.Revision = "o3"
	wantCode(t, e.txErr(func(tx store.Tx) error { return tx.UpdateOperation(e.ctx, stale, "o1", store.Fence{}) }), model.Conflict)
	e.mustView(t, func(r store.Reader) error {
		p, err := r.Attempts(e.ctx, rec.Operation.Ref, page(10))
		if err != nil {
			return err
		}
		if len(p.Items) != 1 || p.Items[0].Attempt != attempt || len(p.Items[0].ProviderReferences) != 1 || p.Items[0].ProviderReferences[0] != ref {
			t.Errorf("attempt evidence = %+v, want the one recorded attempt with its provider reference", p.Items)
		}
		return nil
	})
}

func leaseGenerationsGrow(t *testing.T, e *env, cross func(*testing.T)) {
	if e.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	w := e.work(t)
	e.mustTx(t, func(tx store.Tx) error { return tx.EnqueueWork(e.ctx, w) })
	old := e.claimWork(t, "worker-a", 1)
	if len(old) != 1 {
		t.Fatalf("claimed %v, want the enqueued item", workIDs(old))
	}
	res := model.SubscriptionRef{Tenant: tenantA, ID: e.id(t)}.Ref()
	var released store.Lease
	e.mustTx(t, func(tx store.Tx) error {
		var err error
		if released, err = tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: res, Owner: "a", Until: e.fx.Now().Add(time.Minute)}); err != nil {
			return err
		}
		return tx.ReleaseLease(e.ctx, released)
	})

	cross(t)

	if e.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	if got := e.claimWork(t, "worker-b", 1); len(got) != 0 {
		t.Fatalf("work leased to worker-a was claimed again before expiry: %v", workIDs(got))
	}
	e.fx.Advance(2 * time.Minute)
	cur := e.claimWork(t, "worker-b", 1)
	if len(cur) != 1 || cur[0].ID != w.ID || cur[0].Fence.Generation <= old[0].Fence.Generation {
		t.Fatalf("reclaim = %+v, want %s under a generation above %d", cur, w.ID, old[0].Fence.Generation)
	}
	wantCode(t, e.txErr(func(tx store.Tx) error { return tx.CompleteWork(e.ctx, old[0]) }), model.Conflict)
	e.mustTx(t, func(tx store.Tx) error { return tx.CompleteWork(e.ctx, cur[0]) })
	e.mustTx(t, func(tx store.Tx) error {
		next, err := tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: res, Owner: "b", Until: e.fx.Now().Add(time.Minute)})
		if err == nil && next.Fence.Generation <= released.Fence.Generation {
			t.Errorf("generation %d after released %d: generations must only grow", next.Fence.Generation, released.Fence.Generation)
		}
		return err
	})
}

func checkpointAndPendingBatch(t *testing.T, e *env, cross func(*testing.T)) {
	evs := []model.Event{e.event(t, "conformance.one"), e.event(t, "conformance.two"), e.event(t, "conformance.three")}
	e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, evs) })
	var first, second model.Page[model.Event]
	e.mustView(t, func(r store.Reader) error {
		var err error
		if first, err = r.Events(e.ctx, model.Filter{}, "", 2); err != nil {
			return err
		}
		second, err = r.Events(e.ctx, model.Filter{}, first.Next, 10)
		return err
	})
	if len(first.Items) != 2 || len(second.Items) != 1 {
		t.Fatalf("pages = %d+%d events, want 2+1", len(first.Items), len(second.Items))
	}
	sub := model.SubscriptionRef{Tenant: tenantA, ID: e.id(t)}
	var l store.Lease
	digest := []byte{0xd1, 0x9e}
	e.mustTx(t, func(tx store.Tx) error {
		var err error
		if l, err = tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: sub.Ref(), Owner: "consumer", Until: e.fx.Now().Add(time.Hour)}); err != nil {
			return err
		}
		return tx.PutSubscription(e.ctx, store.SubscriptionRecord{
			Subscription: model.Subscription{Ref: sub, Revision: "s1"},
			Principal:    model.Principal{Tenant: tenantA, Actor: "conformance-actor"},
			Consumer:     "bot",
			PendingBatch: &store.BatchRecord{
				Through: first.Next, EventIDs: eventIDs(first.Items),
				TokenDigest: []byte{1}, ExpiresAt: e.fx.Now().Add(time.Hour), Fence: l.Fence,
			},
		}, "")
	})
	e.mustTx(t, func(tx store.Tx) error {
		if err := tx.AdvanceCheckpoint(e.ctx, store.CheckpointAdvance{
			Subscription: sub, Prior: "", Next: first.Next, Fence: l.Fence, Revision: "s2",
		}); err != nil {
			return err
		}
		cur, err := tx.Subscription(e.ctx, sub)
		if err != nil {
			return err
		}
		cur.Subscription.Revision = "s3"
		cur.PendingBatch = &store.BatchRecord{
			From: first.Next, Through: second.Next, EventIDs: eventIDs(second.Items),
			TokenDigest: digest, ExpiresAt: e.fx.Now().Add(time.Hour), Fence: l.Fence,
		}
		return tx.PutSubscription(e.ctx, cur, "s2")
	})

	cross(t)

	e.mustView(t, func(r store.Reader) error {
		got, err := r.Subscription(e.ctx, sub)
		if err != nil {
			return err
		}
		b := got.PendingBatch
		switch {
		case got.Subscription.Checkpoint != first.Next || got.Subscription.Revision != "s3":
			t.Errorf("subscription = checkpoint %q rev %q, want %q rev s3", got.Subscription.Checkpoint, got.Subscription.Revision, first.Next)
		case b == nil:
			t.Error("pending batch lost")
		case b.From != first.Next || b.Through != second.Next || !slices.Equal(b.EventIDs, eventIDs(second.Items)) || !bytes.Equal(b.TokenDigest, digest) || b.Fence != l.Fence:
			t.Errorf("pending batch = %+v, want the issued range, events, digest and fence", *b)
		}
		rest, err := r.Events(e.ctx, model.Filter{}, got.Subscription.Checkpoint, 10)
		if err == nil && !slices.Equal(eventIDs(rest.Items), eventIDs(second.Items)) {
			t.Errorf("events after checkpoint = %v, want %v", eventIDs(rest.Items), eventIDs(second.Items))
		}
		return err
	})
	e.mustTx(t, func(tx store.Tx) error {
		return tx.AdvanceCheckpoint(e.ctx, store.CheckpointAdvance{
			Subscription: sub, Prior: first.Next, Next: second.Next, Fence: l.Fence, Revision: "s4",
		})
	})
}

// inFlightUnitIsLost runs only in the Restart group: a unit still open when
// the process stops never commits, and a unit rolled back before the stop
// stays rolled back.
func inFlightUnitIsLost(t *testing.T, e *env, cross func(*testing.T)) {
	rolled := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
	if err := e.txErr(func(tx store.Tx) error {
		if err := tx.PutChannel(e.ctx, e.channelRecord(rolled, "r1"), ""); err != nil {
			return err
		}
		return errAbort
	}); !errors.Is(err, errAbort) {
		t.Fatalf("rolled-back unit: %v", err)
	}
	ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
	in := e.ingressRecord(t, "evt-crash")
	key := e.identityKey("U-crash")
	ident := e.identity(t, key)
	write := e.publishingWrite(ch, e.event(t, "conformance.lost"), e.work(t), in)
	u := e.holdUnit(t, func(tx store.Tx) error {
		if err := write(tx); err != nil {
			return err
		}
		_, _, err := tx.ClaimIdentity(e.ctx, key, ident)
		return err
	})

	cross(t)

	// Read-only checks first: a writer on the new handle could wait on locks
	// the abandoned unit still holds.
	e.requireAbsent(t, rolled)
	e.requireUnpublished(t, ch, in)
	err := e.s.View(e.ctx, tenantA, func(r store.Reader) error {
		_, err := r.Identity(e.ctx, key)
		return err
	})
	wantCode(t, err, model.NotFound)

	// The abandoned unit ends without committing; nothing appears later.
	_ = u.finish(false)
	e.requireUnpublished(t, ch, in)
	if got := e.claimWork(t, "worker-a", 10); len(got) != 0 {
		t.Fatalf("work from the abandoned unit is claimable: %v", workIDs(got))
	}
	if _, created := e.claimIngress(t, e.ingressRecord(t, "evt-crash")); !created {
		t.Fatal("abandoned unit's ingress claim survived the restart")
	}
	e.mustTx(t, func(tx store.Tx) error {
		_, created, err := tx.ClaimIdentity(e.ctx, key, e.identity(t, key))
		if err == nil && !created {
			t.Error("abandoned unit's identity claim survived the restart")
		}
		return err
	})
}

// interactionResponseClaimReplay commits an interaction, its initial
// response claim and a follow-up, then requires the claim to hold across
// cross: the initial response is still taken, the follow-up still counted,
// the token still stored apart, and a redaction after cross clears it.
func interactionResponseClaimReplay(t *testing.T, e *env, cross func(*testing.T)) {
	rec := e.storedInteraction(t, "replay-token")
	ref := rec.Interaction.Ref
	initial, follow := e.operationRef(t), e.operationRef(t)
	e.mustTx(t, func(tx store.Tx) error {
		if _, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: initial, Revision: "i2"}); err != nil {
			return err
		}
		_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: follow, FollowUp: true, Revision: "i3"})
		return err
	})
	cross(t)
	want := rec.Interaction.Clone()
	want.Response = model.InteractionResponse{Initial: &initial, FollowUps: 1}
	want.Revision = "i3"
	e.wantInteraction(t, want)
	e.wantToken(t, ref, store.ResponseToken{Token: "replay-token", ExpiresAt: rec.TokenExpiresAt})
	err := e.txErr(func(tx store.Tx) error {
		_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: e.operationRef(t), Revision: "i4"})
		wantCode(t, err, model.Conflict)
		return nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
	e.mustTx(t, func(tx store.Tx) error {
		_, err := tx.Redact(e.ctx, store.Redaction{Interaction: &ref, Expected: "i3", Revision: "i4"})
		return err
	})
	cross(t)
	e.wantToken(t, ref, store.ResponseToken{ExpiresAt: rec.TokenExpiresAt})
}
