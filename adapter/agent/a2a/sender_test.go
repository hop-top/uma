package a2a_test

import (
	"context"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/agent/a2a"
	"hop.top/uma/model"
)

func openInstance(t *testing.T) adapter.Instance {
	t.Helper()
	f, _ := a2a.New(a2a.Options{Authenticate: headerAuth})
	inst, err := f.Open(t.Context(), model.Channel{Ref: chanA, Type: a2a.Type, Configuration: a2a.Configuration(testConfig())}, adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func sendRequest(route model.ExternalRef, vis model.Visibility) adapter.SendRequest {
	return adapter.SendRequest{
		Effect: adapter.Effect{
			Operation: model.OperationRef{Tenant: tenant, ID: "00000a2a-0000-7000-8000-000000000001"},
			Attempt:   "00000a2a-0000-7000-8000-000000000002", Route: route, Audience: model.Audience{Visibility: vis},
		},
		Content: text("hi"),
	}
}

func contextRoute(id string) model.ExternalRef {
	return model.ExternalRef{Channel: chanA, Namespace: a2a.NamespaceContext, Kind: model.RefKindContext, ID: id}
}

// Send has no transport: it reports the reply accepted under a message
// reference derived from the attempt, the same for a repeated attempt.
func TestSenderRecordsWithoutTransport(t *testing.T) {
	s := openInstance(t).(adapter.Sender)
	req := sendRequest(contextRoute("ctx-1"), model.Public)
	out, err := s.Send(t.Context(), req)
	if err != nil || out.State != model.ProviderAccepted || out.Failure != nil {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	want := model.ProviderMessageRef(chanA, string(req.Attempt))
	if len(out.References) != 1 || out.References[0] != want {
		t.Fatalf("references = %v, want %v", out.References, want)
	}
	again, _ := s.Send(t.Context(), req)
	if len(again.References) != 1 || again.References[0] != want {
		t.Fatalf("repeated attempt = %v, want the same reference", again.References)
	}
}

func TestSenderRefusesWhatItCannotShow(t *testing.T) {
	s := openInstance(t).(adapter.Sender)
	other := contextRoute("ctx-1")
	other.Channel = model.ChannelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000b2"}
	room := contextRoute("ctx-1")
	room.Kind = model.RefKindRoom
	foreign := contextRoute("ctx-1")
	foreign.Namespace = "smtp.address"
	for name, req := range map[string]adapter.SendRequest{
		"internal audience":   sendRequest(contextRoute("ctx-1"), model.Internal),
		"restricted audience": sendRequest(contextRoute("ctx-1"), model.Restricted),
		"another channel":     sendRequest(other, model.Public),
		"not a context":       sendRequest(room, model.Public),
		"foreign namespace":   sendRequest(foreign, model.Public),
	} {
		if _, err := s.Send(t.Context(), req); !model.IsCode(err, model.Validation) {
			t.Errorf("%s: Send = %v, want validation", name, err)
		}
	}
	inst := openInstance(t)
	_ = inst.Close(context.Background())
	if _, err := inst.(adapter.Sender).Send(t.Context(), sendRequest(contextRoute("c"), model.Public)); !model.IsCode(err, model.Unavailable) {
		t.Errorf("Send on a closed instance = %v, want unavailable", err)
	}
}
