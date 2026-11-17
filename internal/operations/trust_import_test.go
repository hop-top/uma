package operations_test

import (
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

func imp(r *enginetest.Rig, key string, clean, approvals, corrections int64, last *time.Time) model.ImportTrust {
	return model.ImportTrust{
		Call: model.Call{Correlation: "i"}, Channel: r.Ch.Ref, Key: key,
		Clean: clean, Approvals: approvals, Corrections: corrections, LastCorrectionAt: last,
	}
}

func TestImportTrustSeedsEmptyAndIsIdempotent(t *testing.T) {
	r := newRig(t)
	s := trustSvc(r)
	last := r.Clock.Now().Add(-time.Hour)
	in := imp(r, "k", 5, 9, 2, &last)
	in.Labels = map[string]string{"plugin": "triage"}
	e, err := s.ImportTrust(r.Ctx, in)
	if err != nil || e.Clean != 5 || e.Approvals != 9 || e.Corrections != 2 || e.LastCorrectionAt == nil ||
		!e.LastCorrectionAt.Equal(last) || e.Labels["plugin"] != "triage" {
		t.Fatalf("import = %+v, %v", e, err)
	}
	r.Clock.Advance(time.Minute)
	again, err := s.ImportTrust(r.Ctx, in)
	if err != nil || again.UpdatedAt != e.UpdatedAt || again.Clean != 5 || again.Approvals != 9 || again.Corrections != 2 {
		t.Fatalf("re-import = %+v, %v; want unchanged %+v", again, err, e)
	}
	// Approvals defaults to Clean.
	if e, err := s.ImportTrust(r.Ctx, imp(r, "d", 3, 0, 0, nil)); err != nil || e.Approvals != 3 || e.Clean != 3 {
		t.Fatalf("defaulted = %+v, %v", e, err)
	}
}

func TestImportTrustNeverLowersLiveCounts(t *testing.T) {
	r := newRig(t)
	s := trustSvc(r)
	if _, err := s.ImportTrust(r.Ctx, imp(r, "k", 2, 4, 0, nil)); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"a", "b", "c"} {
		if _, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, "k", nil, b); err != nil {
			t.Fatal(err)
		}
	}
	before := mustTrust(t, r, "k") // clean 5, approvals 7
	r.Clock.Advance(time.Minute)
	e, err := s.ImportTrust(r.Ctx, imp(r, "k", 2, 4, 0, nil))
	if err != nil || e.Clean != 5 || e.Approvals != 7 || !e.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("lower import = %+v, %v; want %+v", e, err, before)
	}
	e, err = s.ImportTrust(r.Ctx, imp(r, "k", 8, 12, 1, nil))
	if err != nil || e.Clean != 8 || e.Approvals != 12 || e.Corrections != 1 {
		t.Fatalf("higher import = %+v, %v", e, err)
	}
}

// A live correction later than the imported history ended the streak the
// imported Clean describes: the count is not raised again, the totals are.
func TestImportTrustSkipsStaleCleanAfterLaterCorrection(t *testing.T) {
	r := newRig(t)
	s := trustSvc(r)
	last := r.Clock.Now().Add(-time.Hour)
	in := imp(r, "k", 6, 10, 1, &last)
	if _, err := s.ImportTrust(r.Ctx, in); err != nil {
		t.Fatal(err)
	}
	r.Clock.Advance(time.Minute)
	if _, err := s.RecordCorrection(r.Ctx, r.Ch.Ref, "k", nil); err != nil {
		t.Fatal(err)
	}
	live := mustTrust(t, r, "k")
	if live.Clean != 0 || live.Corrections != 2 {
		t.Fatalf("live = %+v", live)
	}
	e, err := s.ImportTrust(r.Ctx, in)
	if err != nil || e.Clean != 0 || e.Corrections != 2 || !e.LastCorrectionAt.Equal(*live.LastCorrectionAt) || !e.UpdatedAt.Equal(live.UpdatedAt) {
		t.Fatalf("re-import after correction = %+v, %v; want %+v", e, err, live)
	}
	// An import whose history has a later correction takes the later time.
	later := r.Clock.Now().Add(time.Hour)
	e, err = s.ImportTrust(r.Ctx, imp(r, "k", 3, 12, 3, &later))
	if err != nil || e.Clean != 3 || e.Corrections != 3 || !e.LastCorrectionAt.Equal(later) {
		t.Fatalf("later correction import = %+v, %v", e, err)
	}
}

func TestImportTrustValidation(t *testing.T) {
	r := newRig(t)
	s := trustSvc(r)
	long := string(make([]byte, model.MaxTrustKeyBytes+1))
	for name, in := range map[string]model.ImportTrust{
		"empty key":      imp(r, "", 1, 1, 0, nil),
		"long key":       imp(r, long, 1, 1, 0, nil),
		"negative clean": imp(r, "k", -1, 0, 0, nil),
		"negative appr":  imp(r, "k", 0, -1, 0, nil),
		"negative corr":  imp(r, "k", 0, 0, -1, nil),
		"clean>approved": imp(r, "k", 5, 2, 0, nil),
		"no call":        {Channel: r.Ch.Ref, Key: "k"},
	} {
		if _, err := s.ImportTrust(r.Ctx, in); !model.IsCode(err, model.Validation) {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
	bad := imp(r, "k", 1, 1, 0, nil)
	bad.Labels = map[string]string{"": "v"}
	if _, err := s.ImportTrust(r.Ctx, bad); !model.IsCode(err, model.Validation) {
		t.Errorf("bad label: err = %v", err)
	}
	if _, err := s.Trust(r.Ctx, r.Ch.Ref, "k"); !model.IsCode(err, model.NotFound) {
		t.Errorf("a refused import wrote: %v", err)
	}
}

func TestImportTrustNeedsTrustWrite(t *testing.T) {
	const other model.ActorID = "reviewer-3"
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Authorizer = denyReads{actor: other, actions: []string{model.ActionTrustWrite}}
	})
	d := pluginClient(t, r, other).Operations()
	_, err := d.ImportTrust(r.Ctx, imp(r, "k", 1, 1, 0, nil))
	requireCode(t, err, model.Unauthorized, "")
	if _, err := trustSvc(r).Trust(r.Ctx, r.Ch.Ref, "k"); !model.IsCode(err, model.NotFound) {
		t.Fatalf("denied import wrote: %v", err)
	}
	// The grant is per action: the reviewer holds the others.
	if _, err := trustSvc(r).ImportTrust(r.Ctx, imp(r, "k", 1, 1, 0, nil)); err != nil {
		t.Fatal(err)
	}
}
