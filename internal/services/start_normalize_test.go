package services_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// normalizingFactory is the rig's adapter with adapter.RecipientNormalizer
// added: fn decides each recipient, and seen records what core asked about.
type normalizingFactory struct {
	*outFactory
	fn func(model.ExternalRef) (model.ExternalRef, error)

	mu   sync.Mutex
	seen []model.ExternalRef
}

func (f *normalizingFactory) NormalizeRecipient(r model.ExternalRef) (model.ExternalRef, error) {
	f.mu.Lock()
	f.seen = append(f.seen, r)
	f.mu.Unlock()
	return f.fn(r)
}

func (f *normalizingFactory) asked() []model.ExternalRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.ExternalRef(nil), f.seen...)
}

var _ adapter.RecipientNormalizer = (*normalizingFactory)(nil)

// lowerID is a normalizer that keeps one spelling per identity: lowercase.
func lowerID(r model.ExternalRef) (model.ExternalRef, error) {
	r.ID = strings.ToLower(r.ID)
	return r, nil
}

// withNormalizer swaps the rig's adapter for one implementing
// adapter.RecipientNormalizer with fn.
func withNormalizer(fn func(model.ExternalRef) (model.ExternalRef, error)) (enginetest.RigOption, **normalizingFactory) {
	var nf *normalizingFactory
	return func(c *engine.Config, f *outFactory) {
		nf = &normalizingFactory{outFactory: f, fn: fn}
		c.Adapters = []adapter.Factory{nf}
	}, &nf
}

// claimed lists the identities of the start inbox.
func claimed(t *testing.T, r *rig, in model.InboxRef) []model.Identity {
	t.Helper()
	var out []model.Identity
	err := r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) error {
		p, err := rd.Identities(r.Ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 50}})
		out = p.Items
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func conversationsOf(t *testing.T, r *rig, in model.InboxRef) []model.Conversation {
	t.Helper()
	var out []model.Conversation
	err := r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) error {
		p, err := rd.Conversations(r.Ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 50}})
		out = p.Items
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A factory that normalizes makes identity, conversation route and the
// provider's recipient see the normalized spelling.
func TestStartNormalizesRecipient(t *testing.T) {
	opt, nf := withNormalizer(lowerID)
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	acc, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Ann@Example.COM")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ids := claimed(t, r, in)
	if len(ids) != 1 || ids[0].SourceID != "ann@example.com" || ids[0].Namespace != routeNSpace {
		t.Fatalf("identities %+v, want one claimed as ann@example.com", ids)
	}
	convs := conversationsOf(t, r, in)
	if len(convs) != 1 || convs[0].ReplyRoute.ID != "ann@example.com" {
		t.Fatalf("conversations %+v, want one routed to ann@example.com", convs)
	}
	if seen := (*nf).asked(); len(seen) != 1 || seen[0].ID != "Ann@Example.COM" {
		t.Fatalf("normalizer asked about %+v, want the recipient as sent, once", seen)
	}
	r.Step("w")
	enginetest.RequireState(t, r.Op(acc.Operation.Ref), model.ProviderAccepted, "")
	sends := r.F.ProviderSends()
	if len(sends) != 1 || sends[0].Route.ID != "ann@example.com" {
		t.Fatalf("provider sends %+v, want one to the normalized route", sends)
	}

	// Another spelling of the same recipient reaches the same identity and,
	// the inbox being locked to a single conversation, the same conversation.
	if _, err := r.C.Conversations().Start(r.Ctx, startReq("op-2", in, recipient(r, "ANN@example.com"))); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if n := len(claimed(t, r, in)); n != 1 {
		t.Fatalf("identities = %d after a second spelling, want 1", n)
	}
	if n := len(conversationsOf(t, r, in)); n != 1 {
		t.Fatalf("conversations = %d after a second spelling, want 1", n)
	}
}

// The same operation key with another spelling of the recipient is the same
// operation: replayed, never a conflict, nothing second written.
func TestStartReplayUnderAnotherSpellingIsTheSameOperation(t *testing.T) {
	opt, _ := withNormalizer(lowerID)
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	first, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Ann@Example.COM")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, id := range []string{"ann@example.com", "ANN@EXAMPLE.COM", "Ann@Example.COM"} {
		again, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, id)))
		if err != nil {
			t.Fatalf("replay as %q: %v", id, err)
		}
		if !again.Replayed || again.Operation.Ref != first.Operation.Ref {
			t.Fatalf("replay as %q = %+v, want the first operation replayed", id, again)
		}
	}
	if n := len(r.Claim("w")); n != 1 {
		t.Fatalf("work items = %d, want the first start's one", n)
	}
	if n := len(conversationsOf(t, r, in)); n != 1 {
		t.Fatalf("conversations = %d, want 1", n)
	}
}

// A normalizer that refuses the recipient refuses the start with a
// validation error before anything is written, and the operation ID stays
// free.
func TestStartNormalizerErrorRefusesBeforeAnyWrite(t *testing.T) {
	refuse := true
	opt, _ := withNormalizer(func(r model.ExternalRef) (model.ExternalRef, error) {
		if refuse {
			return r, model.NewError(model.Validation, "not an address")
		}
		return r, nil
	})
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	_, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "u-1")))
	requireCode(t, err, model.Validation, "recipient: not an address")
	assertNothingWritten(t, r, in)

	// An error that is no *model.Error is refused as validation too, its
	// text kept out of the message.
	refuse = false
	bad := errors.New("secret detail")
	opt2, _ := withNormalizer(func(r model.ExternalRef) (model.ExternalRef, error) { return r, bad })
	r2, in2 := newStartRig(t, perContact(true), enginetest.WithStart, opt2)
	_, err = r2.C.Conversations().Start(r2.Ctx, startReq("op-1", in2, recipient(r2, "u-1")))
	requireCode(t, err, model.Validation, "recipient")
	if strings.Contains(err.Error(), "secret detail") {
		t.Fatalf("error %q leaks the adapter's error text", err)
	}
	assertNothingWritten(t, r2, in2)

	// The operation ID was never claimed.
	r3, in3 := newStartRig(t, perContact(true), enginetest.WithStart)
	if _, err := r3.C.Conversations().Start(r3.Ctx, startReq("op-1", in3, recipient(r3, "u-1"))); err != nil {
		t.Fatalf("a start without a normalizer: %v", err)
	}
}

func assertNothingWritten(t *testing.T, r *rig, in model.InboxRef) {
	t.Helper()
	if n := len(r.Claim("w")); n != 0 {
		t.Fatalf("a refused start queued %d work items", n)
	}
	if n := len(claimed(t, r, in)); n != 0 {
		t.Fatalf("a refused start claimed %d identities", n)
	}
	if n := len(conversationsOf(t, r, in)); n != 0 {
		t.Fatalf("a refused start opened %d conversations", n)
	}
	if acts := r.Policy.Actions(); len(acts) != 0 {
		t.Fatalf("decisions %v asked for a refused start", acts)
	}
}

// Normalization may change the ID only: another channel, namespace, kind or
// parent, or a result that is no valid reference, is an adapter fault and
// refuses the start with nothing written.
func TestStartNormalizerMayChangeOnlyTheID(t *testing.T) {
	for name, mutate := range map[string]func(*model.ExternalRef){
		"namespace": func(r *model.ExternalRef) { r.Namespace = "other.ns" },
		"kind":      func(r *model.ExternalRef) { r.Kind = "room" },
		"parent":    func(r *model.ExternalRef) { r.ParentID = "p" },
		"channel":   func(r *model.ExternalRef) { r.Channel.ID = enginetest.ChanTwo },
		"empty id":  func(r *model.ExternalRef) { r.ID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			opt, _ := withNormalizer(func(r model.ExternalRef) (model.ExternalRef, error) {
				mutate(&r)
				return r, nil
			})
			r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
			_, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "u-1")))
			requireCode(t, err, model.Permanent, "normalized recipient")
			assertNothingWritten(t, r, in)
		})
	}
}

// Only a recipient of the inbox's channel reaches the normalizer; another
// channel's is refused as before.
func TestStartNormalizerSeesOnlyTheInboxChannel(t *testing.T) {
	opt, nf := withNormalizer(lowerID)
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	to := recipient(r, "U-1")
	to.Channel.ID = enginetest.ChanTwo
	_, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, to))
	requireCode(t, err, model.Validation, "inbox's channel")
	if seen := (*nf).asked(); len(seen) != 0 {
		t.Fatalf("normalizer asked about %+v, want nothing", seen)
	}
}

// A factory without the interface behaves exactly as before: the recipient
// keeps its spelling, so two spellings are two identities and two
// operations' fingerprints differ.
func TestStartWithoutNormalizerKeepsTheRecipientAsSent(t *testing.T) {
	r, in := newStartRig(t, perContact(true), enginetest.WithStart)
	if _, ok := any(r.F).(adapter.RecipientNormalizer); ok {
		t.Fatal("the plain rig factory implements RecipientNormalizer")
	}
	if _, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Ann@Example.COM"))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := r.C.Conversations().Start(r.Ctx, startReq("op-2", in, recipient(r, "ann@example.com"))); err != nil {
		t.Fatalf("second start: %v", err)
	}
	ids := map[string]bool{}
	for _, i := range claimed(t, r, in) {
		ids[i.SourceID] = true
	}
	if len(ids) != 2 || !ids["Ann@Example.COM"] || !ids["ann@example.com"] {
		t.Fatalf("identities %v, want both spellings kept as sent", ids)
	}
	_, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "ann@example.com")))
	requireCode(t, err, model.Conflict, "")
}
