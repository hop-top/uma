package conformance

import (
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// searchWalk pages f at limit until the query completes, calling between
// after every page that does not complete it, and returns the ids listed.
// A completing page carries no Next.
func (env *serviceEnv) searchWalk(f model.Filter, limit int, between func()) []model.ID {
	env.t.Helper()
	var out []model.ID
	var after model.Cursor
	for range 1000 {
		p := env.searchPage(f, limit, after)
		if len(p.Items) > limit {
			env.t.Fatalf("page of %d items at limit %d", len(p.Items), limit)
		}
		out = append(out, pageIDs(p)...)
		if p.Complete {
			if p.Next != "" {
				env.t.Fatalf("completing page carries Next %q", p.Next)
			}
			return out
		}
		if p.Next == "" {
			env.t.Fatal("incomplete page without Next")
		}
		after = p.Next
		if between != nil {
			between()
		}
	}
	env.t.Fatal("search never completed")
	return nil
}

func noRepeats(t *testing.T, what string, ids []model.ID) {
	t.Helper()
	seen := map[model.ID]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("%s: message %s listed twice in %v", what, id, ids)
		}
		seen[id] = true
	}
}

// searchPagingUnderInserts: messages recorded between pages, matching or
// not, readable or not, appear on a later page of the same walk, never
// twice and never out of recording order; the walk ends with no Next, so a
// match recorded after it is found by running the search again.
func searchPagingUnderInserts(t *testing.T, env *serviceEnv) {
	f := model.Filter{Text: "refund request"}
	var first []model.ID
	for i := range 12 {
		if i%4 == 3 {
			env.searchMessage(model.Internal, searchAt, "refund request")
			continue
		}
		first = append(first, env.searchMessage(model.Public, searchAt, "refund request").Ref.ID)
	}
	inserted := 0
	got := env.searchWalk(f, 3, func() {
		if inserted < 12 {
			env.searchMessage(model.Internal, searchAt, "refund request")
			env.searchMessage(model.Public, searchAt, "thanks")
			env.searchMessage(model.Public, searchAt, "refund request")
			inserted++
		}
	})
	noRepeats(t, "walk", got)
	if len(got) < len(first) || !slices.Equal(got[:len(first)], first) {
		t.Fatalf("walk %v; want the earlier matches %v first, in order", got, first)
	}
	if want := env.messages(f, 100); !slices.Equal(got, want) {
		t.Fatalf("walk %v differs from a fresh search %v", got, want)
	}
	late := env.searchMessage(model.Public, searchAt, "refund request")
	if again := env.messages(f, 3); len(again) != len(got)+1 || again[len(again)-1] != late.Ref.ID {
		t.Fatalf("rerun %v; want the walk %v then %s", again, got, late.Ref.ID)
	}
}

// searchPagingUnderConcurrentInserts: a writer recording matches while a
// reader pages never makes the reader repeat a message, skip an earlier
// one or see recording order broken; what the walk lists is a prefix of
// what a search lists once the writer is done.
func searchPagingUnderConcurrentInserts(t *testing.T, env *serviceEnv) {
	f := model.Filter{Text: "refund request"}
	var first []model.ID
	for range 12 {
		first = append(first, env.searchMessage(model.Public, searchAt, "refund request").Ref.ID)
	}
	var incoming []model.Message
	for i := range 40 {
		text := "refund request"
		vis := model.Public
		if i%3 == 1 {
			vis = model.Internal
		}
		incoming = append(incoming, env.searchValue(vis, searchAt, text))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	start := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for _, m := range incoming {
			if err := env.st.Transact(env.ctx, serviceTenant, func(tx store.Tx) error { return tx.PutMessage(env.ctx, m, "") }); err != nil {
				errs <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	close(start)
	got := env.searchWalk(f, 4, func() { time.Sleep(2 * time.Millisecond) })
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("concurrent writer: %v", err)
	default:
	}
	noRepeats(t, "walk", got)
	final := env.messages(f, 100)
	if len(got) < len(first) || !slices.Equal(got[:len(first)], first) {
		t.Fatalf("walk %v; want the earlier matches %v first, in order", got, first)
	}
	if !slices.Equal(got, final[:len(got)]) {
		t.Fatalf("walk %v is not a prefix of the final search %v", got, final)
	}
	if want := len(first) + 27; len(final) != want { // 40 incoming, 13 of them notes
		t.Fatalf("final search lists %d messages, want %d", len(final), want)
	}
}

// searchCursorBinding: a cursor belongs to the filter it was issued for. An
// equivalent spelling of the filter (token order, case, repeats) accepts
// it; any other filter refuses it as model.Validation.
func searchCursorBinding(t *testing.T, env *serviceEnv) {
	vip := env.searchLabel("vip")
	for range 4 {
		env.searchMessage(model.Public, searchAt, "refund request")
	}
	p := env.searchPage(model.Filter{Text: "Refund request"}, 1, "")
	if p.Complete || p.Next == "" {
		t.Fatalf("first page %+v; want an incomplete page with Next", p)
	}
	if got := env.searchPage(model.Filter{Text: "REQUEST refund request"}, 1, p.Next); len(got.Items) != 1 {
		t.Fatalf("equivalent filter page = %+v; want the next match", got)
	}
	other := searchAt.Add(time.Hour)
	for name, f := range map[string]model.Filter{
		"other text":   {Text: "refund"},
		"extra tokens": {Text: "refund request now"},
		"added date":   {Text: "refund request", Since: &other},
		"added label":  {Text: "refund request", Labels: &model.LabelSelector{Any: []model.LabelRef{vip}}},
		"no search":    {},
	} {
		_, err := env.client.Messages().List(env.ctx, serviceList(f, 1, p.Next))
		if model.CodeOf(err) != model.Validation {
			t.Fatalf("%s: error = %v, want %s", name, err, model.Validation)
		}
	}
}

// searchScanBound: a list scans a bounded number of stored matches per
// call, readable or not (five times the largest page, 500), then returns
// what it found with a Next to continue from. A search whose first matches
// are all unreadable therefore pages, but never loses a readable match nor
// lists one twice, and the walk ends with no Next.
func searchScanBound(t *testing.T, env *serviceEnv) {
	var hidden []model.Message
	for range 520 {
		hidden = append(hidden, env.searchValue(model.Internal, searchAt, "ledger entry"))
	}
	env.put(func(tx store.Tx) error {
		for _, m := range hidden {
			if err := tx.PutMessage(env.ctx, m, ""); err != nil {
				return err
			}
		}
		return nil
	})
	var visible []model.ID
	for range 3 {
		visible = append(visible, env.searchMessage(model.Public, searchAt, "ledger entry").Ref.ID)
		env.searchMessage(model.Internal, searchAt, "ledger entry")
	}
	f := model.Filter{Text: "ledger"}
	for _, limit := range []int{1, 2, 100} {
		got := env.searchWalk(f, limit, nil)
		noRepeats(t, "walk", got)
		if !slices.Equal(got, visible) {
			t.Fatalf("limit %d: walk %v, want %v", limit, got, visible)
		}
	}
}
