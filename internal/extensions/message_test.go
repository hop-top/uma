package extensions_test

import (
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

const (
	msgFirst  model.ID = "00000005-0000-7000-8000-000000000001"
	msgSecond model.ID = "00000005-0000-7000-8000-000000000002"
	msgBare   model.ID = "00000005-0000-7000-8000-000000000003"
)

// seedMessages stores two messages of conv with provider references on the
// channel and one without.
func (x *env) seedMessages() {
	x.t.Helper()
	put := func(id model.ID, ext ...model.ExternalRef) {
		m := model.Message{
			Ref:      model.MessageRef{Conversation: x.conv.Ref, ID: id},
			Audience: model.Audience{Visibility: model.Public},
			Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "m"}}},
			External: ext, Source: "src", OccurredAt: x.clock.Now().UTC(), RecordedAt: x.clock.Now().UTC(), Revision: "m1",
		}
		err := x.store.Transact(x.ctx, enginetest.TenantA, func(tx store.Tx) error { return tx.PutMessage(x.ctx, m, "") })
		if err != nil {
			x.t.Fatal(err)
		}
	}
	put(msgFirst, model.ProviderMessageRef(x.ch.Ref, "gm1"))
	put(msgSecond, model.ProviderMessageRef(x.ch.Ref, "gm2"))
	put(msgBare)
}

func (x *env) msg(id model.ID) *model.MessageRef {
	return &model.MessageRef{Conversation: x.conv.Ref, ID: id}
}

// An effect that names a message of the conversation reaches the adapter
// with that message's provider reference as its target; without one the
// target stays empty and the route is the conversation's.
func TestExecuteMessageTargetsTheProviderMessage(t *testing.T) {
	x := newEnv(t)
	x.seedMessages()
	req := execReq("op-msg", x.conv.Ref.Ref(), `{}`)
	req.Message = x.msg(msgSecond)
	if _, err := x.c.Extensions().Execute(x.ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := x.c.Extensions().Execute(x.ctx, execReq("op-conv", x.conv.Ref.Ref(), `{}`)); err != nil {
		t.Fatal(err)
	}
	if n := x.step(x.rt); n != 2 {
		t.Fatalf("executed %d items, want 2", n)
	}
	var withMsg, without int
	for _, r := range x.f.executed() {
		if r.Route != x.conv.ReplyRoute {
			t.Fatalf("route = %+v, want the conversation's", r.Route)
		}
		switch {
		case r.Target == nil:
			without++
		case *r.Target == model.ProviderMessageRef(x.ch.Ref, "gm2"):
			withMsg++
		default:
			t.Fatalf("target = %+v, want gm2", *r.Target)
		}
	}
	if withMsg != 1 || without != 1 {
		t.Fatalf("targeted %d, untargeted %d; want one each", withMsg, without)
	}
}

// A message that is not a live message of the conversation with a provider
// reference there is a validation error and nothing is stored.
func TestExecuteMessageRefusals(t *testing.T) {
	x := newEnv(t)
	x.seedMessages()
	for name, m := range map[string]*model.MessageRef{
		"unknown":      x.msg("00000005-0000-7000-8000-0000000000ff"),
		"no provider":  x.msg(msgBare),
		"other conv":   {Conversation: x.plainC.Ref, ID: msgFirst},
		"not a member": {Conversation: x.conv.Ref, ID: "other"},
	} {
		t.Run(name, func(t *testing.T) {
			req := execReq(model.OperationID("op-"+name), x.conv.Ref.Ref(), `{}`)
			req.Message = m
			_, err := x.c.Extensions().Execute(x.ctx, req)
			if err == nil {
				t.Fatal("Execute accepted a message that is not the conversation's")
			}
		})
	}
	if n := x.step(x.rt); n != 0 {
		t.Fatalf("executed %d items, want nothing stored", n)
	}
	// Not a conversation target.
	req := execReq("op-chan", x.ch.Ref.Ref(), `{}`)
	req.Message = x.msg(msgFirst)
	if _, err := x.c.Extensions().Execute(x.ctx, req); !model.IsCode(err, model.Validation) {
		t.Fatalf("Execute on a channel with a message = %v, want validation", err)
	}
}

// The same operation ID with another message conflicts.
func TestExecuteMessageIsPartOfTheFingerprint(t *testing.T) {
	x := newEnv(t)
	x.seedMessages()
	req := execReq("op-msg", x.conv.Ref.Ref(), `{}`)
	req.Message = x.msg(msgFirst)
	if _, err := x.c.Extensions().Execute(x.ctx, req); err != nil {
		t.Fatal(err)
	}
	req.Message = x.msg(msgSecond)
	if _, err := x.c.Extensions().Execute(x.ctx, req); !model.IsCode(err, model.Conflict) {
		t.Fatalf("Execute with another message = %v, want conflict", err)
	}
}
