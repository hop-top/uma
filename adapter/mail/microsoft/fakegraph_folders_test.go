package microsoft

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// fakeFolder is one node of the mailbox's folder tree.
type fakeFolder struct {
	id, name, parent string // parent "" is the root
}

// treeLocked returns the tree, starting it with the well-known folders at
// the root. The caller holds g.mu.
func (mb *fakeMailbox) treeLocked() []*fakeFolder {
	if mb.tree == nil {
		for _, n := range []string{"archive", "deleteditems", "drafts", "inbox", "junkemail", "sentitems"} {
			mb.tree = append(mb.tree, &fakeFolder{id: folderID(n), name: folderDisplay(n)})
		}
	}
	return mb.tree
}

// mailboxObjects answers the folder list and create calls (root and
// childFolders) and the master categories.
func (g *fakeGraph) mailboxObjects(w http.ResponseWriter, r *http.Request, mb *fakeMailbox, rest string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if rest == "/outlook/masterCategories" {
		switch r.Method {
		case "GET":
			writeJSON(w, http.StatusOK, gCategoryPage{Value: append([]gCategory{}, mb.cats...)})
		case "POST":
			var c gCategory
			if json.NewDecoder(r.Body).Decode(&c) != nil || strings.TrimSpace(c.DisplayName) == "" {
				graphError(w, http.StatusBadRequest, "ErrorInvalidRequest")
				return
			}
			for _, x := range mb.cats {
				if strings.EqualFold(x.DisplayName, c.DisplayName) {
					graphError(w, http.StatusConflict, "ErrorCategoryExists")
					return
				}
			}
			mb.fseq++
			c.ID = "cat-" + strconv.Itoa(mb.fseq)
			mb.cats = append(mb.cats, c)
			writeJSON(w, http.StatusCreated, c)
		}
		return
	}
	parent := ""
	if p, ok := strings.CutSuffix(rest, "/childFolders"); ok {
		parent, _ = url.PathUnescape(strings.TrimPrefix(p, "/mailFolders/"))
		if !g.hasFolder(mb, parent) {
			graphError(w, http.StatusNotFound, "ErrorItemNotFound")
			return
		}
	}
	switch r.Method {
	case "GET":
		var kids []gFolder
		for _, f := range mb.treeLocked() {
			if f.parent == parent {
				n := 0
				for _, c := range mb.tree {
					if c.parent == f.id {
						n++
					}
				}
				kids = append(kids, gFolder{ID: f.id, DisplayName: f.name, ChildFolderCount: n})
			}
		}
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skiptoken"))
		if skip > len(kids) {
			skip = len(kids)
		}
		kids = kids[skip:]
		page := gFolderPage{}
		if n := mb.pageSize; n > 0 && len(kids) > n {
			q := r.URL.Query()
			q.Set("$skiptoken", strconv.Itoa(skip+n))
			page.NextLink = g.srv.URL + "/v1.0/users/" + mb.name + rest + "?" + encodeQuery(q)
			kids = kids[:n]
		}
		page.Value = kids
		writeJSON(w, http.StatusOK, page)
	case "POST":
		var req gNewFolder
		if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.DisplayName) == "" {
			graphError(w, http.StatusBadRequest, "ErrorInvalidRequest")
			return
		}
		for _, f := range mb.treeLocked() {
			if f.parent == parent && strings.EqualFold(f.name, req.DisplayName) {
				graphError(w, http.StatusConflict, "ErrorFolderExists")
				return
			}
		}
		mb.fseq++
		f := &fakeFolder{id: "fld-" + strconv.Itoa(mb.fseq), name: req.DisplayName, parent: parent}
		mb.tree = append(mb.tree, f)
		writeJSON(w, http.StatusCreated, gFolder{ID: f.id, DisplayName: f.name})
	}
}

func (g *fakeGraph) hasFolder(mb *fakeMailbox, id string) bool {
	for _, f := range mb.treeLocked() {
		if f.id == id {
			return true
		}
	}
	return false
}

// addFolder puts a folder under parent ("" for the root) and returns its ID.
func (g *fakeGraph) addFolder(mb *fakeMailbox, parent, name string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	mb.treeLocked()
	mb.fseq++
	f := &fakeFolder{id: "fld-" + strconv.Itoa(mb.fseq), name: name, parent: parent}
	mb.tree = append(mb.tree, f)
	return f.id
}

// pathOf is the "/"-joined path of folder id.
func (g *fakeGraph) pathOf(mb *fakeMailbox, id string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var parts []string
	for id != "" {
		var next string
		for _, f := range mb.treeLocked() {
			if f.id == id {
				parts = append([]string{f.name}, parts...)
				next = f.parent
			}
		}
		id = next
	}
	return strings.Join(parts, "/")
}
