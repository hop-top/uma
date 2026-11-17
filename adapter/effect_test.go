package adapter_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func interactionEffect() adapter.Effect {
	ch := model.ChannelRef{Tenant: "tenant-a", ID: "00000000-0000-7000-8000-000000000001"}
	msg := model.ExternalRef{Channel: ch, Namespace: "chat.message", Kind: "message", ID: "m-1"}
	target := msg
	return adapter.Effect{
		Operation: model.OperationRef{Tenant: "tenant-a", ID: "00000000-0000-7000-8000-000000000002"},
		Attempt:   "00000000-0000-7000-8000-000000000003",
		Route:     model.ExternalRef{Channel: ch, Namespace: "chat.room", Kind: "room", ID: "r-1"},
		Target:    &target,
		Audience:  model.Audience{Visibility: model.Public},
		Interaction: &adapter.InteractionContext{
			ProviderID: "cb-1", Kind: model.InteractionAction, Token: "secret-token",
			ExpiresAt: time.Unix(1_700_000_900, 0), Message: &msg,
		},
	}
}

// Effect.Clone shares no pointer with the original, the interaction
// context and the message it names included.
func TestEffectInteractionClone(t *testing.T) {
	e := interactionEffect()
	c := e.Clone()
	if !reflect.DeepEqual(e, c) {
		t.Fatalf("clone differs:\n got %+v\nwant %+v", c, e)
	}
	if c.Target == e.Target || c.Interaction == e.Interaction || c.Interaction.Message == e.Interaction.Message {
		t.Fatal("clone aliases the original")
	}
	c.Interaction.Token = "other"
	c.Interaction.Message.ID = "m-2"
	if e.Interaction.Token != "secret-token" || e.Interaction.Message.ID != "m-1" {
		t.Fatal("mutating the clone changed the original")
	}
	if z := (adapter.Effect{}).Clone(); z.Target != nil || z.Interaction != nil {
		t.Fatalf("Clone(zero) = %+v", z)
	}
}

// Formatting an interaction context never prints its token.
func TestInteractionContextHidesToken(t *testing.T) {
	ic := *interactionEffect().Interaction
	for _, s := range []string{fmt.Sprint(ic), fmt.Sprintf("%v", ic), fmt.Sprintf("%+v", ic), fmt.Sprintf("%#v", ic)} {
		if strings.Contains(s, "secret-token") {
			t.Fatalf("formatted context %q prints the token", s)
		}
		if !strings.Contains(s, "cb-1") {
			t.Fatalf("formatted context %q lacks the provider ID", s)
		}
	}
}

// The adapter contract major moved to 3 with the interaction input rename.
func TestContractMajor(t *testing.T) {
	if adapter.ContractMajor != 3 {
		t.Fatalf("ContractMajor = %d, want 3", adapter.ContractMajor)
	}
}

type responder struct{}

func (responder) Respond(_ context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	return adapter.Outcome{State: model.DeliveryState(r.Mode)}, nil
}

var _ adapter.InteractionResponder = responder{}
