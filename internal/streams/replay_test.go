package streams_test

import (
	"encoding/hex"
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/streams"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// ledgerEntry is the part of a stream command's ledger record that decides
// whether an operation stored before this package existed still executes
// and replays: its policy action, fingerprint and durable intent.
type ledgerEntry struct {
	kind        string
	fingerprint string
	intent      string
}

// streamLedger is what the Interactions service stored for the same
// commands before stream lifecycle moved to the Streams service.
var streamLedger = []ledgerEntry{
	{model.ActionStreamWrite, "adb19959fb2a48adb3f77cdf322e2bd06051aa4de2f02b4cd574edb32f823023",
		`{"stream":{"conversation":{"inbox":{"tenant":"tenant-a","id":"00000001-0000-7000-8000-000000000001"},"id":"00000001-0000-7000-8000-000000000003"},"id":"0000000b-0000-7000-8000-000000000001"},"step":"start","mode":"native","audience":{"visibility":"public","policy_ref":""},"sequence":0,"content":{},"cancel":false}`},
	{model.ActionStreamWrite, "29d4ba50aa45cd852385b64b2482531c8da8135bb51d50e497e918f26b0463c6",
		`{"stream":{"conversation":{"inbox":{"tenant":"tenant-a","id":"00000001-0000-7000-8000-000000000001"},"id":"00000001-0000-7000-8000-000000000003"},"id":"0000000b-0000-7000-8000-000000000001"},"step":"update","mode":"native","audience":{"visibility":"public","policy_ref":""},"sequence":1,"content":{"parts":[{"kind":"text","text":"Hel"}]},"cancel":false,"previous":{"tenant":"tenant-a","id":"0000000b-0000-7000-8000-000000000002"}}`},
	{model.ActionStreamWrite, "f5fbebf5f606475839a4a7df1a190f81dacc815d2f4acec12642bd17eb1582dc",
		`{"stream":{"conversation":{"inbox":{"tenant":"tenant-a","id":"00000001-0000-7000-8000-000000000001"},"id":"00000001-0000-7000-8000-000000000003"},"id":"0000000b-0000-7000-8000-000000000001"},"step":"end","mode":"native","audience":{"visibility":"public","policy_ref":""},"sequence":1,"content":{},"cancel":false,"previous":{"tenant":"tenant-a","id":"0000000b-0000-7000-8000-000000000006"}}`},
	{model.ActionStreamWrite, "2a330d3aec7ada1f56fbf24cb3a523b7a29f46ccfabdd4935dda6285713a6b45",
		`{"stream":{"conversation":{"inbox":{"tenant":"tenant-a","id":"00000001-0000-7000-8000-000000000001"},"id":"00000001-0000-7000-8000-000000000003"},"id":"0000000b-0000-7000-8000-00000000000e"},"step":"end","mode":"buffer","audience":{"visibility":"public","policy_ref":""},"sequence":0,"content":{},"cancel":true,"previous":{"tenant":"tenant-a","id":"0000000b-0000-7000-8000-00000000000f"}}`},
}

const streamIntent = "uma.intent.stream"

// Stream commands stored before the move execute and replay through the
// Streams service: the intent name, intent bytes, policy action and
// fingerprint are those the Interactions service stored, the effector
// decodes those intents, and a retransmitted command returns the stored
// operation.
func TestStreamReplayAcrossServices(t *testing.T) {
	if got := streams.StreamEffector().Intent(); got != streamIntent {
		t.Fatalf("effector intent = %q, want %q", got, streamIntent)
	}
	r := newRig(t, everything...)
	start, ref := r.start(model.StreamNative)
	update := r.update(ref, 1, "Hel")
	end := r.end(ref, 1, false)
	_, buffered := r.start(model.StreamBuffer)
	cancel := r.end(buffered, 0, true)
	for i, a := range []model.Acceptance{start, update, end, cancel} {
		want := streamLedger[i]
		rec := r.record(a.Operation.Ref)
		if rec.Key.Kind != want.kind {
			t.Errorf("command %d: action = %q, want %q", i, rec.Key.Kind, want.kind)
		}
		if rec.FingerprintVersion != 1 || hex.EncodeToString(rec.Fingerprint) != want.fingerprint {
			t.Errorf("command %d: fingerprint v%d %x, want v1 %s", i, rec.FingerprintVersion, rec.Fingerprint, want.fingerprint)
		}
		if rec.Command.Name != streamIntent || rec.Command.Version != 1 || string(rec.Command.Data) != want.intent {
			t.Errorf("command %d: intent %s v%d %s, want %s v1 %s", i, rec.Command.Name, rec.Command.Version, rec.Command.Data, streamIntent, want.intent)
		}
		stored := model.Extension{Name: streamIntent, Version: 1, Data: []byte(want.intent)}
		if _, err := streams.StreamEffector().Decode(stored); err != nil {
			t.Errorf("command %d: stored intent does not decode: %v", i, err)
		}
	}
	again, err := r.s.Start(r.ctx, model.StartStream{
		Command:      model.Command{Call: model.Call{Correlation: "corr"}, Operation: "op-1"},
		Conversation: r.conv.Ref, Audience: public(), Mode: model.StreamNative,
	})
	if err != nil {
		t.Fatalf("retransmitted start: %v", err)
	}
	if !again.Replayed || again.Operation.Ref != start.Operation.Ref {
		t.Fatalf("retransmitted start = %+v, want the stored operation %v", again, start.Operation.Ref)
	}
	r.drain()
	if st := r.stream(ref); st.State != model.StreamFinished || st.AppliedSequence != 1 {
		t.Fatalf("stream = %+v, want finished through sequence 1", st)
	}
	if st := r.stream(buffered); st.State != model.StreamCancelled {
		t.Fatalf("buffered stream state = %q, want cancelled", st.State)
	}
}

// record reads op's ledger record.
func (r *rig) record(op model.OperationRef) store.OperationRecord {
	r.t.Helper()
	var rec store.OperationRecord
	err := r.e.Store().View(r.ctx, enginetest.TenantA, func(rd store.Reader) error {
		var err error
		rec, err = rd.Operation(r.ctx, op)
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return rec
}
