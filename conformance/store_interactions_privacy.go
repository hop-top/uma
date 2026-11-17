package conformance

import (
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// interactionPrivacyCases cover redaction of an interaction and the
// expired-token sweep: what is cleared, what stays, and that neither
// touches records it does not own.
var interactionPrivacyCases = []storeCase{
	{"InteractionRedactionTombstonesTheRecordAndItsIngressCopy", func(t *testing.T, e *env) {
		fx := e.interactionRedactionFixture(t)
		ref := fx.rec.Interaction.Ref
		e.mustTx(t, func(tx store.Tx) error {
			res, err := tx.Redact(e.ctx, store.Redaction{Interaction: &ref, Expected: "i1", Revision: "i2"})
			if err == nil && len(res.Blobs) != 0 {
				t.Errorf("redaction unbound %+v, want no blobs", res.Blobs)
			}
			return err
		})
		want := fx.rec.Interaction.Clone()
		want.Fields.Data, want.Arguments = nil, ""
		want.Redacted, want.Revision = true, "i2"
		e.wantInteraction(t, want)
		// The token is gone; the expiry stays.
		e.wantToken(t, ref, store.ResponseToken{ExpiresAt: fx.rec.TokenExpiresAt})
		e.mustView(t, func(r store.Reader) error {
			p, err := r.Interactions(e.ctx, list(10))
			if err == nil && (len(p.Items) != 2 || !reflect.DeepEqual(p.Items[0], want)) {
				t.Errorf("Interactions = %+v, want the tombstone first", p.Items)
			}
			return err
		})

		// The raw copy loses the same data; its identity and dedup key stay.
		cleared := fx.ingress.Clone()
		cleared.Input.Envelope.Interaction.Fields.Data = nil
		cleared.Input.Envelope.Interaction.Arguments = ""
		e.wantIngress(t, cleared)
		redelivered := e.ingressRecord(t, fx.ingress.DedupKey)
		redelivered.Input = fx.ingress.Input
		if back, created := e.claimIngress(t, redelivered); created || back.ID != fx.ingress.ID {
			t.Errorf("redelivery claim = %s created %v, want the tombstoned record", back.ID, created)
		}

		// Another interaction, and the source message, are untouched.
		e.wantInteraction(t, fx.other.Interaction)
		e.wantToken(t, fx.other.Interaction.Ref, store.ResponseToken{Token: "other-token", ExpiresAt: fx.other.TokenExpiresAt})
		e.wantMessage(t, fx.msg)

		// A tombstone can be redacted again, and answers no response.
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Interaction: &ref, Expected: "i2", Revision: "i3"})
			return err
		})
		for _, follow := range []bool{false, true} {
			err := e.txErr(func(tx store.Tx) error {
				_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: e.operationRef(t), FollowUp: follow, Revision: "i4"})
				wantCode(t, err, model.Conflict, "claim on a redacted interaction, follow-up=", follow)
				return nil
			})
			if err != nil {
				t.Fatalf("Transact: %v", err)
			}
		}
	}},
	{"InteractionRedactionKeepsTheResponseState", func(t *testing.T, e *env) {
		fx := e.interactionRedactionFixture(t)
		ref := fx.rec.Interaction.Ref
		initial := e.operationRef(t)
		e.mustTx(t, func(tx store.Tx) error {
			if _, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: initial, Revision: "i2"}); err != nil {
				return err
			}
			_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: e.operationRef(t), FollowUp: true, Revision: "i3"})
			return err
		})
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Interaction: &ref, Expected: "i3", Revision: "i4"})
			return err
		})
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Interaction(e.ctx, ref)
			if err == nil && (!got.Redacted || got.Response.Initial == nil || *got.Response.Initial != initial ||
				got.Response.FollowUps != 1 || got.Revision != "i4") {
				t.Errorf("redacted interaction = %+v, want its response state kept under i4", got)
			}
			return err
		})
	}},
	{"InteractionRedactionChecksRevisionAndScope", func(t *testing.T, e *env) {
		fx := e.interactionRedactionFixture(t)
		ref := fx.rec.Interaction.Ref
		missing := model.InteractionRef{Conversation: ref.Conversation, ID: e.id(t)}
		for name, c := range map[string]struct {
			tenant model.TenantID
			req    store.Redaction
			code   model.ErrorCode
		}{
			"stale revision": {tenantA, store.Redaction{Interaction: &ref, Expected: "i0", Revision: "i2"}, model.Conflict},
			"same revision":  {tenantA, store.Redaction{Interaction: &ref, Expected: "i1", Revision: "i1"}, model.Validation},
			"no expected":    {tenantA, store.Redaction{Interaction: &ref, Revision: "i2"}, model.Validation},
			"unknown":        {tenantA, store.Redaction{Interaction: &missing, Expected: "i1", Revision: "i2"}, model.NotFound},
			"foreign tenant": {tenantB, store.Redaction{Interaction: &ref, Expected: "i1", Revision: "i2"}, model.NotFound},
			"two targets": {tenantA, store.Redaction{
				Interaction: &ref, Message: &fx.msg.Ref, Expected: "i1", Revision: "i2",
			}, model.Validation},
		} {
			err := e.s.Transact(e.ctx, c.tenant, func(tx store.Tx) error {
				_, err := tx.Redact(e.ctx, c.req)
				if !model.IsCode(err, c.code) {
					t.Errorf("%s: Redact = %v, want %s", name, err, c.code)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("%s: Transact: %v", name, err)
			}
		}
		e.wantInteraction(t, fx.rec.Interaction)
		e.wantToken(t, ref, store.ResponseToken{Token: "redact-token", ExpiresAt: fx.rec.TokenExpiresAt})
		e.wantIngress(t, fx.ingress)
	}},
	{"RedactingTheSourceMessageLeavesInteractionsAlone", func(t *testing.T, e *env) {
		fx := e.interactionRedactionFixture(t)
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Message: &fx.msg.Ref, Expected: "m1", Revision: "m2"})
			return err
		})
		e.wantInteraction(t, fx.rec.Interaction)
		e.wantToken(t, fx.rec.Interaction.Ref, store.ResponseToken{Token: "redact-token", ExpiresAt: fx.rec.TokenExpiresAt})
	}},
	{"ExpiredTokenSweepClearsTokensOnly", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		base := time.Unix(1_700_000_000, 0).UTC()
		type stored struct {
			rec     store.InteractionRecord
			expires time.Time
		}
		var recs []stored
		for _, expires := range []time.Time{
			base.Add(time.Minute), base.Add(2 * time.Minute), base.Add(10 * time.Minute), base.Add(5 * time.Minute),
		} {
			rec := e.interactionRecord(t, conv, "tok")
			rec.TokenExpiresAt, rec.Interaction.RespondUntil = expires, expires
			e.mustTx(t, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, rec) })
			recs = append(recs, stored{rec, expires})
		}
		noToken := e.interactionRecord(t, conv, "")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, noToken) })
		// A response recorded before the sweep stays recorded.
		answered := recs[0].rec.Interaction.Ref
		op := e.operationRef(t)
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: answered, Operation: op, Revision: "i2"})
			return err
		})
		heads := e.eventHead(t)

		horizon := base.Add(5 * time.Minute) // strictly before: the 5 minute token stays
		if got := e.purge(t, store.FamilyInteractionTokens, horizon, 1); got.Purged != 1 || got.Complete {
			t.Fatalf("bounded sweep = %+v, want 1 cleared and more left", got)
		}
		if got := e.purge(t, store.FamilyInteractionTokens, horizon, 10); got.Purged != 1 || !got.Complete {
			t.Fatalf("sweep = %+v, want the last expired token cleared, complete", got)
		}
		if got := e.purge(t, store.FamilyInteractionTokens, horizon, 10); got.Purged != 0 || !got.Complete {
			t.Errorf("repeated sweep = %+v, want nothing cleared", got)
		}
		for i, s := range recs {
			expired := s.expires.Before(horizon)
			want := store.ResponseToken{Token: "tok", ExpiresAt: s.expires}
			if expired {
				want.Token = ""
			}
			e.wantToken(t, s.rec.Interaction.Ref, want)
			// No record changes: not its revision, response state or fields.
			wantRec := s.rec.Interaction
			if i == 0 {
				wantRec = wantRec.Clone()
				wantRec.Response.Initial, wantRec.Revision = &op, "i2"
			}
			e.wantInteraction(t, wantRec)
		}
		e.wantToken(t, noToken.Interaction.Ref, store.ResponseToken{})
		e.wantInteraction(t, noToken.Interaction)
		if h := e.eventHead(t); h != heads {
			t.Errorf("event head = %q after the sweep, want %q: the sweep emits nothing", h, heads)
		}
	}},
}

// interactionRedactionFixture holds an interaction with a token, the
// ingress record that created it, a second interaction, and the message the
// control sits on.
type interactionRedactionFixture struct {
	rec     store.InteractionRecord
	other   store.InteractionRecord
	ingress store.IngressRecord
	msg     model.Message
}

func (e *env) interactionRedactionFixture(t *testing.T) interactionRedactionFixture {
	t.Helper()
	conv := e.putConversation(t)
	fx := interactionRedactionFixture{
		rec:   e.interactionRecord(t, conv, "redact-token"),
		other: e.interactionRecord(t, conv, "other-token"),
	}
	fx.msg = e.message(model.MessageRef{Conversation: conv, ID: e.id(t)}, "m1")
	fx.rec.Interaction.Message = &fx.msg.Ref

	fx.ingress = e.ingressRecord(t, "dedup-interaction")
	fx.ingress.Input = model.Ingress{
		Call: model.Call{Correlation: "corr-interaction"},
		Envelope: model.Envelope{
			EventID: "e-interaction", Destination: "support@example.test", Kind: model.EnvelopeInteraction,
			Sender: e.externalRef("user", "u-1"), Assurance: "verified",
			Audience:   model.Audience{Visibility: model.Public},
			OccurredAt: time.Unix(1_700_000_000, 0).UTC(), SourceRevision: "v1",
			Interaction: &model.InteractionInput{
				ID: "ix-1", Kind: model.InteractionSubmission, Actor: e.externalRef("user", "u-1"),
				Arguments: "secret arguments",
				Fields:    model.Extension{Name: "conformance.form", Version: 3, Data: []byte(`{"f":"secret"}`)},
			},
		},
		ReceiverCursor: "rc-interaction",
	}
	fx.ingress.Receipt = model.IngressReceipt{ID: fx.ingress.ID, Durable: true, Committed: true}
	fx.rec.Ingress = fx.ingress.ID
	e.mustTx(t, func(tx store.Tx) error {
		if err := tx.PutMessage(e.ctx, fx.msg, ""); err != nil {
			return err
		}
		claim := fx.ingress.Clone()
		claim.Receipt = model.IngressReceipt{}
		if _, _, err := tx.ClaimIngress(e.ctx, claim); err != nil {
			return err
		}
		if err := tx.CompleteIngress(e.ctx, fx.ingress.ID, fx.ingress.Receipt); err != nil {
			return err
		}
		if err := tx.PutInteraction(e.ctx, fx.rec); err != nil {
			return err
		}
		return tx.PutInteraction(e.ctx, fx.other)
	})
	return fx
}
