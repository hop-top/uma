package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"sync"
	"testing"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// audienceAuthorizer allows everything except reading a message or an
// attachment under a non-public audience, which only staff actors may.
type audienceAuthorizer struct {
	mu    sync.Mutex
	staff map[model.ActorID]bool
}

func (a *audienceAuthorizer) Authorize(_ context.Context, r policy.Authorization) (policy.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	read := r.Action == model.ActionMessageRead || r.Action == model.ActionAttachmentRead
	if read && r.Audience.Visibility != model.Public && !a.staff[r.Principal.Actor] {
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true}, nil
}

func (a *audienceAuthorizer) setStaff(actor model.ActorID, v bool) {
	a.mu.Lock()
	a.staff[actor] = v
	a.mu.Unlock()
}

// privacyView is one principal's view of the surfaces privacy must hold
// across.
type privacyView struct {
	env    *serviceEnv
	client uma.Client
}

func (v privacyView) call() model.Call { return model.Call{Correlation: "privacy"} }

func (v privacyView) get(ref model.MessageRef) error {
	_, err := v.client.Messages().Get(v.env.ctx, model.Lookup[model.MessageRef]{Call: v.call(), Ref: ref})
	return err
}

func (v privacyView) list() []model.ID {
	v.env.t.Helper()
	conv := v.env.conv.Ref
	p, err := v.client.Messages().List(v.env.ctx, serviceList(model.Filter{Conversation: &conv}, 50, ""))
	if err != nil || !p.Complete {
		v.env.t.Fatalf("Messages.List = %+v, %v", p, err)
	}
	return messageIDs(p.Items)
}

// open reads an attachment's bytes, or returns the refusal.
func (v privacyView) open(ref model.AttachmentRef) ([]byte, error) {
	v.env.t.Helper()
	_, rc, err := v.client.Attachments().Open(v.env.ctx, model.Lookup[model.AttachmentRef]{Call: v.call(), Ref: ref})
	if err != nil {
		if rc != nil {
			v.env.t.Fatal("a refused attachment Open returned a reader")
		}
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		v.env.t.Fatalf("read attachment: %v", err)
	}
	return b, nil
}

// export exports the conversation and returns the export's reference.
func (v privacyView) export(op model.OperationID) model.ExportRef {
	v.env.t.Helper()
	conv := v.env.conv.Ref
	a, err := v.client.Privacy().Export(v.env.ctx, model.ExportRequest{
		Command: model.Command{Call: v.call(), Operation: op}, Filter: model.Filter{Conversation: &conv}, Format: model.ExportFormatJSONL,
	})
	if err != nil {
		v.env.t.Fatalf("Export: %v", err)
	}
	ref, err := a.Operation.Result.AsExport()
	if err != nil {
		v.env.t.Fatal(err)
	}
	return ref
}

// lines opens an export and returns the resource references of its
// message and attachment lines, or the refusal.
func (v privacyView) lines(ref model.ExportRef) ([]model.Ref, error) {
	v.env.t.Helper()
	_, rc, err := v.client.Privacy().Open(v.env.ctx, model.Lookup[model.ExportRef]{Call: v.call(), Ref: ref})
	if err != nil {
		if rc != nil {
			v.env.t.Fatal("a refused export Open returned a reader")
		}
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		v.env.t.Fatalf("read export: %v", err)
	}
	var out []model.Ref
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var l model.ExportLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			v.env.t.Fatalf("export line %q: %v", sc.Text(), err)
		}
		switch l.Type {
		case model.ExportLineMessage:
			var m model.Message
			if err := json.Unmarshal(l.Data, &m); err != nil {
				v.env.t.Fatal(err)
			}
			out = append(out, m.Ref.Ref())
		case model.ExportLineAttachment:
			var a model.Attachment
			if err := json.Unmarshal(l.Data, &a); err != nil {
				v.env.t.Fatal(err)
			}
			out = append(out, a.Ref.Ref())
		}
	}
	return out, nil
}

// subscribe opens a subscription from the beginning of the log.
func (v privacyView) subscribe(consumer string) model.SubscriptionRef {
	v.env.t.Helper()
	res, err := v.client.Events().Open(v.env.ctx, model.OpenSubscription{
		Command: v.env.cmd(""), Consumer: consumer, Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		v.env.t.Fatalf("Events.Open: %v", err)
	}
	return res.Subscription.Ref
}

// delivered polls sub and returns the resources of the events delivered;
// the batch stays outstanding, so a later poll redelivers it.
func (v privacyView) delivered(sub model.SubscriptionRef) []model.Ref {
	v.env.t.Helper()
	b, err := v.client.Events().Poll(v.env.ctx, model.Poll{Call: v.call(), Subscription: sub, Limit: 100})
	if err != nil {
		v.env.t.Fatalf("Poll: %v", err)
	}
	out := make([]model.Ref, 0, len(b.Events))
	for _, ev := range b.Events {
		out = append(out, ev.Resource)
	}
	return out
}

// servicePrivacyAcrossSurfaces holds internal notes and narrowed messages
// restricted on every read surface, Messages, event delivery, Attachments
// and Exports, for another actor and for the creator once its permission
// is revoked after the records, the subscription and the export exist.
// newPrivacyEnv starts an engine with blobs over factory's store, under an
// audienceAuthorizer where staff-1 is staff, and returns the views of
// staff-1 and of member-1.
func newPrivacyEnv(t *testing.T, factory StoreFactory) (*audienceAuthorizer, privacyView, privacyView) {
	t.Helper()
	fx := factory(t)
	pol := &audienceAuthorizer{staff: map[model.ActorID]bool{"staff-1": true}}
	clock := portstest.NewFixedClock(fx.Now())
	e, err := uma.New(uma.Config{
		Store: fx.Store, Authorizer: pol, Clock: clock, IDs: portstest.NewSequenceIDs(0xb0f),
		Adapters: []adapter.Factory{serviceAdapter{}}, Blobs: &portstest.MemoryBlobs{},
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	staffClient, err := e.For(model.Principal{Tenant: serviceTenant, Actor: "staff-1"})
	if err != nil {
		t.Fatal(err)
	}
	memberClient, err := e.For(model.Principal{Tenant: serviceTenant, Actor: "member-1"})
	if err != nil {
		t.Fatal(err)
	}
	env := &serviceEnv{
		t: t, ctx: t.Context(), fx: fx, clock: clock, engine: e, st: fx.Store,
		ids: portstest.NewSequenceIDs(0xb0e), client: staffClient,
	}
	env.ch = env.configureChannel()
	env.inbox = env.configureInbox("support")
	env.conv = env.conversation(env.inbox, model.ConversationOpen)
	return pol, privacyView{env, staffClient}, privacyView{env, memberClient}
}

// upload prepares and uploads an attachment named name under vis.
func (v privacyView) upload(name string, vis model.Visibility) model.AttachmentRef {
	t, env := v.env.t, v.env
	t.Helper()
	meta := model.AttachmentMetadata{Name: name, MediaType: "text/plain", Audience: model.Audience{Visibility: vis}}
	a, err := v.client.Attachments().Prepare(env.ctx, model.PrepareAttachment{Command: env.cmd(""), Inbox: env.inbox, Metadata: meta})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ref, err := a.Operation.Result.AsAttachment()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.client.Attachments().Upload(env.ctx, model.Lookup[model.AttachmentRef]{Call: v.call(), Ref: ref}, env.cmd(""), bytes.NewReader([]byte(name+" bytes"))); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	return ref
}

// holding stores a message of the conversation under vis whose parts
// name att.
func (v privacyView) holding(vis model.Visibility, att model.AttachmentRef) model.Message {
	env := v.env
	env.t.Helper()
	m := model.Message{
		Ref: model.MessageRef{Conversation: env.conv.Ref, ID: env.id()}, Audience: model.Audience{Visibility: vis},
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "body"}, {Kind: model.PartAttachment, Attachment: &att}}},
		Revision: "m1",
	}
	env.put(func(tx store.Tx) error { return tx.PutMessage(env.ctx, m, "") })
	return m
}

func servicePrivacyAcrossSurfaces(t *testing.T, factory StoreFactory) {
	pol, staff, member := newPrivacyEnv(t, factory)
	env, clock, staffClient := staff.env, staff.env.clock, staff.client
	// A public message and its attachment, one later narrowed, and an
	// internal note with an internal attachment.
	pub, narrowed, internal := staff.upload("pub", model.Public), staff.upload("later", model.Public), staff.upload("note", model.Internal)
	p, q := staff.holding(model.Public, pub), staff.holding(model.Public, narrowed)
	na, err := staffClient.Messages().AddInternalNote(env.ctx, model.InternalNote{
		Command: env.cmd(""), Conversation: env.conv.Ref, PolicyRef: "staff",
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "internal text"}, {Kind: model.PartAttachment, Attachment: &internal}}},
	})
	if err != nil {
		t.Fatalf("AddInternalNote: %v", err)
	}
	note := *na.Operation.ResultMessage
	staffSub := staff.subscribe("staff-feed")
	if got := staff.delivered(staffSub); !slices.Contains(got, note.Ref()) {
		t.Fatalf("staff delivered %v, want the note's event", got)
	}
	staffExport := staff.export("op-staff-export")
	if got, err := staff.lines(staffExport); err != nil || !slices.Contains(got, note.Ref()) || !slices.Contains(got, internal.Ref()) {
		t.Fatalf("staff export lines = %v, %v; want the note and its attachment", got, err)
	}

	// Narrow q as an audience-only provider edit stores it, with its event.
	env.put(func(tx store.Tx) error {
		next := q
		next.Audience, next.Revision = model.Audience{Visibility: model.Internal}, "m2"
		if err := tx.PutMessage(env.ctx, next, "m1"); err != nil {
			return err
		}
		data, _ := json.Marshal(model.MessageUpdatedPayload{
			Message: q.Ref, Change: model.MessageChangeAudienceNarrowed, Trigger: model.TriggerProvider, PreviousRevision: "m1",
		})
		now := clock.Now()
		return tx.AppendEvents(env.ctx, []model.Event{{
			Version: model.EventEnvelopeVersion, ID: env.id(), Kind: model.EventMessageUpdated, Tenant: serviceTenant,
			Inbox: env.inbox.ID, Resource: q.Ref.Ref(), Actor: "provider", OccurredAt: now, RecordedAt: now,
			Correlation: "narrow", Audience: next.Audience, Revision: next.Revision,
			Payload: model.Extension{Name: model.SchemaMessageUpdated, Version: model.PayloadVersion1, Data: data},
		}})
	})

	requireRestricted := func(t *testing.T, v privacyView, sub model.SubscriptionRef, exp model.ExportRef) {
		t.Helper()
		for _, ref := range []model.MessageRef{note, q.Ref} {
			serviceCode(t, v.get(ref), model.Unauthorized)
		}
		if got := v.list(); !slices.Equal(got, []model.ID{p.Ref.ID}) {
			t.Errorf("Messages.List = %v, want only the public message %s", got, p.Ref.ID)
		}
		for _, ref := range []model.AttachmentRef{internal, narrowed} {
			_, err := v.open(ref)
			serviceCode(t, err, model.Unauthorized)
		}
		if b, err := v.open(pub); err != nil || string(b) != "pub bytes" {
			t.Errorf("public attachment = %q, %v", b, err)
		}
		for _, r := range v.delivered(sub) {
			if r == note.Ref() || r == q.Ref.Ref() {
				t.Errorf("event about restricted %+v delivered", r)
			}
		}
		_, err := v.lines(exp)
		serviceCode(t, err, model.Unauthorized)
	}

	t.Run("other actor", func(t *testing.T) {
		requireRestricted(t, member, member.subscribe("member-feed"), staffExport)
		// Its own export holds only what it may read, and opens.
		got, err := member.lines(member.export("op-member-export"))
		if err != nil || !slices.Equal(got, []model.Ref{p.Ref.Ref(), pub.Ref()}) {
			t.Errorf("member export lines = %v, %v; want the public message and its attachment", got, err)
		}
	})
	t.Run("revoked creator", func(t *testing.T) {
		pol.setStaff("staff-1", false)
		// The subscription's outstanding batch, delivered with the note
		// before, is redelivered without it.
		requireRestricted(t, staff, staffSub, staffExport)
	})
}

// serviceExportSupersededByRedaction refuses an export built before one of
// its messages or attachments was redacted, with model.Conflict and no
// byte, and opens the export built after it, which holds the tombstones.
func serviceExportSupersededByRedaction(t *testing.T, factory StoreFactory) {
	for _, target := range []string{"message", "attachment"} {
		t.Run(target, func(t *testing.T) {
			_, staff, _ := newPrivacyEnv(t, factory)
			env := staff.env
			att := staff.upload("scan", model.Public)
			m := staff.holding(model.Public, att)
			before := staff.export("op-before")
			if _, err := staff.lines(before); err != nil {
				t.Fatalf("open before redaction: %v", err)
			}
			req := model.Redact{Command: env.cmd("m1"), Target: m.Ref.Ref(), Message: &m.Ref, Reason: "erasure"}
			if target == "attachment" {
				var rev model.Revision
				env.put(func(tx store.Tx) error {
					rec, err := tx.Attachment(env.ctx, att)
					rev = rec.Attachment.Revision
					return err
				})
				req = model.Redact{Command: env.cmd(rev), Target: att.Ref(), Reason: "erasure"}
			}
			if _, err := staff.client.Privacy().Redact(env.ctx, req); err != nil {
				t.Fatalf("Redact: %v", err)
			}
			_, err := staff.lines(before)
			serviceCode(t, err, model.Conflict)
			if _, err := staff.lines(staff.export("op-after")); err != nil {
				t.Fatalf("export after redaction: %v", err)
			}
		})
	}
}
