package fakeprovider_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

const (
	respondToken = "tok-respond-secret-0001"
	actorSource  = "u1"
)

// seedInteraction stores an action interaction of the host's conversation
// by a stored identity, with a response token valid for an hour, and
// returns it.
func (h *host) seedInteraction(id model.ID) model.Interaction {
	h.t.Helper()
	actorID := model.ID("00000000-0000-7000-8000-0000000000d1")
	ident := model.Identity{
		Ref:     model.IdentityRef{Inbox: inboxRef, ID: actorID},
		Contact: model.ContactRef{Tenant: tenant, ID: actorID}, SourceID: actorSource,
		Namespace: fakeprovider.Namespace, Kind: model.ContactKindPerson, Revision: "id1",
	}
	now := h.clock.Now().UTC()
	msg := model.ProviderMessageRef(chanA, "pm-1")
	rec := store.InteractionRecord{
		Interaction: model.Interaction{
			Ref:  model.InteractionRef{Conversation: convRef, ID: id},
			Kind: model.InteractionAction, Action: "approve",
			Actor: model.Author{
				Identity: ident.Ref, Kind: ident.Kind,
				External: model.ExternalRef{Channel: chanA, Namespace: ident.Namespace, Kind: string(model.KindIdentity), ID: actorSource},
			},
			MessageExternal: &msg,
			External:        []model.ExternalRef{model.ProviderInteractionRef(chanA, "pi-"+string(id))},
			Audience:        model.Audience{Visibility: model.Public}, OccurredAt: now, RecordedAt: now,
			Deadline: now.Add(3 * time.Second), RespondUntil: now.Add(time.Hour), Revision: "ix1", Source: fakeprovider.TypeInteractive,
		},
		Ingress: "00000009-0000-7000-8000-000000000001", ResponseToken: respondToken, TokenExpiresAt: now.Add(time.Hour),
	}
	err := h.store.Transact(h.ctx, tenant, func(tx store.Tx) error {
		key := model.IdentityKey{Inbox: inboxRef, Namespace: ident.Namespace, SourceID: ident.SourceID}
		if _, _, err := tx.ClaimIdentity(h.ctx, key, ident); err != nil {
			return err
		}
		return tx.PutInteraction(h.ctx, rec)
	})
	if err != nil {
		h.t.Fatalf("seed interaction: %v", err)
	}
	return rec.Interaction
}

func (h *host) respond(in model.Interaction, op model.OperationID, mode string, followUp bool) model.OperationRef {
	h.t.Helper()
	audience := model.Audience{Visibility: model.Public}
	if mode == model.ResponseModeEphemeral {
		audience.Visibility = model.Restricted
	}
	a, err := h.client.Interactions().Respond(h.ctx, model.Respond{
		Command:     model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + string(op))}, Operation: op},
		Interaction: in.Ref, Mode: mode, Audience: audience, FollowUp: followUp,
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "answer"}}},
		ExpiresAt: h.clock.Now().Add(30 * time.Minute),
	})
	if err != nil {
		h.t.Fatalf("Respond(%s): %v", op, err)
	}
	return a.Operation.Ref
}

func responses(calls []fakeprovider.Call) []fakeprovider.Call {
	var out []fakeprovider.Call
	for _, c := range calls {
		if c.Method == fakeprovider.MethodRespond {
			out = append(out, c)
		}
	}
	return out
}

// The interactive variant answers an interaction in the mode and audience
// asked for and records the context it received, the response token
// included, while no dump of the recorded call prints it.
func TestInteractiveRespondRecordsContext(t *testing.T) {
	h := newHost(t, fakeprovider.NewInteractive())
	in := h.seedInteraction("00000005-0000-7000-8000-000000000001")
	initial := h.respond(in, "op-initial", model.ResponseModeMessage, false)
	follow := h.respond(in, "op-follow", model.ResponseModeEphemeral, true)
	h.start("worker")
	defer h.stop()
	for _, ref := range []model.OperationRef{initial, follow} {
		if op := h.op(ref); op.State != model.ProviderAccepted {
			t.Fatalf("response settled %+v", op)
		}
	}
	calls := responses(h.factory.Script(chanA).Calls())
	if len(calls) != 2 {
		t.Fatalf("response calls = %+v, want the initial and the follow-up", calls)
	}
	for _, c := range calls {
		ic := c.Effect.Interaction
		if ic == nil || ic.Ref != in.Ref || ic.Token != respondToken || ic.Message == nil || *ic.Message != *in.MessageExternal ||
			ic.ProviderID != "pi-"+string(in.Ref.ID) || !ic.ExpiresAt.Equal(in.RespondUntil) || !c.Applied {
			t.Fatalf("call %+v: interaction context %+v, want the interaction as the engine loaded it, with its token", c.Method, ic)
		}
		for _, dump := range []string{fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), fmt.Sprint(c)} {
			if strings.Contains(dump, respondToken) {
				t.Fatalf("a dump of the recorded call prints the response token: %s", dump)
			}
		}
	}
	byMode := map[string]fakeprovider.Call{}
	for _, c := range calls {
		byMode[c.Mode] = c
	}
	msg, eph := byMode[model.ResponseModeMessage], byMode[model.ResponseModeEphemeral]
	if msg.FollowUp || msg.Effect.Audience.Visibility != model.Public || !eph.FollowUp || eph.Effect.Audience.Visibility != model.Restricted {
		t.Fatalf("message call %+v and ephemeral call %+v, want an initial public message and a restricted follow-up", msg, eph)
	}
}

// A response in update mode replaces the control's message and the scripted
// result decides its outcome like any call.
func TestInteractiveRespondUpdateAndScriptedResults(t *testing.T) {
	h := newHost(t, fakeprovider.NewInteractive())
	script := h.factory.Script(chanA)
	update := h.seedInteraction("00000005-0000-7000-8000-000000000001")
	ref := h.respond(update, "op-update", model.ResponseModeUpdate, false)
	script.Respond(fakeprovider.Rejected)
	h.start("worker")
	defer h.stop()
	op := h.op(ref)
	if op.State != model.Rejected {
		t.Fatalf("scripted rejection settled %+v", op)
	}
	calls := responses(script.Calls())
	if len(calls) != 1 || calls[0].Mode != model.ResponseModeUpdate || calls[0].Effect.Target == nil || *calls[0].Effect.Target != *update.MessageExternal {
		t.Fatalf("response calls = %+v, want one update of the control's message", calls)
	}
}
