package github_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
)

const issueKey = sandboxRepo + "#7"

// target is the provider message reference of comment id.
func target(id int64) *model.ExternalRef {
	ref := commentRef(chanA, id)
	return &ref
}

// descriptionTarget is the provider message reference of the description
// of issue 7.
func descriptionTarget() *model.ExternalRef {
	ref := model.ProviderMessageRef(chanA, "issue:3000000007")
	return &ref
}

func mutEffect(attempt model.ID, t *model.ExternalRef) adapter.Effect {
	return adapter.Effect{
		Operation: opA, Attempt: attempt, Route: issueRoute("7"), Target: t,
		Audience: model.Audience{Visibility: model.Public},
	}
}

func textOf(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

func (r *sendRig) edit(attempt model.ID, t *model.ExternalRef, text string) (adapter.Outcome, error) {
	return r.inst.(adapter.Editor).Edit(r.t.Context(), adapter.EditRequest{Effect: mutEffect(attempt, t), Content: textOf(text)})
}

func (r *sendRig) delete(t *model.ExternalRef) (adapter.Outcome, error) {
	return r.inst.(adapter.Deleter).Delete(r.t.Context(), adapter.DeleteRequest{Effect: mutEffect(tryOne, t)})
}

func (r *sendRig) react(t *model.ExternalRef, emoji string, remove bool) (adapter.Outcome, error) {
	return r.inst.(adapter.Reactor).React(r.t.Context(), adapter.ReactRequest{Effect: mutEffect(tryOne, t), Emoji: emoji, Remove: remove})
}

func (r *sendRig) reconcileMutation(c model.Capability, t *model.ExternalRef, content model.Content, emoji string, remove bool) (adapter.Outcome, error) {
	return r.inst.(adapter.Reconciler).Reconcile(r.t.Context(), adapter.ReconcileRequest{
		Effect: mutEffect(tryOne, t), Capability: c, Content: content, Emoji: emoji, Remove: remove,
	})
}

// own stores a comment the channel's account wrote for attempt.
func (r *sendRig) own(attempt model.ID) fakeComment {
	r.t.Helper()
	out, err := r.send(attempt)
	if err != nil || out.State != model.ProviderAccepted {
		r.t.Fatalf("Send = %+v, %v", out, err)
	}
	stored := r.gh.stored("a", issueKey)
	return stored[len(stored)-1]
}

func wantRejected(t *testing.T, out adapter.Outcome, err error, code model.ErrorCode) {
	t.Helper()
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != code || out.Failure.Retry != model.NoRetry || out.Binding != "" {
		t.Fatalf("outcome = %+v, %v; want a final rejection %s naming no work item binding", out, err, code)
	}
}

func (r *sendRig) writes() []fakeCall {
	var out []fakeCall
	for _, c := range r.gh.calls("a") {
		if c.Method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

func TestEditCommentCarriesMarker(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	out, err := r.edit(tryTwo, target(c.ID), "Corrected answer.")
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 0 {
		t.Fatalf("Edit = %+v, %v; want provider_accepted without references", out, err)
	}
	patches := r.gh.callsWith("a", http.MethodPatch)
	if len(patches) != 1 || patches[0].Path != "/repos/example/uma-sandbox/issues/comments/"+strconv.FormatInt(c.ID, 10) || !patches[0].Applied || patches[0].Auth != "Bearer "+apiToken {
		t.Fatalf("patches = %+v, want one applied comment update", patches)
	}
	got := r.gh.stored("a", issueKey)[0].Body
	want := "Corrected answer.\n\n<!-- uma:" + string(opA.ID) + ":" + string(tryTwo) + " -->"
	if got != want {
		t.Fatalf("body = %q, want the replacement text and the edit attempt's marker %q", got, want)
	}
}

func TestEditOthersCommentIsSent(t *testing.T) {
	r := newSendRig(t)
	other := r.gh.seed("a", issueKey, 5000001, "Still happens on 1.4.2.", t0)
	out, err := r.edit(tryOne, target(other.ID), "Moderated.")
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit of another user's comment = %+v, %v; want it sent: the provider decides", out, err)
	}
	if got := r.gh.stored("a", issueKey)[0]; !strings.HasPrefix(got.Body, "Moderated.") || got.UserID != 5000001 {
		t.Fatalf("stored = %+v", got)
	}
	// Only comments, never a pre-check: no read before the write.
	if gets := r.gh.callsWith("a", http.MethodGet); len(gets) != 0 {
		t.Fatalf("reads before the edit = %+v, want none", gets)
	}
}

func TestForbiddenEditIsRejectedNotReposted(t *testing.T) {
	r := newSendRig(t)
	other := r.gh.seed("a", issueKey, 5000001, "theirs", t0)
	r.gh.setOwnEditsOnly("a", true)
	out, err := r.edit(tryOne, target(other.ID), "mine now")
	wantRejected(t, out, err, model.Unauthorized)
	if out.Binding != "" {
		t.Fatalf("binding = %q, want none: the issue is not gone", out.Binding)
	}
	r.gh.setOwnEditsOnly("a", false)
	r.gh.respond("a", statusAnswer(404, `{"message":"Not Found","documentation_url":"`+commentsDocsURL+`","status":"404"}`, nil))
	out, err = r.edit(tryTwo, target(other.ID), "mine now")
	wantRejected(t, out, err, model.NotFound)
	for _, c := range r.writes() {
		if c.Method == http.MethodPost {
			t.Fatalf("calls %+v: a refused edit was re-posted", r.writes())
		}
	}
	if got := r.gh.stored("a", issueKey); len(got) != 1 || got[0].Body != "theirs" {
		t.Fatalf("stored = %+v, want the comment untouched and no new one", got)
	}
}

func TestEditIssueDescription(t *testing.T) {
	r := newSendRig(t)
	r.gh.setIssue("a", sandboxRepo, 7, 5000001, "open", nil, t0)
	out, err := r.edit(tryOne, descriptionTarget(), "New description.")
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	patches := r.gh.callsWith("a", http.MethodPatch)
	if len(patches) != 1 || patches[0].Path != "/repos/example/uma-sandbox/issues/7" || !strings.Contains(patches[0].Body, `"body":"New description.\n\n<!-- uma:`) {
		t.Fatalf("patches = %+v, want the issue body updated with the marker", patches)
	}
}

func TestEditRefusesWhatGitHubCannotCarry(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	n := len(r.gh.calls("a"))
	card := model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: "org.example.card", Version: 1}}}}
	out, err := r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: mutEffect(tryTwo, target(c.ID)), Content: card})
	wantRejected(t, out, err, model.Unsupported)
	internal := mutEffect(tryTwo, target(c.ID))
	internal.Audience.Visibility = model.Internal
	out, err = r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: internal, Content: textOf("x")})
	wantRejected(t, out, err, model.Unsupported)
	_, err = r.edit(tryTwo, nil, "x")
	wantCode(t, err, model.Validation)
	foreign := commentRef(chanA, c.ID)
	foreign.Channel.ID = "00000000-0000-7000-8000-00000000c0b2"
	_, err = r.edit(tryTwo, &foreign, "x")
	wantCode(t, err, model.Validation)
	if got := len(r.gh.calls("a")); got != n {
		t.Fatalf("provider calls = %d, want none after the refusals (had %d)", got-n, n)
	}
}

func TestDeleteComment(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	out, err := r.delete(target(c.ID))
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
	dels := r.gh.callsWith("a", http.MethodDelete)
	if len(dels) != 1 || dels[0].Path != "/repos/example/uma-sandbox/issues/comments/"+strconv.FormatInt(c.ID, 10) || !dels[0].Applied {
		t.Fatalf("deletes = %+v", dels)
	}
	if got := r.gh.stored("a", issueKey); len(got) != 0 {
		t.Fatalf("stored = %+v, want the comment gone", got)
	}
	out, err = r.delete(target(c.ID))
	wantRejected(t, out, err, model.NotFound)
}

func TestDeleteIssueBodyRejected(t *testing.T) {
	r := newSendRig(t)
	out, err := r.delete(descriptionTarget())
	wantRejected(t, out, err, model.Unsupported)
	if n := len(r.gh.calls("a")); n != 0 {
		t.Fatalf("provider calls = %d, want none: an issue description cannot be deleted", n)
	}
}

func TestForbiddenDeleteIsRejected(t *testing.T) {
	r := newSendRig(t)
	other := r.gh.seed("a", issueKey, 5000001, "theirs", t0)
	r.gh.setOwnEditsOnly("a", true)
	out, err := r.delete(target(other.ID))
	wantRejected(t, out, err, model.Unauthorized)
	if got := r.gh.stored("a", issueKey); len(got) != 1 {
		t.Fatalf("stored = %+v, want the comment kept", got)
	}
}

var reactionContents = []struct{ emoji, content string }{
	{"👍", "+1"}, {"👎", "-1"}, {"😄", "laugh"}, {"😕", "confused"}, {"❤️", "heart"}, {"🎉", "hooray"}, {"🚀", "rocket"}, {"👀", "eyes"},
}

func TestReactMapsEmojiToContent(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	key := "comment:" + sandboxRepo + "#" + strconv.FormatInt(c.ID, 10)
	for _, rc := range reactionContents {
		out, err := r.react(target(c.ID), rc.emoji, false)
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
			t.Fatalf("React %s = %+v, %v", rc.emoji, out, err)
		}
		posts := r.gh.callsWith("a", http.MethodPost)
		last := posts[len(posts)-1]
		if last.Path != "/repos/example/uma-sandbox/issues/comments/"+strconv.FormatInt(c.ID, 10)+"/reactions" || strings.TrimSpace(last.Body) != `{"content":"`+rc.content+`"}` {
			t.Fatalf("%s sent %+v, want content %q", rc.emoji, last, rc.content)
		}
		ref := out.References[0]
		if ref.Channel != chanA || ref.Namespace != github.Namespace || ref.Kind != "reaction" || ref.ID != strconv.FormatInt(last.Reaction, 10) || ref.ParentID != "comment:"+strconv.FormatInt(c.ID, 10) {
			t.Fatalf("reference = %+v, want the reaction %d on the comment", ref, last.Reaction)
		}
		if err := ref.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(r.gh.reactionsOn("a", key)); n != len(reactionContents) {
		t.Fatalf("stored reactions = %d, want one per content", n)
	}
	// Adding a reaction the account already holds is GitHub's 200, still applied.
	out, err := r.react(target(c.ID), "👍", false)
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("repeated React = %+v, %v", out, err)
	}
	before := len(r.gh.calls("a"))
	for _, emoji := range []string{"🤷", "👍🏽", "❤", "+1"} {
		out, err := r.react(target(c.ID), emoji, false)
		wantRejected(t, out, err, model.Unsupported)
	}
	if got := len(r.gh.calls("a")); got != before {
		t.Fatalf("provider calls for unmapped emoji = %d, want none", got-before)
	}
}

func TestReactOnIssueDescription(t *testing.T) {
	r := newSendRig(t)
	out, err := r.react(descriptionTarget(), "🚀", false)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("React = %+v, %v", out, err)
	}
	posts := r.gh.callsWith("a", http.MethodPost)
	if len(posts) != 1 || posts[0].Path != "/repos/example/uma-sandbox/issues/7/reactions" || strings.TrimSpace(posts[0].Body) != `{"content":"rocket"}` {
		t.Fatalf("posts = %+v", posts)
	}
	if ref := out.References[0]; ref.ParentID != "issue:3000000007" {
		t.Fatalf("reference = %+v, want it on the description", ref)
	}
}

func TestReactRemoveFindsOwnReaction(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	key := "comment:" + sandboxRepo + "#" + strconv.FormatInt(c.ID, 10)
	theirs := r.gh.seedReaction("a", key, 5000001, "+1")
	otherContent := r.gh.seedReaction("a", key, botID, "heart")
	mine := r.gh.seedReaction("a", key, botID, "+1")
	out, err := r.react(target(c.ID), "👍", true)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("React remove = %+v, %v", out, err)
	}
	dels := r.gh.callsWith("a", http.MethodDelete)
	if len(dels) != 1 || dels[0].Reaction != mine.ID || !strings.HasSuffix(dels[0].Path, "/reactions/"+strconv.FormatInt(mine.ID, 10)) {
		t.Fatalf("deletes = %+v, want only the account's own +1", dels)
	}
	left := r.gh.reactionsOn("a", key)
	if len(left) != 2 || left[0].ID != theirs.ID || left[1].ID != otherContent.ID {
		t.Fatalf("reactions left = %+v, want the other user's +1 and the account's heart", left)
	}
	var body string
	for _, c := range r.gh.callsWith("a", http.MethodGet) {
		body = c.Query
	}
	if !strings.Contains(body, "content=%2B1") {
		t.Fatalf("lookup query = %q, want it narrowed to the content", body)
	}

	// Absent: nothing to remove is applied, with no write.
	before := len(r.writes())
	out, err = r.react(target(c.ID), "👍", true)
	if err != nil || out.State != model.ProviderAccepted || len(r.writes()) != before {
		t.Fatalf("React remove of an absent reaction = %+v, %v (writes %d -> %d), want an applied no-op", out, err, before, len(r.writes()))
	}
}

func TestReactRemoveReadsEveryPage(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	key := "comment:" + sandboxRepo + "#" + strconv.FormatInt(c.ID, 10)
	for range 120 {
		r.gh.seedReaction("a", key, 5000001, "eyes")
	}
	mine := r.gh.seedReaction("a", key, botID, "eyes")
	out, err := r.react(target(c.ID), "👀", true)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("React remove = %+v, %v", out, err)
	}
	if dels := r.gh.callsWith("a", http.MethodDelete); len(dels) != 1 || dels[0].Reaction != mine.ID {
		t.Fatalf("deletes = %+v, want the account's reaction on the second page", dels)
	}
}

func TestReactRefusals(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	r.gh.respond("a", statusAnswer(403, `{"message":"Resource not accessible by integration","documentation_url":"`+reactionsDocsURL+`","status":"403"}`, nil))
	out, err := r.react(target(c.ID), "👍", false)
	wantRejected(t, out, err, model.Unauthorized)
	out, err = r.react(target(c.ID+999), "👍", false)
	wantRejected(t, out, err, model.NotFound)
	// The lookup of a removal is a read, and GitHub's refusal of it
	// rejects the removal.
	out, err = r.react(target(c.ID+999), "👍", true)
	wantRejected(t, out, err, model.NotFound)
}

func TestMutationsDisabledWithoutReplies(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	inst := r.open(github.Config{APIURL: r.gh.apiURL("a"), AccountID: botID})
	st, err := inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[model.Capability]model.CapabilityStatus{}
	for _, s := range st {
		seen[s.Name] = s
	}
	for _, name := range []model.Capability{model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact} {
		if s, ok := seen[name]; !ok || s.Available || s.Reason != model.Disabled {
			t.Errorf("capability %s = %+v, want unavailable with reason disabled", name, s)
		}
	}
	n := len(r.gh.calls("a"))
	e := mutEffect(tryOne, target(c.ID))
	_, err = inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: e, Content: textOf("x")})
	wantCode(t, err, model.Disabled)
	_, err = inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: e})
	wantCode(t, err, model.Disabled)
	_, err = inst.(adapter.Reactor).React(t.Context(), adapter.ReactRequest{Effect: e, Emoji: "👍"})
	wantCode(t, err, model.Disabled)
	if got := len(r.gh.calls("a")); got != n {
		t.Fatalf("provider calls = %d, want none while replies are off", got-n)
	}
	on, err := r.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range on {
		if !s.Available {
			t.Errorf("capability %+v with replies on, want available", s)
		}
	}
}

func TestDescriptorDeclaresMutations(t *testing.T) {
	d := github.NewFactory(github.Options{}).Describe()
	for _, c := range []model.Capability{model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact} {
		found := false
		for _, have := range d.Capabilities {
			found = found || have == c
		}
		if !found {
			t.Errorf("capability %s not declared", c)
		}
	}
	if d.Exchange.Reactions != adapter.ReactionsMultiple {
		t.Errorf("reactions cardinality = %q, want multiple: GitHub allows several contents per user", d.Exchange.Reactions)
	}
}

func TestMutationOutcomeMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer answer
		state  model.DeliveryState
		code   model.ErrorCode
		retry  model.RetryAdvice
	}{
		{name: "422 rejected", answer: statusAnswer(422, `{"message":"Validation Failed","status":"422"}`, nil), state: model.Rejected, code: model.Validation, retry: model.NoRetry},
		{name: "410 gone", answer: statusAnswer(410, `{"message":"Gone","status":"410"}`, nil), state: model.Rejected, code: model.NotFound, retry: model.NoRetry},
		{name: "500 unknown", answer: statusAnswer(500, `{"message":"Server Error"}`, nil), code: model.OutcomeUnknown, retry: model.ReconcileFirst},
		{name: "answer lost", answer: answerLost, code: model.OutcomeUnknown, retry: model.ReconcileFirst},
		{name: "unreachable", answer: unreachable, code: model.Transient, retry: model.RetrySameOperation},
		{name: "429", answer: statusAnswer(429, `{"message":"slow down"}`, map[string]string{"Retry-After": "30"}), code: model.RateLimited, retry: model.RetrySameOperation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, run := range map[string]func(*sendRig, int64) (adapter.Outcome, error){
				"edit":   func(r *sendRig, id int64) (adapter.Outcome, error) { return r.edit(tryTwo, target(id), "x") },
				"delete": func(r *sendRig, id int64) (adapter.Outcome, error) { return r.delete(target(id)) },
				"react":  func(r *sendRig, id int64) (adapter.Outcome, error) { return r.react(target(id), "👍", false) },
			} {
				r := newSendRig(t)
				c := r.own(tryOne)
				r.gh.respond("a", tc.answer)
				out, err := run(r, c.ID)
				if tc.state != "" {
					if err != nil || out.State != tc.state || out.Failure == nil || out.Failure.Code != tc.code || out.Failure.Retry != tc.retry {
						t.Fatalf("%s = %+v, %v; want %s %s", name, out, err, tc.state, tc.code)
					}
					continue
				}
				if me := wantCode(t, err, tc.code); me.Retry != tc.retry {
					t.Fatalf("%s error = %+v, want retry %s", name, me, tc.retry)
				}
			}
		})
	}
}

func TestReconcileEdit(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	content := textOf("Corrected answer.")
	r.gh.respond("a", answerLost)
	if _, err := r.edit(tryTwo, target(c.ID), "Corrected answer."); model.CodeOf(err) != model.OutcomeUnknown {
		t.Fatalf("Edit = %v, want outcome unknown", err)
	}
	e := mutEffect(tryTwo, target(c.ID))
	out, err := r.inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: e, Capability: model.CapabilityEdit, Content: content})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile = %+v, %v; want the edit found by its marker", out, err)
	}
	// An attempt that never reached GitHub leaves another marker.
	e.Attempt = "00000000-0000-7000-8000-00000000b0b3"
	out, err = r.inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: e, Capability: model.CapabilityEdit, Content: content})
	if err != nil || out.State != model.Failed || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("Reconcile of an unapplied attempt = %+v, %v; want failed not_found", out, err)
	}
	gone := r.gh.seed("a", issueKey, botID, "x", t0)
	if out, err := r.delete(target(gone.ID)); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
	out, err = r.inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: mutEffect(tryTwo, target(gone.ID)), Capability: model.CapabilityEdit, Content: content})
	wantRejected(t, out, err, model.NotFound)
	for _, w := range r.writes() {
		if w.Method == http.MethodPatch && w.Comment == gone.ID {
			t.Fatalf("reconciliation wrote %+v", w)
		}
	}
}

func TestReconcileDelete(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	out, err := r.reconcileMutation(model.CapabilityDelete, target(c.ID), model.Content{}, "", false)
	if err != nil || out.State != model.Failed || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("Reconcile while the comment exists = %+v, %v; want failed not_found so a retry is safe", out, err)
	}
	r.gh.respond("a", answerLost)
	if _, err := r.delete(target(c.ID)); model.CodeOf(err) != model.OutcomeUnknown {
		t.Fatalf("Delete = %v", err)
	}
	out, err = r.reconcileMutation(model.CapabilityDelete, target(c.ID), model.Content{}, "", false)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile after the delete = %+v, %v; want applied: a 404 is the deleted state", out, err)
	}
}

func TestReconcileReaction(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	key := "comment:" + sandboxRepo + "#" + strconv.FormatInt(c.ID, 10)
	out, err := r.reconcileMutation(model.CapabilityReact, target(c.ID), model.Content{}, "👍", false)
	if err != nil || out.State != model.Failed || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("Reconcile add, absent = %+v, %v; want failed not_found", out, err)
	}
	out, err = r.reconcileMutation(model.CapabilityReact, target(c.ID), model.Content{}, "👍", true)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile remove, absent = %+v, %v; want applied", out, err)
	}
	r.gh.seedReaction("a", key, 5000001, "+1")
	out, err = r.reconcileMutation(model.CapabilityReact, target(c.ID), model.Content{}, "👍", false)
	if err != nil || out.State != model.Failed {
		t.Fatalf("Reconcile add, only another user's = %+v, %v; want failed", out, err)
	}
	mine := r.gh.seedReaction("a", key, botID, "+1")
	out, err = r.reconcileMutation(model.CapabilityReact, target(c.ID), model.Content{}, "👍", false)
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0].ID != strconv.FormatInt(mine.ID, 10) {
		t.Fatalf("Reconcile add, present = %+v, %v; want applied naming the reaction", out, err)
	}
	out, err = r.reconcileMutation(model.CapabilityReact, target(c.ID), model.Content{}, "👍", true)
	if err != nil || out.State != model.Failed {
		t.Fatalf("Reconcile remove, present = %+v, %v; want failed so a retry is safe", out, err)
	}
	if w := r.writes(); len(w) != 1 {
		t.Fatalf("writes = %+v, want only the send: reconciliation reads", w)
	}
}
