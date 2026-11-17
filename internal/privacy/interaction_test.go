package privacy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

const responseToken = "tok-secret-value"

// denyActions allows every decision but the actions it holds.
type denyActions map[string]bool

func (d denyActions) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	return policy.Decision{Allow: !d[a.Action]}, nil
}

func interactionHarness(t *testing.T, opts ...enginetest.HarnessOption) *enginetest.Harness {
	t.Helper()
	return enginetest.NewHarness(t, enginetest.ChatType, append([]enginetest.HarnessOption{
		func(c *engine.Config) { c.Blobs = &portstest.MemoryBlobs{} },
	}, opts...)...)
}

// accept delivers interaction id by sender from, holding secret submitted
// values, arguments and a response token, and returns its record and the
// ingress record that created it.
func accept(t *testing.T, h *enginetest.Harness, in model.InboxRef, id, from string, refs ...model.ExternalRef) (model.Interaction, model.ID) {
	t.Helper()
	env := h.Interaction(id, from, enginetest.Dest, refs...)
	env.Interaction.Arguments = secret
	env.Interaction.Fields.Data = json.RawMessage(`{"note":"` + secret + `"}`)
	env.Interaction.ResponseToken = responseToken
	r := h.MustAccept(env)
	return interactionOf(t, h, in, id), r.ID
}

func interactionOf(t *testing.T, h *enginetest.Harness, in model.InboxRef, id string) model.Interaction {
	t.Helper()
	want := model.ProviderInteractionRef(h.Channel.Ref, id)
	for _, x := range enginetest.All(h, store.Reader.Interactions, model.Filter{Inboxes: []model.InboxRef{in}}) {
		if slices.Contains(x.External, want) {
			return x
		}
	}
	t.Fatalf("no interaction %q in the inbox", id)
	return model.Interaction{}
}

func tokenOf(h *enginetest.Harness, ref model.InteractionRef) string {
	h.T.Helper()
	var tok store.ResponseToken
	h.View(func(r store.Reader) (err error) { tok, err = r.InteractionToken(h.Ctx, ref); return err })
	return tok.Token
}

func redactInteraction(x model.Interaction, op model.OperationID) model.Redact {
	ref := x.Ref
	return model.Redact{Command: cmd(op, x.Revision), Target: ref.Ref(), Interaction: &ref, Reason: "erasure"}
}

func updates(evs []model.Event) []model.Event {
	return slices.DeleteFunc(slices.Clone(evs), func(e model.Event) bool { return e.Kind != model.EventInteractionUpdated })
}

// Redacting an interaction clears its submitted values, arguments and
// response token, keeps the rest, clears the same from the raw ingress
// copy, reports interaction.updated, and replays by operation ID.
func TestRedactInteraction(t *testing.T) {
	h := interactionHarness(t)
	in := h.Configure(enginetest.PerContact(true), enginetest.Dest)
	x, raw := accept(t, h, in.Ref, "i1", "alice")
	if tokenOf(h, x.Ref) != responseToken || x.Arguments != secret {
		t.Fatal("seeded interaction lacks its secrets")
	}
	before := len(h.Events())

	req := redactInteraction(x, "op-redact")
	a, err := h.Client.Privacy().Redact(h.Ctx, req)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if a.Operation.State != model.Committed || a.Operation.Result == nil || *a.Operation.Result != x.Ref.Ref() {
		t.Fatalf("acceptance %+v, want a committed operation on the interaction", a)
	}
	if k := a.Operation.Key; k == nil || k.Kind != model.ActionPrivacyRedact || k.Target != x.Ref.Ref() || k.ID != "op-redact" {
		t.Fatalf("acceptance key = %+v, want privacy.redact on the interaction", k)
	}
	got := interactionOf(t, h, in.Ref, "i1")
	switch {
	case !got.Redacted || got.Revision == x.Revision || got.Arguments != "" || len(got.Fields.Data) != 0:
		t.Fatalf("interaction %+v, want a tombstone under a new revision", got)
	case got.Fields.Name != x.Fields.Name || got.Kind != x.Kind || got.Action != x.Action || got.Actor != x.Actor ||
		got.Ref != x.Ref || got.Audience != x.Audience || !got.Deadline.Equal(x.Deadline):
		t.Fatalf("interaction %+v, want references, actor, kind and identifiers kept (was %+v)", got, x)
	}
	if tok := tokenOf(h, x.Ref); tok != "" {
		t.Fatalf("token %q survived redaction", tok)
	}
	h.View(func(r store.Reader) error {
		rec, err := r.Ingress(h.Ctx, raw)
		if err != nil {
			return err
		}
		i := rec.Input.Envelope.Interaction
		if i == nil || i.Arguments != "" || len(i.Fields.Data) != 0 || i.ID != "i1" || i.Action != "approve" || rec.DedupKey == "" {
			t.Errorf("raw ingress copy %+v, want values cleared and routing kept", i)
		}
		return nil
	})

	evs := h.Events()[before:]
	if len(evs) != 1 || evs[0].Kind != model.EventInteractionUpdated {
		t.Fatalf("events %v, want one interaction.updated", enginetest.EventKinds(evs))
	}
	var p model.InteractionUpdatedPayload
	if err := json.Unmarshal(evs[0].Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Interaction != x.Ref || p.Change != model.InteractionChangeRedacted || p.Trigger != model.TriggerCommand ||
		p.PreviousRevision != x.Revision || p.Operation == nil || *p.Operation != a.Operation.Ref {
		t.Fatalf("payload %+v", p)
	}
	if evs[0].Resource != x.Ref.Ref() || evs[0].Audience != x.Audience || evs[0].Revision != got.Revision || evs[0].Actor != enginetest.Actor {
		t.Fatalf("event %+v, want about the interaction under its audience, by the caller", evs[0])
	}
	requireNoSecret(t, h.Events())

	again, err := h.Client.Privacy().Redact(h.Ctx, req)
	if err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref {
		t.Fatalf("retry = %+v, %v; want the stored operation replayed", again, err)
	}
	if n := len(h.Events()) - before; n != 1 {
		t.Fatalf("events after retry %d, want 1", n)
	}
	// A redelivery stays a duplicate and brings nothing back.
	env := h.Interaction("i1", "alice", enginetest.Dest)
	env.Interaction.Arguments = secret
	if r := h.MustAccept(env); !r.Duplicate {
		t.Fatalf("redelivery %+v, want a duplicate", r)
	}
	if got := interactionOf(t, h, in.Ref, "i1"); got.Arguments != "" || !got.Redacted {
		t.Fatalf("interaction after a redelivery %+v", got)
	}
}

// A redaction is authorized first, and a stale revision, an unknown record
// or another tenant's change nothing.
func TestRedactInteractionRefusals(t *testing.T) {
	deny := denyActions{}
	h := interactionHarness(t, func(c *engine.Config) { c.Authorizer = deny })
	in := h.Configure(enginetest.PerContact(true), enginetest.Dest)
	x, _ := accept(t, h, in.Ref, "i1", "alice")
	before := len(h.Events())

	deny[model.ActionPrivacyRedact] = true
	_, err := h.Client.Privacy().Redact(h.Ctx, redactInteraction(x, "op-denied"))
	enginetest.RequireCode(t, err, model.Unauthorized, "")
	deny[model.ActionPrivacyRedact] = false

	stale := redactInteraction(x, "op-stale")
	stale.ExpectedRevision = "stale"
	_, err = h.Client.Privacy().Redact(h.Ctx, stale)
	enginetest.RequireCode(t, err, model.Conflict, "")

	missing := x
	missing.Ref.ID = h.ID()
	_, err = h.Client.Privacy().Redact(h.Ctx, redactInteraction(missing, "op-missing"))
	enginetest.RequireCode(t, err, model.NotFound, "")

	foreign, err := enginetest.For(h.E, model.Principal{Tenant: enginetest.TenantB, Actor: enginetest.Actor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = foreign.Privacy().Redact(h.Ctx, redactInteraction(x, "op-foreign"))
	enginetest.RequireCode(t, err, model.NotFound, "")

	propagate := redactInteraction(x, "op-propagate")
	propagate.PropagateToProvider = true
	_, err = h.Client.Privacy().Redact(h.Ctx, propagate)
	enginetest.RequireCode(t, err, model.Unsupported, "")

	if got := interactionOf(t, h, in.Ref, "i1"); got.Redacted || got.Revision != x.Revision || tokenOf(h, x.Ref) != responseToken {
		t.Fatalf("interaction after refused redactions = %+v, want it untouched", got)
	}
	if len(h.Events()) != before {
		t.Fatal("refused redactions emitted events")
	}
}

// The response token appears nowhere a caller or consumer can read: not in
// the interaction as listed, the events, an export, the raw ingress copy or
// an error.
func TestInteractionTokenNeverLeaves(t *testing.T) {
	h := interactionHarness(t)
	in := h.Configure(enginetest.PerContact(true), enginetest.Dest)
	x, raw := accept(t, h, in.Ref, "i1", "alice")

	var leaks []string
	check := func(where string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(responseToken)) {
			leaks = append(leaks, where)
		}
	}
	h.View(func(r store.Reader) error {
		got, err := r.Interaction(h.Ctx, x.Ref)
		if err != nil {
			return err
		}
		check("Interaction", got)
		p, err := r.Interactions(h.Ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in.Ref}}, Page: model.PageRequest{Limit: 10}})
		if err != nil {
			return err
		}
		check("Interactions", p)
		rec, err := r.Ingress(h.Ctx, raw)
		check("Ingress", rec)
		return err
	})
	check("events", h.Events())

	_, _, lines := export(t, h.Client, h.Ctx, exportReq("op-export", model.Filter{Inboxes: []model.InboxRef{in.Ref}}))
	check("export", lines)

	redacted := redactInteraction(x, "op-redact")
	_, err := h.Client.Privacy().Redact(h.Ctx, redacted)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Client.Privacy().Redact(h.Ctx, redacted)
	if err != nil {
		t.Fatal(err)
	}
	stale := redactInteraction(x, "op-stale")
	_, err = h.Client.Privacy().Redact(h.Ctx, stale)
	if err == nil || strings.Contains(err.Error(), responseToken) {
		t.Fatalf("stale redaction error %v, want a refusal without the token", err)
	}
	if len(leaks) > 0 {
		t.Fatalf("response token leaked through %v", leaks)
	}
}

// An export holds the interactions it selects that the caller may read,
// each as one interaction line without the token; an opener is refused with
// a conflict once one was redacted since.
func TestExportInteractions(t *testing.T) {
	deny := denyActions{}
	h := interactionHarness(t, func(c *engine.Config) { c.Authorizer = deny })
	in := h.Configure(enginetest.PerContact(true), enginetest.Dest)
	h.MustAccept(h.Inbound("e1", "m1", "alice", enginetest.Dest))
	x, _ := accept(t, h, in.Ref, "i1", "alice")
	y, _ := accept(t, h, in.Ref, "i2", "bob")

	conv := x.Ref.Conversation
	for name, f := range map[string]model.Filter{
		"conversation": {Conversation: &conv},
		"inbox":        {Inboxes: []model.InboxRef{in.Ref}},
	} {
		_, _, lines := export(t, h.Client, h.Ctx, exportReq(model.OperationID("op-"+name), f))
		got := ofType[model.Interaction](t, lines, model.ExportLineInteraction)
		want := 2
		if name == "conversation" {
			want = 1
		}
		if len(got) != want {
			t.Fatalf("%s export holds %d interactions, want %d", name, len(got), want)
		}
		if i := slices.IndexFunc(got, func(g model.Interaction) bool { return g.Ref == x.Ref }); i < 0 || got[i].Arguments != secret {
			t.Fatalf("%s export interactions %+v, want the record as stored", name, got)
		}
	}

	// A contact export holds the interactions its identities performed.
	var alice model.Identity
	h.View(func(r store.Reader) (err error) { alice, err = r.GetIdentity(h.Ctx, x.Actor.Identity); return err })
	_, _, lines := export(t, h.Client, h.Ctx, exportReq("op-contact", model.Filter{Contact: &alice.Contact}))
	if got := ofType[model.Interaction](t, lines, model.ExportLineInteraction); len(got) != 1 || got[0].Ref != x.Ref {
		t.Fatalf("contact export interactions %+v, want alice's alone", got)
	}

	// Reading is decided per record: a denied read leaves the line out.
	deny[model.ActionInteractionRead] = true
	_, _, lines = export(t, h.Client, h.Ctx, exportReq("op-denied", model.Filter{Inboxes: []model.InboxRef{in.Ref}}))
	if got := ofType[model.Interaction](t, lines, model.ExportLineInteraction); len(got) != 0 {
		t.Fatalf("export holds %d interactions under a denied read, want none", len(got))
	}
	// Opening reauthorizes what the export holds.
	deny[model.ActionInteractionRead] = false
	a, err := h.Client.Privacy().Export(h.Ctx, exportReq("op-stale", model.Filter{Inboxes: []model.InboxRef{in.Ref}}))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := a.Operation.Result.AsExport()
	deny[model.ActionInteractionRead] = true
	_, _, err = h.Client.Privacy().Open(h.Ctx, model.Lookup[model.ExportRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
	enginetest.RequireCode(t, err, model.Unauthorized, "")
	deny[model.ActionInteractionRead] = false

	if _, err := h.Client.Privacy().Redact(h.Ctx, redactInteraction(y, "op-redact")); err != nil {
		t.Fatal(err)
	}
	_, _, err = h.Client.Privacy().Open(h.Ctx, model.Lookup[model.ExportRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
	enginetest.RequireCode(t, err, model.Conflict, "superseded")
	// An export built after the redaction lists it as a tombstone and opens.
	_, _, lines = export(t, h.Client, h.Ctx, exportReq("op-after", model.Filter{Inboxes: []model.InboxRef{in.Ref}}))
	for _, g := range ofType[model.Interaction](t, lines, model.ExportLineInteraction) {
		if g.Ref == y.Ref && (!g.Redacted || g.Arguments != "") {
			t.Fatalf("exported tombstone %+v", g)
		}
	}
}

// A conversation cascade redacts the interactions of the conversation with
// its messages, counting each in its progress.
func TestCascadeRedactsInteractions(t *testing.T) {
	h := interactionHarness(t)
	in := h.Configure(enginetest.PerContact(true), enginetest.Dest)
	h.MustAccept(h.Inbound("e1", "m1", "alice", enginetest.Dest))
	a1, _ := accept(t, h, in.Ref, "i1", "alice")
	a2, _ := accept(t, h, in.Ref, "i2", "alice")
	b1, _ := accept(t, h, in.Ref, "i3", "bob")
	conv := a1.Ref.Conversation
	var cv model.Conversation
	h.View(func(r store.Reader) (err error) { cv, err = r.GetConversation(h.Ctx, conv); return err })

	before := len(h.Events())
	res, err := h.Client.Privacy().Redact(h.Ctx, cascade(cv.Ref.Ref(), "op-cascade", cv.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if got := interactionOf(t, h, in.Ref, "i1"); got.Redacted {
		t.Fatal("cascade redacted before any worker ran")
	}
	drive(t, h.Ctx, h.RT, enginetest.TenantA)
	op := operation(t, h.Client, h.Ctx, res.Operation.Ref)
	if op.State != model.Committed || done(op) != 3 {
		t.Fatalf("cascade operation %+v, want committed with the message and 2 interactions done", op)
	}
	for _, x := range []model.Interaction{a1, a2} {
		got := interactionOf(t, h, in.Ref, x.External[0].ID)
		if !got.Redacted || got.Arguments != "" || len(got.Fields.Data) != 0 || tokenOf(h, got.Ref) != "" {
			t.Errorf("interaction %s after the cascade = %+v, want a tombstone without token", x.Ref.ID, got)
		}
	}
	if got := interactionOf(t, h, in.Ref, "i3"); got.Redacted || got.Revision != b1.Revision || tokenOf(h, b1.Ref) != responseToken {
		t.Errorf("another conversation's interaction touched: %+v", got)
	}
	if n := len(updates(h.Events()[before:])); n != 2 {
		t.Fatalf("interaction.updated events = %d, want one per interaction", n)
	}
	requireNoSecret(t, h.Events())
}

// A contact cascade redacts the interactions its identities performed, in
// bounded batches, each exactly once; an identity cascade only that
// identity's.
func TestCascadeRedactsContactInteractions(t *testing.T) {
	for _, kind := range []string{"contact", "identity"} {
		t.Run(kind, func(t *testing.T) {
			h := interactionHarness(t)
			in := h.Configure(enginetest.PerContact(false), enginetest.Dest)
			h.MustAccept(h.Inbound("e1", "m1", "alice", enginetest.Dest))
			const n = 230
			for i := range n {
				accept(t, h, in.Ref, "a"+string(rune('A'+i/26))+string(rune('a'+i%26)), "alice")
			}
			bob, _ := accept(t, h, in.Ref, "b1", "bob")
			var alice model.Identity
			var contact model.Contact
			h.View(func(r store.Reader) (err error) {
				m := h.MessageByProviderID(in.Ref, "m1")
				if alice, err = r.GetIdentity(h.Ctx, m.Author.Identity); err != nil {
					return err
				}
				contact, err = r.GetContact(h.Ctx, alice.Contact)
				return err
			})
			target, rev := contact.Ref.Ref(), contact.Revision
			if kind == "identity" {
				target, rev = alice.Ref.Ref(), alice.Revision
			}
			res, err := h.Client.Privacy().Redact(h.Ctx, cascade(target, "op-cascade", rev))
			if err != nil {
				t.Fatal(err)
			}
			steps := drive(t, h.Ctx, h.RT, enginetest.TenantA)
			op := operation(t, h.Client, h.Ctx, res.Operation.Ref)
			if op.State != model.Committed || done(op) != n+1 || steps < 3 {
				t.Fatalf("cascade %+v after %d steps, want committed with %d done in bounded batches", op, steps, n+1)
			}
			counts := map[model.ID]int{}
			for _, ev := range updates(h.Events()) {
				counts[ev.Resource.ID]++
			}
			for _, x := range enginetest.All(h, store.Reader.Interactions, model.Filter{Inboxes: []model.InboxRef{in.Ref}}) {
				if x.Actor.Identity == alice.Ref {
					if !x.Redacted || counts[x.Ref.ID] != 1 || tokenOf(h, x.Ref) != "" {
						t.Fatalf("alice's interaction %s redacted %v %d times", x.Ref.ID, x.Redacted, counts[x.Ref.ID])
					}
					continue
				}
				if x.Ref != bob.Ref || x.Redacted || x.Revision != bob.Revision {
					t.Fatalf("bob's interaction touched: %+v", x)
				}
			}
		})
	}
}

// A contact cascade skips an identity its actor may not redact, and that
// identity keeps its interactions.
func TestCascadeSkippedIdentityKeepsInteractions(t *testing.T) {
	s := seedSpread(t, nil)
	h := s.h
	for _, name := range []string{"alice", "carol"} {
		env := h.Interaction("i-"+name, name, enginetest.Dest)
		env.Interaction.Arguments = secret
		h.MustAccept(env)
	}
	s.denyRedact("carol")
	a, err := s.cascadeOn(s.shared2, "op-shared")
	if err != nil {
		t.Fatal(err)
	}
	drive(t, h.Ctx, h.RT, enginetest.TenantA)
	if op := operation(t, h.Client, h.Ctx, a.Operation.Ref); op.State != model.Committed || !op.Progress.Partial {
		t.Fatalf("cascade %+v, want a committed partial operation", op)
	}
	for name, redacted := range map[string]bool{"alice": true, "carol": false} {
		x := interactionOf(t, h, s.one, "i-"+name)
		if x.Redacted != redacted || (!redacted && x.Arguments != secret) {
			t.Errorf("%s's interaction redacted = %v, want %v", name, x.Redacted, redacted)
		}
	}
}
