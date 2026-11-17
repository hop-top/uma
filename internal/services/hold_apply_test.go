package services_test

import (
	"encoding/json"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// heldWorld is a service fixture whose proposer is a plugin actor and whose
// decider is the rig's own principal; the policy allows both everything.
type heldWorld struct {
	*svc
	plugin enginetest.Client
	other  model.Conversation
	labels map[string]model.LabelRef
}

func (w *heldWorld) labelRef(name string) model.LabelRef { return w.labels[name] }

func (w *heldWorld) getInbox() model.Inbox {
	w.T.Helper()
	var in model.Inbox
	err := w.E.Store().View(w.Ctx, tenantA, func(r store.Reader) (err error) {
		in, err = r.GetInbox(w.Ctx, w.Conv.Ref.Inbox)
		return err
	})
	if err != nil {
		w.T.Fatal(err)
	}
	return in
}

func newHeldWorld(t *testing.T, opts ...enginetest.RigOption) *heldWorld {
	t.Helper()
	p := &rulePolicy{}
	opts = append([]enginetest.RigOption{func(c *engine.Config, _ *outFactory) { c.Authorizer = p }}, opts...)
	r := newRig(t, opts...)
	s := &svc{rig: r, pol: p, seq: portstest.NewSequenceIDs(0x5e1)}
	s.inbox2 = model.Inbox{Ref: model.InboxRef{Tenant: tenantA, ID: inboxTwo}, Channel: r.Ch.Ref, Name: "two", Revision: "in2"}
	s.put(func(tx store.Tx) error { return tx.PutInbox(r.Ctx, s.inbox2, "") })
	plugin, err := enginetest.For(r.E, model.Principal{Tenant: tenantA, Actor: "plugin:triage"})
	if err != nil {
		t.Fatal(err)
	}
	return &heldWorld{svc: s, plugin: plugin, other: s.conversation(s.inbox2.Ref, model.ConversationOpen), labels: map[string]model.LabelRef{}}
}

func heldCmd(op model.OperationID, rev model.Revision) model.Command {
	c := cmd(op, rev)
	c.Hold = &model.HoldProposal{Reason: "review", TrustKey: "triage/" + string(op)}
	return c
}

func (w *heldWorld) intent(op model.Operation) model.Extension {
	w.T.Helper()
	var rec store.OperationRecord
	err := w.E.Store().View(w.Ctx, tenantA, func(r store.Reader) (err error) {
		rec, err = r.Operation(w.Ctx, op.Ref)
		return err
	})
	if err != nil {
		w.T.Fatal(err)
	}
	return rec.Command
}

func (w *heldWorld) decide(op model.Operation) model.DecideHeld {
	return model.DecideHeld{Call: call(), Operation: op.Ref, ExpectedRevision: op.Revision}
}

func (w *heldWorld) edit(op model.Operation, args model.Extension) (model.Decided, error) {
	return w.C.Operations().Edit(w.Ctx, model.EditHeld{DecideHeld: w.decide(op), Args: args})
}

func ext(t *testing.T, name string, v any) model.Extension {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return model.Extension{Name: name, Version: 1, Data: data}
}

// heldIntent describes one holdable intent: how to propose it, what an
// edit within the target and an edit away from it look like, and how to
// observe that it was applied.
type heldIntent struct {
	name    string
	opts    []enginetest.RigOption
	setup   func(w *heldWorld)
	propose func(w *heldWorld, op model.OperationID) (model.Acceptance, error)
	// within and away edit the stored intent.
	within func(w *heldWorld, stored model.Extension) model.Extension
	away   func(w *heldWorld, stored model.Extension) model.Extension
	// applied reports how many times the write landed and its text, label
	// or kind; fresh is the same before anything is approved.
	applied func(w *heldWorld, op model.Operation) []string
	// local: approval commits with no work. editWant is what applied
	// reports after the edit within (default "edited").
	local    bool
	editWant string
}

func (w *heldWorld) messageText(ref *model.MessageRef) []string {
	if ref == nil {
		return nil
	}
	m, err := w.C.Messages().Get(w.Ctx, model.Lookup[model.MessageRef]{Call: call(), Ref: *ref})
	if model.IsCode(err, model.NotFound) {
		return nil
	}
	if err != nil {
		w.T.Fatal(err)
	}
	if m.Origin == nil || m.Origin.Actor != "plugin:triage" {
		w.T.Fatalf("message origin = %+v, want the proposing plugin", m.Origin)
	}
	return []string{m.Content.Parts[0].Text}
}

func edited(t *testing.T, stored model.Extension, f func(m map[string]any)) model.Extension {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(stored.Data, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	return ext(t, stored.Name, m)
}

func textParts(s string) []any {
	return []any{map[string]any{"kind": string(model.PartText), "text": s}}
}

func retarget(w *heldWorld) any {
	data, _ := json.Marshal(w.other.Ref)
	var v any
	_ = json.Unmarshal(data, &v)
	return v
}

func heldIntents() []heldIntent {
	return append(localWriteIntents(), []heldIntent{
		{
			name: "reply",
			propose: func(w *heldWorld, op model.OperationID) (model.Acceptance, error) {
				return w.plugin.Messages().Reply(w.Ctx, model.Reply{Command: heldCmd(op, ""), Conversation: w.Conv.Ref, Content: text("hi")})
			},
			within: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["content"].(map[string]any)["parts"] = textParts("edited") })
			},
			away: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["conversation"] = retarget(w) })
			},
			applied: func(w *heldWorld, op model.Operation) []string { return w.messageText(op.ResultMessage) },
		},
		{
			name: "delivered note",
			opts: []enginetest.RigOption{enginetest.WithNotes},
			setup: func(w *heldWorld) {
				in := w.getInbox()
				in.DeliverNotes = true
				prev := w.getInbox().Revision
				in.Revision = "in-notes"
				w.put(func(tx store.Tx) error { return tx.PutInbox(w.Ctx, in, prev) })
			},
			propose: func(w *heldWorld, op model.OperationID) (model.Acceptance, error) {
				return w.plugin.Messages().AddInternalNote(w.Ctx, model.InternalNote{Command: heldCmd(op, ""), Conversation: w.Conv.Ref, Content: text("note")})
			},
			within: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["content"].(map[string]any)["parts"] = textParts("edited") })
			},
			away: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["conversation"] = retarget(w) })
			},
			applied: func(w *heldWorld, op model.Operation) []string { return w.messageText(op.ResultMessage) },
		},
		{
			name: "local note",
			propose: func(w *heldWorld, op model.OperationID) (model.Acceptance, error) {
				return w.plugin.Messages().AddInternalNote(w.Ctx, model.InternalNote{Command: heldCmd(op, ""), Conversation: w.Conv.Ref, Content: text("note")})
			},
			within: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["content"].(map[string]any)["parts"] = textParts("edited") })
			},
			away: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["conversation"] = retarget(w) })
			},
			applied: func(w *heldWorld, op model.Operation) []string { return w.messageText(op.ResultMessage) },
		},
		{
			name: "conversation labels",
			setup: func(w *heldWorld) {
				w.labels["Urgent"] = w.configure("lab-1", "Urgent").Ref
				w.labels["Billing"] = w.configure("lab-2", "Billing").Ref
			},
			propose: func(w *heldWorld, op model.OperationID) (model.Acceptance, error) {
				return w.plugin.Conversations().AddLabels(w.Ctx, model.ConversationLabels{
					Command: heldCmd(op, ""), Conversation: w.Conv.Ref, Labels: []model.LabelRef{w.labelRef("Urgent")},
				})
			},
			within: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) {
					v, _ := json.Marshal([]model.LabelRef{w.labelRef("Billing")})
					var l any
					_ = json.Unmarshal(v, &l)
					m["labels"] = l
				})
			},
			away: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["conversation"] = retarget(w) })
			},
			applied: func(w *heldWorld, _ model.Operation) []string {
				var out []string
				for _, l := range w.getConv(w.Conv.Ref).Labels {
					out = append(out, w.getLabel(l).Name)
				}
				return out
			},
		},
		{
			name: "flag",
			opts: []enginetest.RigOption{enginetest.WithState(model.ScopeAccount)},
			propose: func(w *heldWorld, op model.OperationID) (model.Acceptance, error) {
				return w.plugin.Conversations().Flag(w.Ctx, model.FlagConversation{
					Command: heldCmd(op, ""), Conversation: w.Conv.Ref, Kind: model.FlagStar,
				})
			},
			within: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["kind"] = model.FlagImportant })
			},
			away: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["conversation"] = retarget(w) })
			},
			// A flag writes nothing locally: its approval queues the provider
			// call, which the rig delivers.
			applied: func(w *heldWorld, _ model.Operation) []string {
				w.Step("w")
				var out []string
				for _, c := range w.F.MutationCalls() {
					if c.State != nil {
						out = append(out, c.State.Kind)
					}
				}
				return out
			},
		},
	}...)
}

func TestHeldIntentsHoldApproveEditRetarget(t *testing.T) {
	for _, in := range heldIntents() {
		t.Run(in.name, func(t *testing.T) {
			w := newHeldWorld(t, in.opts...)
			if in.setup != nil {
				in.setup(w)
			}
			// Hold: stored as held, nothing applied.
			a, err := in.propose(w, "op-1")
			if err != nil || a.Operation.State != model.Held || a.Operation.Held == nil {
				t.Fatalf("hold = %+v, %v", a.Operation, err)
			}
			held := a.Operation
			if got := in.applied(w, held); len(got) != 0 {
				t.Fatalf("applied before approval: %v", got)
			}
			// Retarget refused, still held and unapplied.
			stored := w.intent(held)
			if _, err := w.edit(held, in.away(w, stored)); !model.IsCode(err, model.Validation) {
				t.Fatalf("retarget edit err = %v, want validation", err)
			}
			if got := w.Op(held.Ref); got.State != model.Held || got.Held.Decision != "" {
				t.Fatalf("after refused retarget = %s %+v", got.State, got.Held)
			}
			// Approve applies once; a second approval is a replay.
			d, err := w.C.Operations().Approve(w.Ctx, w.decide(held))
			if err != nil || d.Operation.State == model.Held || d.Replayed {
				t.Fatalf("Approve = %+v, %v", d.Operation, err)
			}
			want := model.Queued
			if in.local || in.name == "conversation labels" || in.name == "local note" {
				want = model.Committed // a local write commits on approval, with no work
			}
			if d.Operation.State != want {
				t.Fatalf("approved state = %s, want %s", d.Operation.State, want)
			}
			first := in.applied(w, d.Operation)
			if len(first) != 1 {
				t.Fatalf("applied after approval = %v, want once", first)
			}
			again, err := w.C.Operations().Approve(w.Ctx, w.decide(d.Operation))
			if err != nil || !again.Replayed {
				t.Fatalf("second Approve = %+v, %v; want a replay", again.Operation, err)
			}
			if got := in.applied(w, d.Operation); len(got) != 1 {
				t.Fatalf("applied after replay = %v, want once", got)
			}
		})
		t.Run(in.name+" edit", func(t *testing.T) {
			w := newHeldWorld(t, in.opts...)
			if in.setup != nil {
				in.setup(w)
			}
			a, err := in.propose(w, "op-2")
			if err != nil {
				t.Fatal(err)
			}
			d, err := w.edit(a.Operation, in.within(w, w.intent(a.Operation)))
			if err != nil || d.Decision != model.HeldEdited {
				t.Fatalf("Edit = %+v, %v", d, err)
			}
			got := in.applied(w, d.Operation)
			if in.editWant != "" {
				if len(got) != 1 || got[0] != in.editWant {
					t.Fatalf("applied edit = %v, want %q", got, in.editWant)
				}
				return
			}
			if len(got) != 1 || (in.name != "conversation labels" && in.name != "flag" && got[0] != "edited") {
				t.Fatalf("applied edit = %v", got)
			}
			if in.name == "conversation labels" && got[0] != "Billing" || in.name == "flag" && got[0] != model.FlagImportant {
				t.Fatalf("applied edit = %v, want the edited arguments", got)
			}
		})
	}
}

var _ = operations.IntentReply
