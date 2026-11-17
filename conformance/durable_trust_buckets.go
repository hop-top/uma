package conformance

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Trust counters live in channel-scoped protocol records, one record per hash
// bucket (key "uma.trust.<2 hex digits>"). A count is a read-modify-write of
// one record in one unit, so these cases pin the compare-and-set a counter
// relies on: a stale revision never lands, and concurrent increments converge
// with none lost.
var trustBucketCases = []durableCase{
	{"ProtocolBucketRaceHasOneWinner", func(t *testing.T, e *env, _ func(*testing.T)) {
		const key = "uma.trust.0a"
		put := func(data string, rev, expected model.Revision) error {
			return e.s.Transact(e.ctx, tenantA, func(tx store.Tx) error {
				return tx.PutProtocol(e.ctx, bucket(e, key, data, rev), expected)
			})
		}
		if err := put(`{"n":0}`, "b0", ""); err != nil {
			t.Fatal(err)
		}
		// Both writers read b0 before either commits.
		const writers = 2
		errs := make([]error, writers)
		meet := newRendezvous(writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				errs[i] = e.s.Transact(e.ctx, tenantA, func(tx store.Tx) error {
					err := tx.PutProtocol(e.ctx, bucket(e, key, fmt.Sprintf(`{"n":%d}`, i+1), model.Revision(fmt.Sprintf("w%d", i))), "b0")
					meet.wait()
					return err
				})
			})
		}
		wg.Wait()
		var winners, losers int
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
			case model.IsCode(err, model.Conflict), model.IsCode(err, model.Transient):
				losers++
			default:
				t.Errorf("writer %d: %v, want nil, Conflict or Transient", i, err)
			}
		}
		if winners != 1 || losers != 1 {
			t.Fatalf("winners=%d losers=%d (%v), want exactly one of each", winners, losers, errs)
		}
		// The loser stored nothing; a retry on the winner's revision lands.
		var winner model.Revision
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Protocol(e.ctx, e.channel, key)
			winner = got.Revision
			return err
		})
		if winner != "w0" && winner != "w1" {
			t.Fatalf("revision = %q, want the winner's", winner)
		}
		if err := put(`{"n":9}`, "b1", winner); err != nil {
			t.Fatalf("retry on the winner's revision: %v", err)
		}
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.PutProtocol(e.ctx, bucket(e, key, `{"n":7}`, "b2"), winner)
		}), model.Conflict)
	}},
	{"ProtocolBucketIncrementsConverge", func(t *testing.T, e *env, _ func(*testing.T)) {
		const key = "uma.trust.3f"
		const workers, each = 8, 5
		var wg sync.WaitGroup
		meet := newRendezvous(workers)
		for w := range workers {
			wg.Go(func() {
				for n := range each {
					unit := func(tx store.Tx) error {
						cur, err := tx.Protocol(e.ctx, e.channel, key)
						count, expected := 0, model.Revision("")
						switch {
						case model.IsCode(err, model.NotFound):
						case err != nil:
							return err
						default:
							var v struct{ N int }
							if err := json.Unmarshal(cur.Value.Data, &v); err != nil {
								return err
							}
							count, expected = v.N, cur.Revision
						}
						return tx.PutProtocol(e.ctx, bucket(e, key, fmt.Sprintf(`{"N":%d}`, count+1), model.Revision(fmt.Sprintf("c%d", count+1))), expected)
					}
					var err error
					for range 500 {
						if n == 0 {
							err = e.overlapTx(meet, unit)
						} else {
							err = e.retryTx(unit)
						}
						if !model.IsCode(err, model.Conflict) {
							break
						}
					}
					if err != nil {
						t.Errorf("worker %d increment %d: %v", w, n, err)
					}
				}
			})
		}
		wg.Wait()
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Protocol(e.ctx, e.channel, key)
			if err != nil {
				return err
			}
			var v struct{ N int }
			if err := json.Unmarshal(got.Value.Data, &v); err != nil {
				return err
			}
			if v.N != workers*each || got.Revision != model.Revision(fmt.Sprintf("c%d", workers*each)) {
				t.Errorf("count = %d at %q, want %d: an increment was lost or doubled", v.N, got.Revision, workers*each)
			}
			return nil
		})
	}},
}

func bucket(e *env, key, data string, rev model.Revision) store.ProtocolState {
	return store.ProtocolState{
		Channel: e.channel, Key: key,
		Value: model.Extension{Name: "conformance.trust.bucket", Version: 1, Data: []byte(data)}, Revision: rev,
	}
}
