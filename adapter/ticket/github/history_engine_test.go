package github_test

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// Runtime.Import over an issue's conversation imports the comments uma
// does not hold, marked as imported, and skips what it holds whichever way
// it arrived: the description from the webhook and uma's own reply, which
// the send recorded by its comment ID. A second import skips everything.
func TestEngineImportsIssueHistoryOnce(t *testing.T) {
	r := newEngineRig(t)
	r.gh.setIssue("a", sandboxRepo, 7, 5000001, "open", nil, t0)
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	op := r.reply("On it.")
	if op.State != model.ProviderAccepted {
		t.Fatalf("reply = %+v", op)
	}
	missed := r.gh.seed("a", sandboxRepo+"#7", histUser, "Any news?", t0.Add(-time.Hour))
	r.gh.seed("a", sandboxRepo+"#7", histUser, "Before the window.", t0.Add(-48*time.Hour))
	conv := r.conversations()[0].Ref
	req := uma.ImportRequest{Requester: "agent-1", Conversation: &conv, Since: t0.Add(-24 * time.Hour)}

	rep, err := r.engine.Runtime().Import(r.ctx, r.call(), req)
	// The description (created 2026-10-11T09:00Z, in the window), the
	// reply and the missed comment.
	if err != nil || rep != (uma.ImportReport{Imported: 1, Skipped: 2, Complete: true}) {
		t.Fatalf("Import = %+v, %v; want the missed comment imported, the held two skipped", rep, err)
	}
	msgs := r.messages()
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want description, reply and the missed comment", len(msgs))
	}
	imported := slices.IndexFunc(msgs, func(m model.Message) bool { return m.ImportedBy != "" })
	if imported < 0 || !slices.Contains(msgs[imported].External, commentRef(chanA, missed.ID)) || msgs[imported].ImportedBy != "agent-1" ||
		msgs[imported].Ref.Conversation != conv {
		t.Fatalf("messages = %+v, want the missed comment imported by agent-1 into the conversation", msgs)
	}
	again, err := r.engine.Runtime().Import(r.ctx, r.call(), req)
	if err != nil || again != (uma.ImportReport{Skipped: 3, Complete: true}) {
		t.Fatalf("second Import = %+v, %v; want all skipped", again, err)
	}
}
