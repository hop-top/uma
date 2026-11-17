package github

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
)

// historyCursorPrefix starts every history cursor the adapter issues.
const historyCursorPrefix = "gh1."

// maxHistoryPage bounds the page number a cursor may name.
const maxHistoryPage = 1 << 20

// historyCursor is where a history read resumes. It names no URL: the
// adapter rebuilds every request from the channel's API root, so a cursor
// can steer no request elsewhere. Scope binds it to the route or
// repositories and the window start it was issued for.
type historyCursor struct {
	Scope uint64 `json:"k"`
	// PerPage is the page size of every listing of the read, fixed by its
	// first page so page numbers keep their meaning.
	PerPage int `json:"n"`
	// Description reports the issue description considered (an issue read
	// only).
	Description bool `json:"d,omitempty"`
	// Streams are the listings' positions, in scope order.
	Streams []streamPos `json:"s"`
}

// streamPos is a listing's position: the page holding the last item
// consumed (1 before any) and that item's ID (0 before any).
type streamPos struct {
	Page int   `json:"p"`
	Last int64 `json:"l"`
}

// historyScopeKey binds a cursor to what it reads.
func historyScopeKey(what string, since, until time.Time) uint64 {
	h := fnv.New64a()
	key := what + "\x00" + strconv.FormatInt(since.UTC().UnixNano(), 10)
	if !until.IsZero() {
		key += "\x00" + strconv.FormatInt(until.UTC().UnixNano(), 10)
	}
	_, _ = h.Write([]byte(key))
	return h.Sum64()
}

func (c historyCursor) encode() string {
	b, _ := json.Marshal(c)
	return historyCursorPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// decodeHistoryCursor parses s, a cursor issued for scope with streams
// listings. Anything else is model.Validation, before any provider call.
func decodeHistoryCursor(s string, scope uint64, streams int) (historyCursor, error) {
	bad := final(model.Validation, "history cursor not issued for this read")
	raw, ok := strings.CutPrefix(s, historyCursorPrefix)
	if !ok {
		return historyCursor{}, bad
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return historyCursor{}, bad
	}
	var c historyCursor
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || dec.More() || c.Scope != scope || c.PerPage < 1 || c.PerPage > historyPerPage || len(c.Streams) != streams {
		return historyCursor{}, bad
	}
	for _, p := range c.Streams {
		if p.Page < 1 || p.Page > maxHistoryPage || p.Last < 0 {
			return historyCursor{}, bad
		}
	}
	return c, nil
}
