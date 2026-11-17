package conformance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// A provider with coarse revisions reports two edits of one message within
// one revision: both edits carry the same source revision and time, with
// different content. The engine cannot order them, so it keeps what it
// holds and queues a durable refetch; the refetched copy is authoritative.
// The process dies right after the refetched copy commits. The next owner
// refetches again: the copy is a duplicate, applied once, and the refetch
// settles.
func coarseRevisionTieRefetches(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	needsAdvance(t, e)
	f.p.refetches = true
	at := f.fx.Now().Add(time.Second)
	edit := func(event, text string) model.Ingress {
		in := f.message(event, "m-1")
		in.Envelope.Kind = model.EnvelopeMessageEdited
		in.Envelope.OccurredAt, in.Envelope.SourceRevision = at, at.Format(time.RFC3339)
		in.Envelope.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}
		return in
	}
	latest := edit("", "third").Envelope
	f.p.current = map[string]model.Envelope{"m-1": latest}
	p := f.start(t)
	_, stop := p.receive()
	for _, in := range []model.Ingress{f.message("evt-1", "m-1"), edit("evt-2", "second"), edit("evt-3", "third")} {
		f.push(t, in)
		if r := f.accept(t); r.err != nil || !r.receipt.Committed {
			t.Fatalf("ingress %s = %+v, %v; want committed", in.Envelope.EventID, r.receipt, r.err)
		}
	}
	stop()
	if got := f.text(t); got != "second" {
		t.Fatalf("content after the tie = %q, want the copy held, %q", got, "second")
	}
	p.store.at(afterCommit, func(u *unitLog) bool {
		return len(u.ingress) == 1 && strings.HasPrefix(u.ingress[0].Input.Envelope.EventID, "uma.refetch.")
	}, p.store.crash)
	p.work()
	p.crashed(t)
	if got := f.text(t); got != "third" {
		t.Fatalf("content after the refetch = %q, want the provider's copy", got)
	}

	p = f.restart(t, cross)
	p.work()
	p.idle(t)
	p.later(t, leaseExpiry)
	ops := f.refetchOps(t)
	if len(ops) != 1 || ops[0].State != model.Committed {
		t.Fatalf("refetch operations = %+v, want one committed", ops)
	}
	if got := f.p.log(); len(got) != 2 || got[0] != "refetch" || got[1] != "refetch" {
		t.Fatalf("provider calls = %v, want the refetch, repeated once after the crash", got)
	}
	var edits int
	for _, ev := range f.allEvents(t, model.Filter{}, 50) {
		if ev.Kind != model.EventMessageUpdated {
			continue
		}
		var pl model.MessageUpdatedPayload
		if err := json.Unmarshal(ev.Payload.Data, &pl); err != nil {
			t.Fatal(err)
		}
		if pl.Change == model.MessageChangeEdited {
			edits++
		}
	}
	if edits != 2 {
		t.Fatalf("message.updated edits = %d, want the second edit and the refetched copy once", edits)
	}
}

// text returns the content of the env's one message.
func (f *faultEnv) text(t *testing.T) string {
	t.Helper()
	msgs := f.messages(t)
	if len(msgs) != 1 || len(msgs[0].Content.Parts) == 0 {
		t.Fatalf("messages = %+v, want one with content", msgs)
	}
	return msgs[0].Content.Parts[0].Text
}

// refetchOps returns the operations whose work ran a refetch.
func (f *faultEnv) refetchOps(t *testing.T) []model.Operation {
	t.Helper()
	var out []model.Operation
	for _, ev := range f.allEvents(t, model.Filter{}, 50) {
		if ev.Kind != model.EventDeliveryUpdated {
			continue
		}
		ref, err := ev.Resource.AsOperation()
		if err != nil {
			t.Fatal(err)
		}
		op := f.proc.op(t, ref)
		if op.Result != nil && op.Result.Kind == model.KindMessage {
			out = append(out, op)
		}
	}
	return out
}
