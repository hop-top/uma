package google

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// labelsAPI answers users.labels.list and users.labels.create: the system
// labels and the user's own; a create of a name that exists, in any case, is
// 409 like Gmail's "Label name exists or conflicts", and an empty name is
// 400.
func (g *fakeGmail) labelsAPI(w http.ResponseWriter, r *http.Request, b *fakeBox) {
	g.mu.Lock()
	defer g.mu.Unlock()
	all := func() []gLabel {
		var out []gLabel
		for _, id := range systemLabels {
			out = append(out, gLabel{ID: id, Name: id, Type: "system"})
		}
		return append(out, b.userLabels...)
	}
	switch r.Method {
	case "GET":
		writeJSON(w, http.StatusOK, gLabelList{Labels: all()})
	case "POST":
		var req gLabelCreate
		if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.Name) == "" {
			googleError(w, http.StatusBadRequest, "invalidArgument", "INVALID_ARGUMENT")
			return
		}
		for _, l := range all() {
			if strings.EqualFold(l.Name, req.Name) {
				googleError(w, http.StatusConflict, "duplicate", "ALREADY_EXISTS")
				return
			}
		}
		b.labelSeq++
		l := gLabel{ID: "Label_" + strconv.Itoa(b.labelSeq), Name: req.Name, Type: "user"}
		b.userLabels = append(b.userLabels, l)
		writeJSON(w, http.StatusOK, l)
	default:
		googleError(w, http.StatusMethodNotAllowed, "invalidArgument", "INVALID_ARGUMENT")
	}
}

// addUserLabel gives the mailbox a user label.
func (g *fakeGmail) addUserLabel(b *fakeBox, name string) gLabel {
	g.mu.Lock()
	defer g.mu.Unlock()
	b.labelSeq++
	l := gLabel{ID: "Label_" + strconv.Itoa(b.labelSeq), Name: name, Type: "user"}
	b.userLabels = append(b.userLabels, l)
	return l
}
