package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"hop.top/uma/model"
)

// attachExport exports a conversation holding a note with an attachment
// through Privacy.Export, reads it back through Privacy.Open, replays the
// export on retry and, once past the Exports horizon, purges it with its
// bytes. The attachment is read under the note's internal audience, so
// the caller is staff.
func attachExport(t *testing.T, env *attachEnv) {
	env.policy.set(func(a *attachAuthorizer) { a.staff = true })
	data := []byte("scan bytes")
	ref := env.mustPrepare(publicMeta("scan.txt", data))
	if _, err := env.upload(ref, env.cmd(""), bytes.NewReader(data)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	conv := env.conversation(env.inbox, model.ConversationOpen)
	note, err := env.client.Messages().AddInternalNote(env.ctx, model.InternalNote{
		Command: env.cmd(""), Conversation: conv.Ref, PolicyRef: "staff",
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "note"}, {Kind: model.PartAttachment, Attachment: &ref}}},
	})
	if err != nil {
		t.Fatalf("AddInternalNote: %v", err)
	}
	req := model.ExportRequest{Command: env.cmd(""), Filter: model.Filter{Conversation: &conv.Ref}, Format: model.ExportFormatJSONL}
	a, err := env.client.Privacy().Export(env.ctx, req)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	xref, err := a.Operation.Result.AsExport()
	if err != nil {
		t.Fatalf("export result %+v: %v", a.Operation.Result, err)
	}
	env.requireHeld(2)
	env.requireIntents(0)
	x, rc, err := env.client.Privacy().Open(env.ctx, model.Lookup[model.ExportRef]{Call: model.Call{Correlation: "c"}, Ref: xref})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || int64(len(body)) != x.Size {
		t.Fatalf("export bytes = %d, %v; want %d", len(body), err, x.Size)
	}
	types := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		var l model.ExportLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		types[l.Type]++
		if l.Type == model.ExportLineMessage {
			var m model.Message
			if err := json.Unmarshal(l.Data, &m); err != nil || m.Ref != *note.Operation.ResultMessage {
				t.Errorf("message line = %s, want the note", l.Data)
			}
		}
	}
	if types[model.ExportLineExport] != 1 || types[model.ExportLineConversation] != 1 ||
		types[model.ExportLineMessage] != 1 || types[model.ExportLineAttachment] != 1 {
		t.Errorf("line types = %v", types)
	}

	again, err := env.client.Privacy().Export(env.ctx, req)
	if err != nil || !again.Replayed || *again.Operation.Result != xref.Ref() {
		t.Fatalf("retry = %+v, %v; want the stored export", again, err)
	}
	env.requireHeld(2)

	if env.fx.Advance == nil {
		return
	}
	env.advance(2 * attachHorizon)
	rep, err := env.purge()
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if rep.Exports != 1 {
		t.Errorf("purge report = %+v, want one export", rep)
	}
	env.requireHeld(1)
	env.requireIntents(0)
	_, _, err = env.client.Privacy().Open(env.ctx, model.Lookup[model.ExportRef]{Call: model.Call{Correlation: "c"}, Ref: xref})
	requireCodeOf(t, err, model.NotFound)
}
