package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// RunAttachmentSuite runs the Attachments service over factory's store and
// blobs' blob store: prepare, upload and open with declared and computed
// digests, size and media type limits, preserved audience, locators kept
// out of every public result and error, operation replay, cleanup of staged
// blobs after failed staging or a failed commit, reconciliation after an
// ambiguous commit, staging intents recorded before every Stage and bound
// on commit, the retention sweep of blobs a crash left unbound,
// cancellation, reference scope, redaction and export of a note holding an
// attachment, and a conversation cascade. A store passing
// RunStoreSuite and a blob store passing RunBlobStoreSuite should pass it
// unchanged.
func RunAttachmentSuite(t *testing.T, factory StoreFactory, blobs BlobFactory) {
	t.Helper()
	for _, c := range []struct {
		name string
		run  func(*testing.T, *attachEnv)
	}{
		{"PrepareUploadOpen", attachRoundTrip},
		{"DigestVerified", attachDigestVerified},
		{"DigestFormat", attachDigestFormat},
		{"SizeLimits", attachSizeLimits},
		{"MediaTypes", attachMediaTypes},
		{"AudiencePreserved", attachAudience},
		{"LocatorNeverPublic", attachLocatorPrivate},
		{"Replay", attachReplay},
		{"FailedCommitCleansUp", attachFailedCommit},
		{"AmbiguousCommitReconciles", attachAmbiguousCommit},
		{"FailedStagingLeavesNothing", attachFailedStaging},
		{"IntentBoundOnCommit", attachIntentBound},
		{"CrashBeforeCommitIsSwept", attachCrashSwept},
		{"SweepRetriesFailedDelete", attachSweepRetries},
		{"SweptStagingFailsTheUpload", attachSweptStaging},
		{"Cancellation", attachCancellation},
		{"ExpectedRevision", attachExpectedRevision},
		{"UploadDenied", attachUploadDenied},
		{"OpenVerifiesBytes", attachOpenVerifies},
		{"ReferenceScope", attachScope},
		{"Redaction", attachRedaction},
		{"Export", attachExport},
		{"Cascade", attachCascade},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newAttachEnv(t, factory, blobs, true)) })
	}
	t.Run("NoBlobStore", func(t *testing.T) { attachNoBlobs(t, newAttachEnv(t, factory, blobs, false)) })
}

// attachLimit is the suite's configured attachment size limit.
const attachLimit = 1024

// trackedBlobs records which staged keys are still held and every key
// Stage was asked for, and can fail staging or deletion, corrupt Open and
// run hooks around Stage.
type trackedBlobs struct {
	inner ports.BlobStore

	mu        sync.Mutex
	live      map[ports.BlobKey]bool
	staged    []ports.BlobKey
	openErr   error
	corrupt   bool
	stageErr  error
	deleteErr error
	// before runs ahead of every Stage, after staged records the key.
	before func(ports.BlobKey)
	// after runs once a Stage succeeded.
	after func(ports.BlobKey)
}

func (b *trackedBlobs) Stage(ctx context.Context, k ports.BlobKey, r io.Reader, size int64) error {
	b.mu.Lock()
	b.staged = append(b.staged, k)
	stageErr, before, after := b.stageErr, b.before, b.after
	b.mu.Unlock()
	if before != nil {
		before(k)
	}
	if stageErr != nil {
		_, _ = io.Copy(io.Discard, r)
		return stageErr
	}
	err := b.inner.Stage(ctx, k, r, size)
	b.mu.Lock()
	if err == nil {
		b.live[k] = true
	} else {
		delete(b.live, k)
	}
	b.mu.Unlock()
	if err == nil && after != nil {
		after(k)
	}
	return err
}

func (b *trackedBlobs) Open(ctx context.Context, k ports.BlobKey) (io.ReadCloser, error) {
	b.mu.Lock()
	openErr, corrupt := b.openErr, b.corrupt
	b.mu.Unlock()
	if openErr != nil {
		return nil, openErr
	}
	rc, err := b.inner.Open(ctx, k)
	if err != nil || !corrupt {
		return rc, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		data[0] ^= 0xff
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *trackedBlobs) Delete(ctx context.Context, k ports.BlobKey) error {
	b.mu.Lock()
	deleteErr := b.deleteErr
	b.mu.Unlock()
	if deleteErr != nil {
		return deleteErr
	}
	err := b.inner.Delete(ctx, k)
	if err == nil {
		b.mu.Lock()
		delete(b.live, k)
		b.mu.Unlock()
	}
	return err
}

func (b *trackedBlobs) held() []ports.BlobKey {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []ports.BlobKey
	for k := range b.live {
		out = append(out, k)
	}
	return out
}

// keys returns every key Stage was asked for, in call order.
func (b *trackedBlobs) keys() []ports.BlobKey {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.staged)
}

func (b *trackedBlobs) set(fn func(*trackedBlobs)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(b)
}

// commitFault selects how faultStore fails the next Transact.
type commitFault int

const (
	faultNone commitFault = iota
	// faultRollback fails the unit after its callback succeeded: nothing
	// commits.
	faultRollback
	// faultAmbiguous commits the unit, then reports an unknown outcome.
	faultAmbiguous
)

// faultStore injects one commit failure into the next unit that claims an
// operation: the upload's commit, never the unit recording its staging
// intent.
type faultStore struct {
	store.Store
	mu   sync.Mutex
	next commitFault
}

func (s *faultStore) fail(f commitFault) {
	s.mu.Lock()
	s.next = f
	s.mu.Unlock()
}

// claimTx reports whether its unit claimed an operation.
type claimTx struct {
	store.Tx
	claimed bool
}

func (tx *claimTx) ClaimOperation(ctx context.Context, rec store.OperationRecord) (store.OperationRecord, bool, error) {
	tx.claimed = true
	return tx.Tx.ClaimOperation(ctx, rec)
}

func (s *faultStore) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	var f commitFault
	err := s.Store.Transact(ctx, tenant, func(tx store.Tx) error {
		ct := &claimTx{Tx: tx}
		if err := fn(ct); err != nil {
			return err
		}
		if ct.claimed {
			s.mu.Lock()
			f, s.next = s.next, faultNone
			s.mu.Unlock()
		}
		if f == faultRollback {
			return model.NewError(model.Unavailable, "injected commit failure")
		}
		return nil
	})
	if err == nil && f == faultAmbiguous {
		return model.NewError(model.OutcomeUnknown, "injected unknown commit outcome")
	}
	return err
}

// attachAuthorizer allows everything except reading a non-public
// attachment unless staff, and uploads while denyUpload. It records every
// attachment authorization.
type attachAuthorizer struct {
	mu         sync.Mutex
	staff      bool
	denyUpload bool
	seen       []policy.Authorization
}

func (a *attachAuthorizer) Authorize(_ context.Context, r policy.Authorization) (policy.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.HasPrefix(r.Action, "attachment.") {
		a.seen = append(a.seen, r)
	}
	switch {
	case r.Action == model.ActionAttachmentRead && r.Audience.Visibility != model.Public && !a.staff:
		return policy.Decision{}, nil
	case r.Action == model.ActionAttachmentUpload && a.denyUpload:
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true}, nil
}

func (a *attachAuthorizer) set(fn func(*attachAuthorizer)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(a)
}

func (a *attachAuthorizer) actions(action string) []policy.Authorization {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []policy.Authorization
	for _, r := range a.seen {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

type attachEnv struct {
	*serviceEnv
	fault  *faultStore
	blobs  *trackedBlobs
	policy *attachAuthorizer
}

// attachHorizon is every retention horizon of the attachment suite's
// engine, the staging horizon included.
const attachHorizon = time.Hour

func newAttachEnv(t *testing.T, factory StoreFactory, blobs BlobFactory, withBlobs bool) *attachEnv {
	t.Helper()
	fx := factory(t)
	fault := &faultStore{Store: fx.Store}
	tracked := &trackedBlobs{inner: blobs(t), live: map[ports.BlobKey]bool{}}
	pol := &attachAuthorizer{}
	clock := portstest.NewFixedClock(fx.Now())
	cfg := uma.Config{
		Store: fault, Authorizer: pol, Clock: clock, IDs: portstest.NewSequenceIDs(0xa77),
		Adapters:             []adapter.Factory{serviceAdapter{}},
		AttachmentSizeLimit:  attachLimit,
		AttachmentMediaTypes: []string{"text/plain", "image/*"},
		Retention: uma.RetentionPolicy{
			Events: attachHorizon, Operations: attachHorizon, Ingress: attachHorizon,
			Attempts: attachHorizon, Staging: attachHorizon, Exports: attachHorizon,
			Contacts: attachHorizon,
		},
	}
	if withBlobs {
		cfg.Blobs = tracked
	}
	e, err := uma.New(cfg)
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	client, err := e.For(model.Principal{Tenant: serviceTenant, Actor: "staff-1"})
	if err != nil {
		t.Fatal(err)
	}
	env := &attachEnv{
		serviceEnv: &serviceEnv{
			t: t, ctx: t.Context(), fx: fx, clock: clock, engine: e, st: fx.Store,
			ids: portstest.NewSequenceIDs(0xa76), client: client,
		},
		fault: fault, blobs: tracked, policy: pol,
	}
	// Every Stage must find its key's intent recorded, and not abandoned,
	// before any byte is written.
	tracked.before = func(k ports.BlobKey) {
		in, err := env.intent(k)
		switch {
		case err != nil:
			t.Errorf("Stage(%+v) before its staging intent was recorded: %v", k, err)
		case in.Abandoned:
			t.Errorf("Stage(%+v) under an abandoned intent", k)
		}
	}
	env.ch = env.configureChannel()
	env.inbox = env.configureInbox("files")
	env.inbox2 = env.configureInbox("other")
	return env
}

// intent reads the staging intent recorded for k.
func (env *attachEnv) intent(k ports.BlobKey) (store.StagingIntent, error) {
	var in store.StagingIntent
	err := env.st.View(context.WithoutCancel(env.ctx), serviceTenant, func(r store.Reader) (err error) {
		in, err = r.Staging(env.ctx, k)
		return err
	})
	return in, err
}

// requireIntents fails unless exactly n of the keys Stage was asked for
// still have a staging intent.
func (env *attachEnv) requireIntents(n int) {
	env.t.Helper()
	got := 0
	for _, k := range env.blobs.keys() {
		_, err := env.intent(k)
		switch {
		case err == nil:
			got++
		case !model.IsCode(err, model.NotFound):
			env.t.Fatalf("read staging intent: %v", err)
		}
	}
	if got != n {
		env.t.Fatalf("%d staging intents left, want %d", got, n)
	}
}

// purge runs Runtime.Purge for the suite's tenant.
func (env *attachEnv) purge() (uma.PurgeReport, error) {
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "staff-1"}, Call: model.Call{Correlation: "purge"}}
	return env.engine.Runtime().Purge(env.ctx, call)
}

// advance moves the store and engine clocks forward together.
func (env *attachEnv) advance(d time.Duration) {
	env.fx.Advance(d)
	env.clock.Advance(d)
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func publicMeta(name string, data []byte) model.AttachmentMetadata {
	return model.AttachmentMetadata{
		Name: name, MediaType: "text/plain; charset=utf-8", Size: int64(len(data)),
		Audience: model.Audience{Visibility: model.Public}, Digest: digestOf(data),
	}
}

func (env *attachEnv) prepare(meta model.AttachmentMetadata) (model.AttachmentRef, error) {
	env.t.Helper()
	a, err := env.client.Attachments().Prepare(env.ctx, model.PrepareAttachment{Command: env.cmd(""), Inbox: env.inbox, Metadata: meta})
	if err != nil {
		return model.AttachmentRef{}, err
	}
	if a.Operation.State != model.Committed || a.Operation.Result == nil {
		env.t.Fatalf("Prepare acceptance = %+v", a)
	}
	ref, err := a.Operation.Result.AsAttachment()
	if err != nil {
		env.t.Fatal(err)
	}
	if !ref.In(env.inbox) {
		env.t.Fatalf("attachment %+v outside inbox %+v", ref, env.inbox)
	}
	return ref, nil
}

func (env *attachEnv) mustPrepare(meta model.AttachmentMetadata) model.AttachmentRef {
	env.t.Helper()
	ref, err := env.prepare(meta)
	if err != nil {
		env.t.Fatalf("Prepare: %v", err)
	}
	return ref
}

func (env *attachEnv) upload(ref model.AttachmentRef, cmd model.Command, r io.Reader) (model.Acceptance, error) {
	return env.client.Attachments().Upload(env.ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "c"}, Ref: ref}, cmd, r)
}

func (env *attachEnv) open(ref model.AttachmentRef) (model.Attachment, []byte, error) {
	att, rc, err := env.client.Attachments().Open(env.ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
	if err != nil {
		if rc != nil {
			env.t.Fatal("Open returned a reader with an error")
		}
		return att, nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	return att, data, err
}

func (env *attachEnv) record(ref model.AttachmentRef) store.AttachmentRecord {
	env.t.Helper()
	var rec store.AttachmentRecord
	if err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) (err error) {
		rec, err = r.Attachment(env.ctx, ref)
		return err
	}); err != nil {
		env.t.Fatalf("read attachment record: %v", err)
	}
	return rec
}

func (env *attachEnv) requireHeld(n int) {
	env.t.Helper()
	if got := env.blobs.held(); len(got) != n {
		env.t.Fatalf("blob store holds %d staged blobs, want %d", len(got), n)
	}
}

func requireCodeOf(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	var me *model.Error
	if !errors.As(err, &me) || me.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func attachRoundTrip(t *testing.T, env *attachEnv) {
	data := []byte("hello world")
	meta := publicMeta("hello.txt", data)
	ref := env.mustPrepare(meta)
	prepared := env.record(ref)
	if prepared.Blob != nil {
		t.Fatalf("prepared record holds a blob: %+v", prepared.Blob)
	}
	if _, _, err := env.open(ref); err == nil {
		t.Fatal("Open before upload succeeded")
	} else {
		requireCodeOf(t, err, model.Conflict)
	}
	a, err := env.upload(ref, env.cmd(""), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if a.Replayed || a.Operation.State != model.Committed || a.Operation.Result == nil || *a.Operation.Result != ref.Ref() {
		t.Fatalf("Upload acceptance = %+v", a)
	}
	env.requireHeld(1)
	att, got, err := env.open(ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("Open read %q, want %q", got, data)
	}
	if att.Ref != ref || att.Metadata != meta || att.Revision == "" || att.Revision == prepared.Attachment.Revision {
		t.Fatalf("Open attachment = %+v, want metadata %+v and a new revision", att, meta)
	}
	rec := env.record(ref)
	if rec.Blob == nil || rec.Blob.Tenant != serviceTenant || rec.StagingOperation != a.Operation.Ref {
		t.Fatalf("record = %+v, want the staged key and the upload operation", rec)
	}

	// Without a declared digest or size the stored metadata records the
	// computed ones.
	meta2 := publicMeta("bare.txt", nil)
	meta2.Digest, meta2.Size = "", 0
	ref2 := env.mustPrepare(meta2)
	if _, err := env.upload(ref2, env.cmd(""), bytes.NewReader(data)); err != nil {
		t.Fatalf("Upload without declared digest: %v", err)
	}
	att2, _, err := env.open(ref2)
	if err != nil {
		t.Fatal(err)
	}
	if att2.Metadata.Digest != digestOf(data) || att2.Metadata.Size != int64(len(data)) {
		t.Fatalf("stored metadata = %+v, want computed digest and size", att2.Metadata)
	}
}

func attachDigestVerified(t *testing.T, env *attachEnv) {
	data := []byte("the real bytes")
	meta := publicMeta("d.txt", data)
	meta.Digest = digestOf([]byte("other bytes"))
	meta.Size = 0
	ref := env.mustPrepare(meta)
	_, err := env.upload(ref, env.cmd(""), bytes.NewReader(data))
	requireCodeOf(t, err, model.Validation)
	env.requireHeld(0)
	if rec := env.record(ref); rec.Blob != nil || rec.Attachment.Metadata.Digest != meta.Digest {
		t.Fatalf("record after a digest mismatch = %+v", rec)
	}
	if _, err := env.upload(ref, env.cmd(""), strings.NewReader("other bytes")); err != nil {
		t.Fatalf("Upload of the declared bytes: %v", err)
	}
	env.requireHeld(1)
}

func attachDigestFormat(t *testing.T, env *attachEnv) {
	data := []byte("x")
	sum := sha256.Sum256(data)
	for _, d := range []string{
		"md5:" + hex.EncodeToString(sum[:16]),
		"sha256:" + strings.ToUpper(hex.EncodeToString(sum[:])),
		"sha256:" + hex.EncodeToString(sum[:31]),
		hex.EncodeToString(sum[:]),
		"sha256:",
	} {
		meta := publicMeta("f.txt", data)
		meta.Digest = d
		_, err := env.prepare(meta)
		requireCodeOf(t, err, model.Validation)
	}
}

func attachSizeLimits(t *testing.T, env *attachEnv) {
	big := publicMeta("big.txt", nil)
	big.Digest, big.Size = "", attachLimit+1
	_, err := env.prepare(big)
	requireCodeOf(t, err, model.Validation)

	undeclared := publicMeta("u.txt", nil)
	undeclared.Digest, undeclared.Size = "", 0
	ref := env.mustPrepare(undeclared)
	_, err = env.upload(ref, env.cmd(""), bytes.NewReader(make([]byte, attachLimit+1)))
	requireCodeOf(t, err, model.Validation)
	env.requireHeld(0)

	declared := publicMeta("five.txt", nil)
	declared.Digest, declared.Size = "", 5
	ref5 := env.mustPrepare(declared)
	for _, body := range []string{"123456", "1234"} {
		_, err := env.upload(ref5, env.cmd(""), strings.NewReader(body))
		requireCodeOf(t, err, model.Validation)
		env.requireHeld(0)
	}
	if _, err := env.upload(ref, env.cmd(""), bytes.NewReader(make([]byte, attachLimit))); err != nil {
		t.Fatalf("Upload at the limit: %v", err)
	}
	env.requireHeld(1)
}

func attachMediaTypes(t *testing.T, env *attachEnv) {
	for _, tc := range []struct {
		mediaType string
		ok        bool
	}{
		{"text/plain", true},
		{"Text/Plain; charset=utf-8", true},
		{"image/png", true},
		{"application/zip", false},
		{"text/html", false},
		{"", false},
		{"not a type", false},
	} {
		meta := publicMeta("m", nil)
		meta.MediaType, meta.Digest, meta.Size = tc.mediaType, "", 0
		_, err := env.prepare(meta)
		if tc.ok && err != nil {
			t.Errorf("Prepare %q: %v", tc.mediaType, err)
		}
		if !tc.ok && !model.IsCode(err, model.Validation) {
			t.Errorf("Prepare %q = %v, want validation", tc.mediaType, err)
		}
	}
}

func attachAudience(t *testing.T, env *attachEnv) {
	data := []byte("staff only")
	meta := publicMeta("note.txt", data)
	meta.Audience = model.Audience{Visibility: model.Internal, PolicyRef: "staff"}
	ref := env.mustPrepare(meta)
	if _, err := env.upload(ref, env.cmd(""), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	_, _, err := env.open(ref)
	requireCodeOf(t, err, model.Unauthorized)
	env.policy.set(func(a *attachAuthorizer) { a.staff = true })
	att, got, err := env.open(ref)
	if err != nil {
		t.Fatalf("staff Open: %v", err)
	}
	if att.Metadata.Audience != meta.Audience || string(got) != string(data) {
		t.Fatalf("Open = %+v %q, want audience %+v", att, got, meta.Audience)
	}
	for _, action := range []string{model.ActionAttachmentPrepare, model.ActionAttachmentUpload, model.ActionAttachmentRead} {
		seen := env.policy.actions(action)
		if len(seen) == 0 {
			t.Fatalf("no %s authorization", action)
		}
		for _, r := range seen {
			if r.Audience != meta.Audience {
				t.Errorf("%s authorized under %+v, want %+v", action, r.Audience, meta.Audience)
			}
		}
	}
	// Every Open reauthorizes.
	n := len(env.policy.actions(model.ActionAttachmentRead))
	env.policy.set(func(a *attachAuthorizer) { a.staff = false })
	_, _, err = env.open(ref)
	requireCodeOf(t, err, model.Unauthorized)
	if len(env.policy.actions(model.ActionAttachmentRead)) != n+1 {
		t.Fatal("Open did not reauthorize")
	}
}

func attachLocatorPrivate(t *testing.T, env *attachEnv) {
	data := []byte("locator check")
	meta := publicMeta("l.txt", data)
	ref := env.mustPrepare(meta)
	cmd := env.cmd("")
	up, err := env.upload(ref, cmd, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	held := env.blobs.held()
	if len(held) != 1 {
		t.Fatalf("held = %v", held)
	}
	key := held[0]
	if rec := env.record(ref); rec.Blob == nil || *rec.Blob != key {
		t.Fatalf("record locator = %+v, want %+v", rec.Blob, key)
	}
	att, _, err := env.open(ref)
	if err != nil {
		t.Fatal(err)
	}
	op, err := env.client.Operations().Get(env.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c"}, Ref: up.Operation.Ref})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := env.upload(ref, cmd, bytes.NewReader(data))
	if err != nil || !replay.Replayed {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	for name, v := range map[string]any{"upload": up, "attachment": att, "operation": op, "replay": replay} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(key.ID)) {
			t.Errorf("%s result carries the blob locator: %s", name, b)
		}
	}
	env.blobs.set(func(b *trackedBlobs) {
		b.openErr = &model.Error{Code: model.Unavailable, SafeMessage: "bucket key " + string(key.ID) + " unreachable"}
	})
	_, _, err = env.open(ref)
	if err == nil {
		t.Fatal("Open succeeded over a failing blob store")
	}
	if strings.Contains(err.Error(), string(key.ID)) {
		t.Fatalf("Open error %q carries the blob locator", err)
	}
	requireCodeOf(t, err, model.Unavailable)
	env.blobs.set(func(b *trackedBlobs) { b.openErr = fmt.Errorf("open /data/%s: no such file", key.ID) })
	_, _, err = env.open(ref)
	if err == nil || strings.Contains(err.Error(), string(key.ID)) {
		t.Fatalf("Open error %v carries the blob locator", err)
	}
}

func attachReplay(t *testing.T, env *attachEnv) {
	data := []byte("replayed bytes")
	meta := publicMeta("r.txt", data)
	meta.Digest = ""
	ref := env.mustPrepare(meta)
	cmd := env.cmd("")
	first, err := env.upload(ref, cmd, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	again, err := env.upload(ref, cmd, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("replay = %+v, want the stored operation replayed", again)
	}
	env.requireHeld(1)
	// Same operation, different bytes: the bytes are compared even though
	// no digest was declared.
	other := []byte("different byte")
	_, err = env.upload(ref, cmd, bytes.NewReader(other))
	requireCodeOf(t, err, model.Conflict)
	env.requireHeld(1)
	// A new operation on an uploaded attachment conflicts.
	_, err = env.upload(ref, env.cmd(""), bytes.NewReader(data))
	requireCodeOf(t, err, model.Conflict)
	env.requireHeld(1)
	if _, got, err := env.open(ref); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Open after replays = %q, %v", got, err)
	}
}

func attachFailedCommit(t *testing.T, env *attachEnv) {
	data := []byte("rolled back")
	ref := env.mustPrepare(publicMeta("f.txt", data))
	cmd := env.cmd("")
	env.fault.fail(faultRollback)
	if _, err := env.upload(ref, cmd, bytes.NewReader(data)); err == nil {
		t.Fatal("Upload succeeded over a failed commit")
	}
	env.requireHeld(0)
	env.requireIntents(0)
	if rec := env.record(ref); rec.Blob != nil {
		t.Fatalf("record after a failed commit = %+v", rec)
	}
	a, err := env.upload(ref, cmd, bytes.NewReader(data))
	if err != nil || a.Replayed {
		t.Fatalf("retry after a failed commit = %+v, %v", a, err)
	}
	env.requireHeld(1)
}

func attachAmbiguousCommit(t *testing.T, env *attachEnv) {
	data := []byte("committed after all")
	ref := env.mustPrepare(publicMeta("a.txt", data))
	cmd := env.cmd("")
	env.fault.fail(faultAmbiguous)
	if _, err := env.upload(ref, cmd, bytes.NewReader(data)); err == nil {
		t.Fatal("Upload reported success over an unknown commit outcome")
	}
	env.requireHeld(1)
	env.requireIntents(0)
	held := env.blobs.held()
	if rec := env.record(ref); rec.Blob == nil || *rec.Blob != held[0] {
		t.Fatalf("record = %+v, want it to keep the committed blob %+v", rec, held[0])
	}
	a, err := env.upload(ref, cmd, bytes.NewReader(data))
	if err != nil || !a.Replayed {
		t.Fatalf("retry = %+v, %v; want a replay", a, err)
	}
	env.requireHeld(1)
	if _, got, err := env.open(ref); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func attachFailedStaging(t *testing.T, env *attachEnv) {
	data := []byte("never staged")
	ref := env.mustPrepare(publicMeta("s.txt", data))
	_, err := env.upload(ref, env.cmd(""), &failingReader{n: 4})
	if err == nil {
		t.Fatal("Upload succeeded over a failing reader")
	}
	env.requireHeld(0)
	env.blobs.set(func(b *trackedBlobs) { b.stageErr = errors.New("disk full at /var/blobs") })
	_, err = env.upload(ref, env.cmd(""), bytes.NewReader(data))
	requireCodeOf(t, err, model.Unavailable)
	if strings.Contains(err.Error(), "/var/blobs") {
		t.Fatalf("Upload error %q carries blob store detail", err)
	}
	env.blobs.set(func(b *trackedBlobs) { b.stageErr = nil })
	env.requireIntents(0)
	if rec := env.record(ref); rec.Blob != nil {
		t.Fatalf("record after failed staging = %+v", rec)
	}
	if _, err := env.upload(ref, env.cmd(""), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	env.requireHeld(1)
}

// attachIntentBound: an upload stages under a fresh key in the tenant,
// recorded before Stage, and the commit binds it in the record and clears
// the intent.
func attachIntentBound(t *testing.T, env *attachEnv) {
	data := []byte("bound")
	ref := env.mustPrepare(publicMeta("b.txt", data))
	if _, err := env.upload(ref, env.cmd(""), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	keys := env.blobs.keys()
	if len(keys) != 1 || keys[0].Tenant != serviceTenant || keys[0].ID.Validate() != nil {
		t.Fatalf("staged keys = %+v, want one canonical key in the tenant", keys)
	}
	if rec := env.record(ref); rec.Blob == nil || *rec.Blob != keys[0] {
		t.Fatalf("record = %+v, want it bound to %+v", rec, keys[0])
	}
	env.requireIntents(0)
	other := env.mustPrepare(publicMeta("b2.txt", data))
	if _, err := env.upload(other, env.cmd(""), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if keys := env.blobs.keys(); len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("staged keys = %+v, want a fresh key per upload", keys)
	}
}

// crashUpload uploads data to ref and dies once the bytes are staged, as a
// process crashing between Stage and the commit: no commit, no cleanup.
func (env *attachEnv) crashUpload(ref model.AttachmentRef, data []byte) {
	env.t.Helper()
	env.blobs.set(func(b *trackedBlobs) { b.after = func(ports.BlobKey) { runtime.Goexit() } })
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := env.upload(ref, env.cmd(""), bytes.NewReader(data))
		env.t.Errorf("Upload returned %v after the crash", err)
	}()
	<-done
	env.blobs.set(func(b *trackedBlobs) { b.after = nil })
}

// attachCrashSwept: a blob a crash left between Stage and the commit is
// kept while its intent is younger than the staging horizon, then deleted
// by Runtime.Purge; bound blobs are never touched, and the attachment can
// be uploaded again.
func attachCrashSwept(t *testing.T, env *attachEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	kept := []byte("bound and kept")
	keptRef := env.mustPrepare(publicMeta("k.txt", kept))
	if _, err := env.upload(keptRef, env.cmd(""), bytes.NewReader(kept)); err != nil {
		t.Fatal(err)
	}
	data := []byte("crashed before commit")
	ref := env.mustPrepare(publicMeta("c.txt", data))
	env.crashUpload(ref, data)
	env.requireHeld(2)
	env.requireIntents(1)

	env.advance(attachHorizon / 2)
	if rep, err := env.purge(); err != nil || rep.Blobs != 0 {
		t.Fatalf("purge inside the staging horizon = %+v, %v; want nothing swept", rep, err)
	}
	env.requireHeld(2)
	env.requireIntents(1)

	env.advance(attachHorizon)
	rep, err := env.purge()
	if err != nil || rep.Blobs != 1 {
		t.Fatalf("purge past the staging horizon = %+v, %v; want the orphan swept", rep, err)
	}
	env.requireHeld(1)
	env.requireIntents(0)
	if _, got, err := env.open(keptRef); err != nil || !bytes.Equal(got, kept) {
		t.Fatalf("bound attachment after the sweep = %q, %v", got, err)
	}
	if rec := env.record(ref); rec.State != "prepared" || rec.Blob != nil {
		t.Fatalf("crashed attachment = %+v, want it still prepared", rec)
	}
	if _, err := env.upload(ref, env.cmd(""), bytes.NewReader(data)); err != nil {
		t.Fatalf("upload after the sweep: %v", err)
	}
	env.requireHeld(2)
	env.advance(2 * attachHorizon)
	if rep, err := env.purge(); err != nil || rep.Blobs != 0 {
		t.Fatalf("later purge = %+v, %v; want bound blobs kept", rep, err)
	}
	env.requireHeld(2)
}

// attachSweepRetriesFailedDelete: a sweep whose blob delete fails stops
// with a safe error and keeps the intent, and the next purge deletes it.
func attachSweepRetries(t *testing.T, env *attachEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	data := []byte("orphan")
	ref := env.mustPrepare(publicMeta("o.txt", data))
	env.crashUpload(ref, data)
	env.advance(2 * attachHorizon)
	env.blobs.set(func(b *trackedBlobs) { b.deleteErr = errors.New("delete /var/blobs/x: denied") })
	_, err := env.purge()
	if err == nil || strings.Contains(err.Error(), "/var/blobs") {
		t.Fatalf("purge over a failing delete = %v, want a safe error", err)
	}
	env.requireHeld(1)
	env.requireIntents(1)
	env.blobs.set(func(b *trackedBlobs) { b.deleteErr = nil })
	if rep, err := env.purge(); err != nil || rep.Blobs != 1 {
		t.Fatalf("retried purge = %+v, %v; want the orphan swept", rep, err)
	}
	env.requireHeld(0)
	env.requireIntents(0)
}

// attachSweptStaging: an upload whose staging outlived the horizon, so the
// sweep abandoned its intent, fails at the commit and keeps nothing.
func attachSweptStaging(t *testing.T, env *attachEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	data := []byte("too slow")
	ref := env.mustPrepare(publicMeta("slow.txt", data))
	var once sync.Once
	check := env.blobs.before
	env.blobs.set(func(b *trackedBlobs) {
		b.before = func(k ports.BlobKey) {
			check(k)
			once.Do(func() {
				env.advance(2 * attachHorizon)
				if _, err := env.purge(); err != nil {
					t.Errorf("purge during staging: %v", err)
				}
			})
		}
	})
	_, err := env.upload(ref, env.cmd(""), bytes.NewReader(data))
	requireCodeOf(t, err, model.Conflict)
	env.requireHeld(0)
	env.requireIntents(0)
	if rec := env.record(ref); rec.State != "prepared" || rec.Blob != nil {
		t.Fatalf("record after a swept staging = %+v", rec)
	}
	if _, err := env.upload(ref, env.cmd(""), bytes.NewReader(data)); err != nil {
		t.Fatalf("retry after a swept staging: %v", err)
	}
	env.requireHeld(1)
}

// cancelReader cancels its context after the first read.
type cancelReader struct {
	r      io.Reader
	cancel context.CancelFunc
	reads  int
}

func (c *cancelReader) Read(p []byte) (int, error) {
	c.reads++
	if c.reads == 2 {
		c.cancel()
	}
	return c.r.Read(p[:min(len(p), 8)])
}

func attachCancellation(t *testing.T, env *attachEnv) {
	data := bytes.Repeat([]byte("c"), 512)
	meta := publicMeta("c.txt", data)
	ref := env.mustPrepare(meta)
	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	_, err := env.client.Attachments().Upload(ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "c"}, Ref: ref},
		env.cmd(""), &cancelReader{r: bytes.NewReader(data), cancel: cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload = %v, want context.Canceled", err)
	}
	env.requireHeld(0)
	if rec := env.record(ref); rec.Blob != nil {
		t.Fatalf("record after cancellation = %+v", rec)
	}
}

func attachExpectedRevision(t *testing.T, env *attachEnv) {
	data := []byte("rev")
	ref := env.mustPrepare(publicMeta("rev.txt", data))
	cmd := env.cmd("stale-revision")
	_, err := env.upload(ref, cmd, bytes.NewReader(data))
	requireCodeOf(t, err, model.Conflict)
	env.requireHeld(0)
	cur := env.record(ref).Attachment.Revision
	if _, err := env.upload(ref, env.cmd(cur), bytes.NewReader(data)); err != nil {
		t.Fatalf("Upload at the current revision: %v", err)
	}
}

func attachUploadDenied(t *testing.T, env *attachEnv) {
	data := []byte("denied")
	ref := env.mustPrepare(publicMeta("deny.txt", data))
	env.policy.set(func(a *attachAuthorizer) { a.denyUpload = true })
	r := &countingReader{r: bytes.NewReader(data)}
	_, err := env.upload(ref, env.cmd(""), r)
	requireCodeOf(t, err, model.Unauthorized)
	if r.n != 0 {
		t.Fatalf("a denied upload read %d bytes", r.n)
	}
	env.requireHeld(0)
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func attachOpenVerifies(t *testing.T, env *attachEnv) {
	data := []byte("verified on read")
	ref := env.mustPrepare(publicMeta("v.txt", data))
	if _, err := env.upload(ref, env.cmd(""), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	env.blobs.set(func(b *trackedBlobs) { b.corrupt = true })
	_, got, err := env.open(ref)
	if err == nil {
		t.Fatalf("Open read corrupted bytes %q without error", got)
	}
	if model.IsCode(err, model.Validation) || !model.IsCode(err, model.Permanent) {
		t.Fatalf("Open of corrupted bytes = %v, want permanent", err)
	}
}

func attachScope(t *testing.T, env *attachEnv) {
	data := []byte("scoped")
	ref := env.mustPrepare(publicMeta("scope.txt", data))
	if _, err := env.upload(ref, env.cmd(""), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	foreign := ref
	foreign.Inbox.Tenant = "conformance-other"
	wrongInbox := ref
	wrongInbox.Inbox = env.inbox2
	unknown := ref
	unknown.ID = env.id()
	for name, r := range map[string]model.AttachmentRef{"foreign": foreign, "wrong inbox": wrongInbox, "unknown": unknown} {
		_, _, err := env.open(r)
		if !model.IsCode(err, model.NotFound) {
			t.Errorf("Open %s = %v, want not_found", name, err)
		}
		_, err = env.upload(r, env.cmd(""), bytes.NewReader(data))
		if !model.IsCode(err, model.NotFound) {
			t.Errorf("Upload %s = %v, want not_found", name, err)
		}
	}
	other := publicMeta("o.txt", data)
	_, err := env.client.Attachments().Prepare(env.ctx, model.PrepareAttachment{
		Command: env.cmd(""), Inbox: model.InboxRef{Tenant: "conformance-other", ID: env.inbox.ID}, Metadata: other,
	})
	requireCodeOf(t, err, model.NotFound)
	_, err = env.client.Attachments().Prepare(env.ctx, model.PrepareAttachment{
		Command: env.cmd(""), Inbox: model.InboxRef{Tenant: serviceTenant, ID: env.id()}, Metadata: other,
	})
	requireCodeOf(t, err, model.NotFound)
	env.requireHeld(1)
}

func attachNoBlobs(t *testing.T, env *attachEnv) {
	data := []byte("nowhere")
	_, err := env.prepare(publicMeta("n.txt", data))
	requireCodeOf(t, err, model.Disabled)
	ref := model.AttachmentRef{Inbox: env.inbox, ID: env.id()}
	_, err = env.upload(ref, env.cmd(""), bytes.NewReader(data))
	requireCodeOf(t, err, model.Disabled)
	_, _, err = env.open(ref)
	requireCodeOf(t, err, model.Disabled)
	if len(env.blobs.held()) != 0 {
		t.Fatal("blob store touched without being configured")
	}
}
