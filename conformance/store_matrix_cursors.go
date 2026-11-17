package conformance

import (
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// cursorSubject is one paged read set up for the cursor rules.
type cursorSubject struct {
	name string
	// setup stores a few items and returns the read, unfiltered, and a
	// function committing one more item it returns.
	setup func(t *testing.T, e *env) (read pageReader, add func())
}

type pageReader func(r store.Reader, p model.PageRequest) (readView, error)

// cursorSubjects is every paged read: the filtered ones over the filter
// matrix world, and the reads that take a reference instead of a filter.
func cursorSubjects() []cursorSubject {
	var out []cursorSubject
	for _, rd := range filteredReads {
		out = append(out, cursorSubject{rd.name, func(t *testing.T, e *env) (pageReader, func()) {
			w := e.seedMatrixWorld(t)
			return func(r store.Reader, p model.PageRequest) (readView, error) {
					return rd.read(w, r, model.Filter{}, p)
				},
				func() { rd.add(t, e, w) }
		}})
	}
	return append(out,
		cursorSubject{"Replies", func(t *testing.T, e *env) (pageReader, func()) {
			conv := e.putConversation(t)
			parent := e.putLinked(t, conv, nil, nil)
			add := func() { e.putLinked(t, conv, nil, &parent) }
			add()
			return func(r store.Reader, p model.PageRequest) (readView, error) {
				pg, err := r.Replies(e.ctx, parent, p)
				return viewOf(pg, err, func(v model.Message) model.ID { return v.Ref.ID })
			}, add
		}},
		cursorSubject{"MessagesByAttachment", func(t *testing.T, e *env) (pageReader, func()) {
			conv, att := e.putConversation(t), e.putSearchAttachment(t, "image/png")
			add := func() { e.putSearchMessage(t, conv, e.at(1), attachmentParts(att)...) }
			add()
			return func(r store.Reader, p model.PageRequest) (readView, error) {
				pg, err := r.MessagesByAttachment(e.ctx, att, p)
				return viewOf(pg, err, func(v model.Message) model.ID { return v.Ref.ID })
			}, add
		}},
		cursorSubject{"Attempts", func(t *testing.T, e *env) (pageReader, func()) {
			rec := e.operation(t, "cursor-attempts", []byte{1})
			e.mustTx(t, func(tx store.Tx) error {
				_, _, err := tx.ClaimOperation(e.ctx, rec)
				return err
			})
			add := func() {
				e.mustTx(t, func(tx store.Tx) error {
					return tx.AppendAttemptEvidence(e.ctx, store.AttemptEvidence{
						Operation: rec.Operation.Ref, Attempt: e.id(t), State: model.Delivered, ObservedAt: e.fx.Now(),
					})
				})
			}
			add()
			return func(r store.Reader, p model.PageRequest) (readView, error) {
				pg, err := r.Attempts(e.ctx, rec.Operation.Ref, p)
				return viewOf(pg, err, func(v store.AttemptEvidence) model.ID { return v.Attempt })
			}, add
		}},
		cursorSubject{"IdentityLinks", func(t *testing.T, e *env) (pageReader, func()) {
			x := e.provisional(t)
			to := []model.ContactRef{e.putContact(t), e.putContact(t)}
			n := 0
			add := func() { e.move(t, &x, to[n%2]); n++ }
			add()
			return func(r store.Reader, p model.PageRequest) (readView, error) {
				pg, err := r.IdentityLinks(e.ctx, store.LinkFilter{Identity: &x.id.Ref}, p)
				return viewOf(pg, err, func(v store.LinkRecord) model.ID { return v.ID })
			}, add
		}},
	)
}

// cursorRun drives one read in fresh units, so a cursor is only ever the
// opaque value a previous unit returned.
type cursorRun struct {
	t    *testing.T
	e    *env
	name string
	read pageReader
}

func (c cursorRun) page(after model.Cursor, limit int) (readView, error) {
	var v readView
	err := c.e.s.View(c.e.ctx, tenantA, func(r store.Reader) error {
		var err error
		v, err = c.read(r, model.PageRequest{Limit: limit, After: after})
		return err
	})
	return v, err
}

// from pages to the end starting after the cursor, one unit per page.
func (c cursorRun) from(after model.Cursor, limit int) []string {
	c.t.Helper()
	var keys []string
	for range 200 {
		v, err := c.page(after, limit)
		if err != nil {
			c.t.Fatalf("%s: page after %q at limit %d: %v", c.name, after, limit, err)
		}
		keys = append(keys, v.keys...)
		if v.complete {
			return keys
		}
		if v.next == "" || v.next == after {
			c.t.Fatalf("%s: incomplete page after %q returned next %q, which does not advance", c.name, after, v.next)
		}
		after = v.next
	}
	c.t.Fatalf("%s: paging did not complete", c.name)
	return nil
}

// cursorMatrixCases pin the cursor rules of the store package for every
// paged read: the limit must be positive, a cursor a read cannot decode is
// model.Validation, a cursor resumes after the item it followed whatever
// the page size and in any unit, and a commit between pages neither repeats
// nor skips an item already reached.
var cursorMatrixCases = func() []storeCase {
	var cases []storeCase
	for _, s := range cursorSubjects() {
		cases = append(cases, storeCase{s.name, func(t *testing.T, e *env) {
			read, add := s.setup(t, e)
			c := cursorRun{t: t, e: e, name: s.name, read: read}
			for len(c.from("", 100)) < 4 {
				add()
			}
			base := c.from("", 100)

			t.Run("limit", func(t *testing.T) {
				for _, limit := range []int{0, -1} {
					_, err := c.page("", limit)
					wantCode(t, err, model.Validation, s.name, " at limit ", limit)
				}
			})
			t.Run("malformed", func(t *testing.T) {
				_, err := c.page("not-a-cursor", 1)
				wantCode(t, err, model.Validation)
				first, err := c.page("", 1)
				if err != nil {
					t.Fatal(err)
				}
				if first.next == "" {
					t.Fatalf("%s: first page of %d items at limit 1 returned no cursor", s.name, len(base))
				}
				_, err = c.page(first.next+"~", 1)
				wantCode(t, err, model.Validation)
			})
			t.Run("resume", func(t *testing.T) {
				for _, limit := range []int{1, 2, 3, 100} {
					first, err := c.page("", limit)
					if err != nil {
						t.Fatal(err)
					}
					if first.complete {
						continue
					}
					got := slices.Concat(first.keys, c.from(first.next, limit))
					if !slices.Equal(got, base) {
						t.Errorf("%s: pages at limit %d = %v, want %v: a cursor must resume exactly after the last item returned", s.name, limit, got, base)
					}
					// A cursor is not consumed: the same one resumes the same way.
					again := slices.Concat(first.keys, c.from(first.next, limit))
					if !slices.Equal(again, got) {
						t.Errorf("%s: resuming one cursor twice gave %v then %v", s.name, got, again)
					}
					// The page size may change on resume.
					if limit < 100 {
						got = slices.Concat(first.keys, c.from(first.next, 100))
						if !slices.Equal(got, base) {
							t.Errorf("%s: resumed at limit 100 after a page of %d = %v, want %v", s.name, limit, got, base)
						}
					}
				}
			})
			t.Run("stable", func(t *testing.T) {
				first, err := c.page("", 1)
				if err != nil {
					t.Fatal(err)
				}
				if first.complete {
					t.Fatalf("%s: %d items fit one page at limit 1", s.name, len(base))
				}
				add()
				added := keysAbsentFrom(c.from("", 100), base)
				if len(added) != 1 {
					t.Fatalf("%s: one add changed the read by %v, want exactly one item", s.name, added)
				}
				got := slices.Concat(first.keys, c.from(first.next, 1))
				got = slices.DeleteFunc(got, func(k string) bool { return k == added[0] })
				if !slices.Equal(got, base) {
					t.Errorf("%s: paging across a commit = %v, want %v with the new item %s at most once: a cursor stays valid across commits and never repeats or skips an item", s.name, got, base, added[0])
				}
			})
		}})
	}
	return append(cases, storeCase{"EventsCursorBeyondHead", func(t *testing.T, e *env) {
		var future model.Cursor
		_ = e.txErr(func(tx store.Tx) error {
			if err := tx.AppendEvents(e.ctx, []model.Event{e.event(t, "matrix.one"), e.event(t, "matrix.two")}); err != nil {
				return err
			}
			var err error
			future, err = tx.EventHead(e.ctx)
			if err != nil {
				t.Errorf("EventHead: %v", err)
			}
			return errAbort
		})
		if future == "" {
			t.Fatal("EventHead of a unit holding two appends is the beginning of the log")
		}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Events(e.ctx, model.Filter{}, future, 10)
			wantCode(t, err, model.Validation)
			return nil
		})
	}}, storeCase{"EventsNextIsACheckpoint", func(t *testing.T, e *env) {
		e.mustTx(t, func(tx store.Tx) error {
			return tx.AppendEvents(e.ctx, []model.Event{e.event(t, "matrix.one"), e.event(t, "matrix.two")})
		})
		var next model.Cursor
		e.mustView(t, func(r store.Reader) error {
			// Next is the position after the last scanned event, also when
			// the filter selects none of them.
			p, err := r.Events(e.ctx, model.Filter{Kinds: []string{"matrix.absent"}}, "", 10)
			next = p.Next
			if err == nil && (len(p.Items) != 0 || !p.Complete || next == "") {
				t.Errorf("filtered page = %d items complete=%v next=%q, want none, complete, and a checkpoint", len(p.Items), p.Complete, next)
			}
			return err
		})
		later := e.event(t, "matrix.absent")
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, []model.Event{later}) })
		got := e.allEventsAfter(t, model.Filter{}, next)
		if len(got) != 1 || got[0].ID != later.ID {
			t.Errorf("events after the checkpoint = %d, want only the one committed since", len(got))
		}
	}})
}()

// keysAbsentFrom returns the keys of a that b lacks.
func keysAbsentFrom(a, b []string) []string {
	return slices.DeleteFunc(slices.Clone(a), func(k string) bool { return slices.Contains(b, k) })
}

// allEventsAfter pages the events matching filter after cursor.
func (e *env) allEventsAfter(t *testing.T, filter model.Filter, after model.Cursor) []model.Event {
	t.Helper()
	var out []model.Event
	for range 100 {
		var p model.Page[model.Event]
		e.mustView(t, func(r store.Reader) error {
			var err error
			p, err = r.Events(e.ctx, filter, after, 10)
			return err
		})
		out = append(out, p.Items...)
		if p.Complete {
			return out
		}
		after = p.Next
	}
	t.Fatal("event paging did not complete")
	return nil
}
