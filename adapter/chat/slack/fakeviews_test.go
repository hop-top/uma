package slack

import (
	"strconv"
	"time"
)

// The view methods of the fake workspace answer as the references state
// (https://docs.slack.dev/reference/methods/views.open/, views.push and
// views.update): a trigger_id is accepted once and "expire[s] 3 seconds
// after it's sent to your app"
// (https://docs.slack.dev/surfaces/modals/), the error codes are the ones
// the pages list, a modal holds at most 3 views and a view is at most
// 250kb. The answer to a successful call is {"ok": true, "view": {"id",
// "hash", ...}}.

// fakeViews is an account's modals and the trigger_ids Slack sent.
type fakeViews struct {
	triggers map[string]*fakeTrigger
	views    map[string]*fakeView
	seq      int
	// anyTrigger accepts a trigger_id nobody issued as one sent just now:
	// a suite that cannot know the ones its fake sends.
	anyTrigger bool
}

type fakeTrigger struct {
	issued time.Time
	// from is the view a trigger_id from inside a modal belongs to.
	from string
	used bool
}

type fakeView struct {
	id, externalID, hash string
	root                 string
	view                 map[string]any
}

// trigger registers a trigger_id Slack sent at issued, from view when it
// came from inside a modal.
func (a *fakeAccount) trigger(id string, issued time.Time, from string) {
	if a.views.triggers == nil {
		a.views.triggers = map[string]*fakeTrigger{}
	}
	a.views.triggers[id] = &fakeTrigger{issued: issued, from: from}
}

// view returns the modal view by ID, nil when none.
func (a *fakeAccount) view(id string) *fakeView { return a.views.views[id] }

func (f *fakeSlack) viewMethod(a *fakeAccount, method string, in map[string]any, call *fakeCall, apply bool) (map[string]any, bool) {
	view, _ := in["view"].(map[string]any)
	if view == nil || view["type"] != "modal" {
		return apiFail("invalid_arguments"), false
	}
	if len(call.Body) > 250<<10 {
		return apiFail("view_too_large"), false
	}
	if method == "views.update" {
		return f.updateView(a, in, view, apply)
	}
	t := a.views.triggers[str(in, "trigger_id")]
	if t == nil && a.views.anyTrigger {
		t = &fakeTrigger{issued: f.now()}
		a.trigger(str(in, "trigger_id"), t.issued, "")
		t = a.views.triggers[str(in, "trigger_id")]
	}
	switch {
	case t == nil:
		return apiFail("invalid_trigger_id"), false
	case t.used:
		return apiFail("exchanged_trigger_id"), false
	case !f.now().Before(t.issued.Add(3 * time.Second)):
		return apiFail("expired_trigger_id"), false
	}
	if ext := str(view, "external_id"); ext != "" && a.byExternal(ext) != nil {
		return apiFail("duplicate_external_id"), false
	}
	root := ""
	if method == "views.push" {
		from := a.views.views[t.from]
		if from == nil {
			return apiFail("invalid_arguments"), false
		}
		root = from.root
		depth := 0
		for _, v := range a.views.views {
			if v.root == root {
				depth++
			}
		}
		if depth >= 3 {
			return apiFail("push_limit_reached"), false
		}
	}
	if !apply {
		return map[string]any{"ok": true}, false
	}
	t.used = true
	v := a.newView(view, root)
	return viewAnswer(v), true
}

func (f *fakeSlack) updateView(a *fakeAccount, in, view map[string]any, apply bool) (map[string]any, bool) {
	v := a.views.views[str(in, "view_id")]
	if v == nil {
		v = a.byExternal(str(in, "external_id"))
	}
	if v == nil {
		return apiFail("not_found"), false
	}
	if h := str(in, "hash"); h != "" && h != v.hash {
		return apiFail("hash_conflict"), false
	}
	if !apply {
		return map[string]any{"ok": true}, false
	}
	v.view = view
	if ext := str(view, "external_id"); ext != "" {
		v.externalID = ext
	}
	a.views.seq++
	v.hash = strconv.Itoa(a.views.seq) + ".hash"
	return viewAnswer(v), true
}

func (a *fakeAccount) byExternal(ext string) *fakeView {
	if ext == "" {
		return nil
	}
	for _, v := range a.views.views {
		if v.externalID == ext {
			return v
		}
	}
	return nil
}

func (a *fakeAccount) newView(view map[string]any, root string) *fakeView {
	if a.views.views == nil {
		a.views.views = map[string]*fakeView{}
	}
	a.views.seq++
	id := "V" + strconv.Itoa(1000+a.views.seq)
	if root == "" {
		root = id
	}
	v := &fakeView{id: id, externalID: str(view, "external_id"), hash: strconv.Itoa(a.views.seq) + ".hash", root: root, view: view}
	a.views.views[id] = v
	return v
}

func viewAnswer(v *fakeView) map[string]any {
	out := map[string]any{"id": v.id, "hash": v.hash, "type": "modal", "root_view_id": v.root}
	if v.externalID != "" {
		out["external_id"] = v.externalID
	}
	return map[string]any{"ok": true, "view": out}
}
