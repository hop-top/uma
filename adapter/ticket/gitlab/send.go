package gitlab

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// ReconcileWindow bounds how far back Reconcile looks for an attempt's
// note: notes created earlier are not read.
const ReconcileWindow = 7 * 24 * time.Hour

// maxNoteRunes is GitLab's note body limit (1,000,000 characters).
const maxNoteRunes = 1_000_000

// maxDescriptionRunes is GitLab's issue description limit (1,048,576
// characters).
const maxDescriptionRunes = 1_048_576

// reconcilePages bounds the note pages one reconciliation reads; past it the
// result is inconclusive.
const reconcilePages = 20

// issueRoute is a parsed work item reference: the numeric project ID and the
// issue's number within the project (iid).
type issueRoute struct {
	project int64
	iid     int64
}

// workItemRef returns the reference of issue iid in project on ch.
func workItemRef(ch model.ChannelRef, project, iid int64) model.ExternalRef {
	return model.ExternalRef{
		Channel: ch, Namespace: Namespace, Kind: model.RefKindWorkItem,
		ID: strconv.FormatInt(iid, 10), ParentID: strconv.FormatInt(project, 10),
	}
}

// parseRoute checks that r is a work item reference of the instance's
// channel and returns the issue it names.
func (x *Instance) parseRoute(r model.ExternalRef) (issueRoute, error) {
	bad := final(model.Validation, "route is not a GitLab issue of this channel")
	if r.Channel != x.channel.Ref || r.Namespace != Namespace || r.Kind != model.RefKindWorkItem {
		return issueRoute{}, bad
	}
	iid, ok := canonicalInt(r.ID)
	project, ok2 := canonicalInt(r.ParentID)
	if !ok || !ok2 {
		return issueRoute{}, bad
	}
	return issueRoute{project: project, iid: iid}, nil
}

// canonicalInt parses a positive decimal integer without sign or padding.
func canonicalInt(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0 && strconv.FormatInt(n, 10) == s
}

func (r issueRoute) issuePath() string {
	return "projects/" + strconv.FormatInt(r.project, 10) + "/issues/" + strconv.FormatInt(r.iid, 10)
}

func (r issueRoute) notesPath() string { return r.issuePath() + "/notes" }

func (r issueRoute) notePath(id int64) string { return r.notesPath() + "/" + strconv.FormatInt(id, 10) }

// Echo marker: the last line of a note uma posts, an HTML comment GitLab
// does not render, naming the operation and attempt.
const (
	markerOpen  = "<!-- uma:"
	markerClose = " -->"
)

func marker(op model.OperationRef, attempt model.ID) string {
	return markerOpen + string(op.ID) + ":" + string(attempt) + markerClose
}

// parseMarker returns the operation and attempt of a body's trailing marker
// and the body without it.
func parseMarker(body string) (op, attempt model.ID, rest string, ok bool) {
	trimmed := strings.TrimRight(body, " \t\r\n")
	i := strings.LastIndex(trimmed, markerOpen)
	if i < 0 || !strings.HasSuffix(trimmed, markerClose) {
		return "", "", body, false
	}
	inner := trimmed[i+len(markerOpen) : len(trimmed)-len(markerClose)]
	a, b, found := strings.Cut(inner, ":")
	opID, err1 := model.ParseID(a)
	attID, err2 := model.ParseID(b)
	if !found || err1 != nil || err2 != nil {
		return "", "", body, false
	}
	return opID, attID, strings.TrimRight(trimmed[:i], " \t\r\n"), true
}

// bodyText is body without an echo marker.
func bodyText(body string) string {
	if _, _, rest, ok := parseMarker(body); ok {
		return rest
	}
	return body
}

// escapeQuickActions keeps a reply from running as GitLab quick actions.
// GitLab executes and removes any line of a note or description that starts
// with "/" and a command ("/close", "/assign @user", "/label ~x"), so
// reply text a person did not mean as commands would change the issue. A
// backslash before the slash is a Markdown escape: the line is stored as
// written and shown without the backslash.
func escapeQuickActions(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		t := strings.TrimLeft(l, " \t")
		if len(t) > 1 && t[0] == '/' && isLetter(t[1]) {
			lines[i] = l[:len(l)-len(t)] + `\` + t
		}
	}
	return strings.Join(lines, "\n")
}

func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// renderBody returns the note body for content: its text parts, with
// quick actions escaped, then the echo marker. Other parts are unsupported.
// limit is the target's character limit.
func renderBody(c model.Content, op model.OperationRef, attempt model.ID, limit int) (string, error) {
	var parts []string
	for _, p := range c.Parts {
		if p.Kind != model.PartText {
			return "", final(model.Unsupported, "GitLab notes carry text only")
		}
		parts = append(parts, p.Text)
	}
	if len(parts) == 0 {
		return "", final(model.Validation, "content has no text")
	}
	body := escapeQuickActions(strings.Join(parts, "\n\n")) + "\n\n" + marker(op, attempt)
	if utf8.RuneCountInString(body) > limit {
		return "", final(model.Validation, "text exceeds GitLab's body limit")
	}
	return body, nil
}

func noteMessageID(id int64) string  { return "note:" + strconv.FormatInt(id, 10) }
func issueMessageID(id int64) string { return "issue:" + strconv.FormatInt(id, 10) }

// Send posts content as a note on the issue the route names. Only public
// audiences are sent: an internal note is not sent by this adapter.
func (x *Instance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if !x.cfg.Replies {
		return adapter.Outcome{}, final(model.Disabled, "replies are not enabled for the channel")
	}
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	route, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return adapter.Outcome{}, final(model.Unsupported, "GitLab replies are public notes")
	}
	body, err := renderBody(r.Content, r.Operation, r.Attempt, maxNoteRunes)
	if err != nil {
		return adapter.Outcome{}, err
	}
	a, err := x.call(ctx, spec{method: http.MethodPost, path: route.notesPath(), body: map[string]any{"body": body}})
	if err != nil || !a.ok() {
		return x.failure(ctx, a, err, noteCall)
	}
	n, ok := decodeNote(a.body)
	if !ok {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "GitLab answer has no note ID; reconcile before retrying")
	}
	return x.accepted(n.ID), nil
}

func (x *Instance) accepted(id int64) adapter.Outcome {
	return adapter.Outcome{
		State:      model.ProviderAccepted,
		References: []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, noteMessageID(id))},
	}
}

// Reconcile resolves an attempt whose answer was lost. A work item act
// (req.Status set) is resolved by reading the issue; see reconcileAct. An
// edit or delete (req.Capability) is resolved by reading the target; see
// reconcileMutation.
//
// For a reply it looks for the note of the attempt: a note by the channel's
// account, created within ReconcileWindow, whose marker names the attempt's
// operation and attempt. Found is provider_accepted with its reference; a
// complete listing without it is failed with not_found, so a retry is safe;
// a listing cut short is inconclusive (state unknown).
func (x *Instance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	if req.Status != "" {
		return x.reconcileAct(ctx, req)
	}
	switch req.Capability {
	case model.CapabilityEdit, model.CapabilityDelete:
		return x.reconcileMutation(ctx, req)
	case "", model.CapabilitySend:
	default:
		return adapter.Outcome{}, final(model.Unsupported, "reconciliation of this capability is not supported")
	}
	e := req.Effect
	if !x.cfg.Replies {
		return adapter.Outcome{}, final(model.Disabled, "replies are not enabled for the channel")
	}
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	route, err := x.parseRoute(e.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	since := x.clock.Now().Add(-ReconcileWindow).UTC()
	page := 1
	for range reconcilePages {
		a, err := x.call(ctx, spec{method: http.MethodGet, path: route.notesPath(), query: url.Values{
			"sort": {"desc"}, "order_by": {"created_at"}, "per_page": {"100"}, "page": {strconv.Itoa(page)},
		}})
		if err != nil || !a.ok() {
			return adapter.Outcome{}, x.readFailure(ctx, a, err)
		}
		ns, ok := decodeNotes(a.body)
		if !ok {
			return adapter.Outcome{}, model.NewError(model.Transient, "GitLab answer is not a list of notes")
		}
		windowEnd := false
		for _, n := range ns {
			if n.System {
				continue
			}
			if parseTime(n.CreatedAt).Before(since) {
				// Newest first: every later note is older still.
				windowEnd = true
				break
			}
			if x.ownNote(n) {
				if op, att, _, ok := parseMarker(n.Body); ok && op == e.Operation.ID && att == e.Attempt {
					return x.accepted(n.ID), nil
				}
			}
		}
		next, _ := strconv.Atoi(a.header.Get("X-Next-Page"))
		if windowEnd || next == 0 {
			return adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "no note recorded for the attempt")}, nil
		}
		page = next
	}
	return adapter.Outcome{State: model.Unknown}, nil
}

// ownNote reports whether n was written by the channel's account.
func (x *Instance) ownNote(n apiNote) bool {
	return x.cfg.AccountID != 0 && n.Author.ID == x.cfg.AccountID
}
