package conformance

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// contactCases are the identity-link cases of RunServiceSuite: Link,
// Unlink, History, List and Identities through the engine, their policy
// and verifier boundaries, their effects on routes, selection and reads,
// races with ingress, other links and retention, and the privacy cascade
// and retention purge of link history and orphaned contacts.
var contactCases = []struct {
	name string
	run  func(*testing.T, *contactsEnv)
}{
	{"LinkThroughLedger", contactsLinkThroughLedger},
	{"UnlinkThenMergeBack", contactsUnlinkThenMergeBack},
	{"UnlinkRulesThroughLedger", contactsUnlinkRules},
	{"OverrideRequiresDistinctAction", contactsOverrideRequiresDistinctAction},
	{"VerifierCannotBypassScope", contactsVerifierCannotBypassScope},
	{"EvidenceIntentIsNotTransferable", contactsEvidenceIntentNotTransferable},
	{"DecisionsCarryVerifiedEvidenceOnly", contactsDecisionsCarryVerifiedEvidence},
	{"StaleRevisionAndSingleUseConflict", contactsStaleAndSingleUse},
	{"ReplayAfterOperationPurgeIsNewCommand", contactsReplayAfterPurge},
	{"ReplyRouteAndSelectionUnchangedAfterLink", contactsRouteAndSelectionUnchanged},
	{"LinkedContactGrantsNoCrossInboxRead", contactsNoCrossInboxRead},
	{"ListHidesUnreadableInboxes", contactsListHidesUnreadableInboxes},
	{"QueriesFilterAndPage", contactsQueriesFilterAndPage},
	{"HistoryChainAndCursors", contactsHistoryChainAndCursors},
	{"MergeThroughLedger", contactsMergeThroughLedger},
	{"ConcurrentMergesOneWins", contactsConcurrentMerges},
	{"ConcurrentLinksOneWins", contactsConcurrentLinks},
	{"ConcurrentSameOperationReplays", contactsConcurrentSameOperation},
	{"ConcurrentClaimAndLinkNoOrphanContact", contactsConcurrentClaimAndLink},
	{"AnchorRelinkedBeforeCommit", contactsAnchorRelinkedBeforeCommit},
	{"LinkRacesOrphanPurge", contactsLinkRacesOrphanPurge},
	{"PurgedContactIsGoneForRelink", contactsPurgedContactGone},
	{"OrphanPurgeBatches", contactsOrphanPurgeBatches},
	{"CascadeTombstonesLinkHistory", contactsCascadeTombstones},
	{"ContactCascadeSkipsDeniedInboxes", contactsContactCascadeSkipsDenied},
	{"ContactCascadeThreshold", contactsContactCascadeThreshold},
	{"ContactCascadeThresholdPerInboxAndTenant", contactsContactCascadeThresholdScope},
	{"MessageRedactionLeavesLinks", contactsMessageRedactionLeavesLinks},
	{"IngressSenderKindRecordedAtClaim", contactsIngressSenderKind},
	{"IngressReplyLinkResolved", contactsIngressReplyLink},
	{"IngressSenderAttributesRecordedAtClaim", contactsIngressSenderAttributes},
}

// contactsEnv is one engine over a fresh store with a chat channel, two
// per-contact inboxes and the sink bound to it. The attestation verifier
// is honest by default; host_session answers only once a case scripts it.
type contactsEnv struct {
	t       *testing.T
	ctx     context.Context
	fx      StoreFixture
	st      store.Store
	clock   *portstest.FixedClock
	engine  uma.Engine
	client  uma.Client
	pol     *contactsPolicy
	limits  *cascadeLimits
	attest  *ScriptedVerifier
	session *ScriptedVerifier
	// attribute answers verified_attribute evidence once a case scripts it.
	attribute *ScriptedVerifier
	chat      *chatAdapter
	sink      adapter.Sink
	ids       *portstest.SequenceIDs
	ch        model.ChannelRef
	one       model.InboxRef
	two       model.InboxRef
	n         atomic.Int64
}

const contactsActor model.ActorID = "staff-1"

// sessionVersion is the version the host_session verifier reports.
const sessionVersion = "hs-1"

type versioned struct {
	*ScriptedVerifier
	v string
}

func (v versioned) Version() string { return v.v }

func newContactsEnv(t *testing.T, factory StoreFactory) *contactsEnv {
	t.Helper()
	fx := factory(t)
	env := &contactsEnv{
		t: t, ctx: t.Context(), fx: fx, st: fx.Store, clock: portstest.NewFixedClock(fx.Now()),
		pol: &contactsPolicy{}, limits: &cascadeLimits{}, attest: &ScriptedVerifier{}, session: &ScriptedVerifier{}, attribute: &ScriptedVerifier{}, chat: &chatAdapter{},
		ids: portstest.NewSequenceIDs(0xc0c),
	}
	env.attest.Script(AttestationVerifier{}.Evidence)
	e, err := uma.New(uma.Config{
		Store: fx.Store, Authorizer: env.pol, CascadePolicy: env.limits, Clock: env.clock, IDs: portstest.NewSequenceIDs(0xc0b),
		Adapters: []adapter.Factory{env.chat},
		IdentityVerifiers: map[string]policy.IdentityVerifier{
			model.EvidenceKindAttestation:       env.attest,
			model.EvidenceKindHostSession:       versioned{env.session, sessionVersion},
			model.EvidenceKindVerifiedAttribute: env.attribute,
		},
		Retention: uma.RetentionPolicy{
			Events: serviceHorizon, Operations: serviceHorizon, Ingress: serviceHorizon,
			Attempts: serviceHorizon, Staging: serviceHorizon, Exports: serviceHorizon,
			Contacts: serviceHorizon,
		},
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	env.engine = e
	if env.client, err = e.For(model.Principal{Tenant: serviceTenant, Actor: contactsActor}); err != nil {
		t.Fatal(err)
	}
	ca, err := env.client.Configuration().ConfigureChannel(env.ctx, model.ConfigureChannel{
		Command: env.cmd(""),
		Channel: model.Channel{Ref: model.ChannelRef{Tenant: serviceTenant}, Type: chatType, Configuration: model.Extension{Name: chatSchema, Version: 1}},
	})
	if err != nil {
		t.Fatalf("ConfigureChannel: %v", err)
	}
	if env.ch, err = ca.Operation.Result.AsChannel(); err != nil {
		t.Fatal(err)
	}
	env.one, env.two = env.configureInbox(chatOne), env.configureInbox(chatTwo)
	service := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "chat-service"}, Call: model.Call{Correlation: "corr"}}
	if _, err := e.Runtime().Webhook(env.ctx, service, env.ch); err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	env.chat.mu.Lock()
	env.sink = env.chat.sink
	env.chat.mu.Unlock()
	if env.sink == nil {
		t.Fatal("webhook binding did not hand the instance a sink")
	}
	env.pol.set(nil)
	return env
}

func (env *contactsEnv) configureInbox(dest string) model.InboxRef {
	env.t.Helper()
	lock := true
	a, err := env.client.Configuration().ConfigureInbox(env.ctx, model.ConfigureInbox{
		Command: env.cmd(""),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: serviceTenant}, Channel: env.ch, Name: dest, Destinations: []string{dest},
			Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
		},
	})
	if err != nil {
		env.t.Fatalf("ConfigureInbox: %v", err)
	}
	ref, err := a.Operation.Result.AsInbox()
	if err != nil {
		env.t.Fatal(err)
	}
	return ref
}

// op returns a fresh operation ID.
func (env *contactsEnv) op() model.OperationID {
	return model.OperationID(fmt.Sprintf("op-%d", env.n.Add(1)))
}

func (env *contactsEnv) cmd(rev model.Revision) model.Command {
	return model.Command{Call: model.Call{Correlation: "corr-contacts"}, Operation: env.op(), ExpectedRevision: rev}
}

func (env *contactsEnv) id() model.ID {
	id, err := env.ids.New(env.ctx)
	if err != nil {
		env.t.Fatal(err)
	}
	return id
}

func (env *contactsEnv) dest(in model.InboxRef) string {
	if in == env.two {
		return chatTwo
	}
	return chatOne
}

func (env *contactsEnv) user(sender string) model.ExternalRef {
	return model.ExternalRef{Channel: env.ch, Namespace: chatNS, Kind: "user", ID: sender}
}

// envelope is a new public message from sender into in.
func (env *contactsEnv) envelope(in model.InboxRef, sender string) model.Envelope {
	n := env.n.Add(1)
	return model.Envelope{
		EventID: fmt.Sprintf("ev-%d", n), MessageID: fmt.Sprintf("pm-%d", n), Destination: env.dest(in),
		Kind: model.EnvelopeMessage, Sender: env.user(sender), Assurance: "verified",
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello from " + sender}}},
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: env.clock.Now(),
	}
}

func (env *contactsEnv) accept(e model.Envelope) error {
	_, err := env.sink.Accept(env.ctx, model.Ingress{Call: model.Call{Correlation: model.CorrelationID("corr-" + e.EventID)}, Envelope: e})
	return err
}

// arrive accepts a message from sender into in and returns the sender's
// identity and the stored message.
func (env *contactsEnv) arrive(in model.InboxRef, sender string) (model.Identity, model.Message) {
	env.t.Helper()
	return env.arriveAs(in, sender, "")
}

// arriveAs is arrive with the sender reported as kind.
func (env *contactsEnv) arriveAs(in model.InboxRef, sender, kind string) (model.Identity, model.Message) {
	env.t.Helper()
	e := env.envelope(in, sender)
	e.SenderKind = kind
	if err := env.accept(e); err != nil {
		env.t.Fatalf("Accept(%s): %v", e.EventID, err)
	}
	var msg model.Message
	env.view(func(r store.Reader) error {
		ms, err := r.MessagesByReference(env.ctx, in, model.ExternalRef{
			Channel: env.ch, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: e.MessageID,
		}, 2)
		if err != nil {
			return err
		}
		if len(ms) != 1 {
			return fmt.Errorf("messages for %s = %d, want 1", e.MessageID, len(ms))
		}
		msg = ms[0]
		return nil
	})
	return env.byKey(in, sender), msg
}

func (env *contactsEnv) view(fn func(store.Reader) error) {
	env.t.Helper()
	if err := env.st.View(env.ctx, serviceTenant, fn); err != nil {
		env.t.Fatalf("view: %v", err)
	}
}

func (env *contactsEnv) put(fn func(store.Tx) error) {
	env.t.Helper()
	if err := env.st.Transact(env.ctx, serviceTenant, fn); err != nil {
		env.t.Fatalf("seed: %v", err)
	}
}

func (env *contactsEnv) byKey(in model.InboxRef, sender string) model.Identity {
	env.t.Helper()
	var id model.Identity
	env.view(func(r store.Reader) (err error) {
		id, err = r.Identity(env.ctx, model.IdentityKey{Inbox: in, Namespace: chatNS, SourceID: sender})
		return err
	})
	return id
}

func (env *contactsEnv) identity(ref model.IdentityRef) model.Identity {
	env.t.Helper()
	var id model.Identity
	env.view(func(r store.Reader) (err error) { id, err = r.GetIdentity(env.ctx, ref); return err })
	return id
}

func (env *contactsEnv) contact(ref model.ContactRef) (model.Contact, error) {
	var c model.Contact
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) (err error) { c, err = r.GetContact(env.ctx, ref); return err })
	return c, err
}

// person stores a person contact with no identities.
func (env *contactsEnv) person() model.Contact {
	env.t.Helper()
	c := model.Contact{Ref: model.ContactRef{Tenant: serviceTenant, ID: env.id()}, Kind: model.ContactKindPerson, Revision: "c1"}
	env.put(func(tx store.Tx) error { return tx.PutContact(env.ctx, c, "") })
	return c
}

// linkReq links id to `to` with attestation evidence under ref.
func (env *contactsEnv) linkReq(id model.Identity, to model.ContactRef, ref string) model.LinkIdentity {
	return model.LinkIdentity{
		Command: env.cmd(id.Revision), Identity: id.Ref, Contact: to,
		Evidence: model.Evidence{Verifier: model.EvidenceKindAttestation, Reference: ref},
	}
}

func (env *contactsEnv) link(req model.LinkIdentity) (model.Acceptance, error) {
	return env.client.Contacts().Link(env.ctx, req)
}

// mustLink links id to `to` with attestation evidence and refreshes id.
func (env *contactsEnv) mustLink(id *model.Identity, to model.ContactRef, ref string) model.Acceptance {
	env.t.Helper()
	acc, err := env.link(env.linkReq(*id, to, ref))
	if err != nil {
		env.t.Fatalf("Link %s: %v", id.Ref.ID, err)
	}
	*id = env.identity(id.Ref)
	return acc
}

func (env *contactsEnv) unlinkReq(id model.Identity, reason string, ev model.Evidence) model.UnlinkIdentity {
	return model.UnlinkIdentity{Command: env.cmd(id.Revision), Identity: id.Ref, Reason: reason, Evidence: ev}
}

func (env *contactsEnv) unlink(req model.UnlinkIdentity) (model.Acceptance, error) {
	return env.client.Contacts().Unlink(env.ctx, req)
}
