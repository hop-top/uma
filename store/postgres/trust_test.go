package postgres_test

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/postgres"
)

// Trust counters on PostgreSQL: concurrent writers of one bucket converge
// (serializable units lost to a winner are rerun on its state) and a
// decision moves its counter once, never for a replay.
func TestTrustCountersOnPostgres(t *testing.T) {
	pool := postgres.NewTestDatabase(t, true)
	st, err := postgres.Open(t.Context(), pool, postgres.Options{Clock: portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())})
	if err != nil {
		t.Fatal(err)
	}
	r := enginetest.NewRig(t, func(c *engine.Config, _ *enginetest.OutFactory) { c.Store = st })
	s := r.C.Operations()

	// Keys that share a bucket contend on one record.
	var keys []string
	for i := 0; len(keys) < 4; i++ {
		k := fmt.Sprintf("key-%d", i)
		if bucketOf(k) == bucketOf("key-0") {
			keys = append(keys, k)
		}
	}
	const per = 6
	var wg sync.WaitGroup
	errs := make(chan error, len(keys)*per*2)
	for _, k := range keys {
		for i := range per {
			for range 2 { // each batch twice: a replay of a batch counts nothing
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, k, nil, fmt.Sprintf("b%d", i))
					errs <- err
				}()
			}
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent approval: %v", err)
		}
	}
	for _, k := range keys {
		e, err := s.Trust(r.Ctx, r.Ch.Ref, k)
		if err != nil || e.Clean != per || e.Approvals != per {
			t.Fatalf("%s = %+v, %v; want %d clean approvals", k, e, err, per)
		}
	}
	decisionsOnPostgres(t, r)
	if err := s.ResetTrustSet(r.Ctx, r.Ch.Ref, keys); err != nil {
		t.Fatal(err)
	}
	all, err := s.Trusts(r.Ctx, r.Ch.Ref, model.TrustFilter{})
	if err != nil || len(all) != len(keys)+1 {
		t.Fatalf("Trusts = %d, %v", len(all), err)
	}
	for _, e := range all {
		if e.Key == "decided/key" {
			continue
		}
		if e.Clean != 0 || e.Approvals != per {
			t.Fatalf("after reset %+v", e)
		}
	}
	importOnPostgres(t, r, "import/key")
}

// bucketOf is the persisted bucket of a trust key: the first byte of its
// SHA-256 modulo 64.
func bucketOf(key string) int { sum := sha256.Sum256([]byte(key)); return int(sum[0]) % 64 }

func decisionsOnPostgres(t *testing.T, r *enginetest.Rig) {
	t.Helper()
	plugin, err := enginetest.For(r.E, model.Principal{Tenant: enginetest.TenantA, Actor: "plugin:triage"})
	if err != nil {
		t.Fatal(err)
	}
	conv := r.Conv.Ref.Ref()
	hold := func(id model.OperationID) model.Operation {
		a, err := operations.Submit(r.Ctx, plugin.Client, operations.Submission{
			Kind: model.ActionMessageReply, Target: conv, Owner: conv,
			Command:     model.Command{Call: model.Call{Correlation: "c"}, Operation: id},
			Fingerprint: []byte("fp-" + string(id)),
			Intent:      model.Extension{Name: "test.intent", Version: 1, Data: []byte(`{}`)},
			Hold:        &operations.Hold{Reason: "r", TrustKey: "decided/key", TrustLabels: map[string]string{"k": "v"}},
		})
		if err != nil {
			t.Fatalf("hold: %v", err)
		}
		return a.Operation
	}
	decide := func(op model.Operation) model.DecideHeld {
		return model.DecideHeld{Call: model.Call{Correlation: "d"}, Operation: op.Ref, ExpectedRevision: op.Revision}
	}
	s := r.C.Operations()
	a := hold("p-1")
	if _, err := s.Approve(r.Ctx, decide(a)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Approve(r.Ctx, decide(a)); err != nil || !got.Replayed {
		t.Fatalf("replay = %+v, %v", got, err)
	}
	e, err := s.Trust(r.Ctx, r.Ch.Ref, "decided/key")
	if err != nil || e.Clean != 1 || e.Approvals != 1 || e.Labels["k"] != "v" {
		t.Fatalf("after approve and replay = %+v, %v", e, err)
	}
	b := hold("p-2")
	if _, err := s.Reject(r.Ctx, decide(b)); err != nil {
		t.Fatal(err)
	}
	if e, err = s.Trust(r.Ctx, r.Ch.Ref, "decided/key"); err != nil || e.Clean != 0 || e.Corrections != 1 || e.Approvals != 1 {
		t.Fatalf("after reject = %+v, %v", e, err)
	}
}

// An import merges by maximum under the bucket compare-and-set: a repeat
// and a lower import change nothing, a higher one raises.
func importOnPostgres(t *testing.T, r *enginetest.Rig, key string) {
	t.Helper()
	s := r.C.Operations()
	if _, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, key, nil, "live"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Trust(r.Ctx, r.Ch.Ref, key)
	if err != nil {
		t.Fatal(err)
	}
	imp := model.ImportTrust{Call: model.Call{Correlation: "i"}, Channel: r.Ch.Ref, Key: key, Clean: 1, Approvals: 1}
	got, err := s.ImportTrust(r.Ctx, imp)
	if err != nil || got.Clean != before.Clean || got.Approvals != before.Approvals || !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("lower import = %+v, %v; want %+v", got, err, before)
	}
	imp.Clean, imp.Approvals = before.Clean+5, before.Approvals+9
	got, err = s.ImportTrust(r.Ctx, imp)
	if err != nil || got.Clean != imp.Clean || got.Approvals != imp.Approvals {
		t.Fatalf("higher import = %+v, %v", got, err)
	}
	if again, err := s.ImportTrust(r.Ctx, imp); err != nil || again.Approvals != imp.Approvals {
		t.Fatalf("repeat = %+v, %v", again, err)
	}
}
