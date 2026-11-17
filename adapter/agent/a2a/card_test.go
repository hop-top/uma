package a2a_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func (r *rig) get(path string, header ...string) reply {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodGet, r.srv.URL+path, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := r.srv.Client().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var buf [1 << 16]byte
	n, _ := res.Body.Read(buf[:])
	return reply{Status: res.StatusCode, Header: res.Header, Body: buf[:n]}
}

// canonical re-encodes JSON with sorted keys and no spacing.
func canonical(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G304 -- fixed testdata directory
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The card is served without authentication or a version header, at both
// paths under the mount, and is the shape of the specification's AgentCard.
func TestAgentCard(t *testing.T) {
	r := newRig(t)
	want := canonical(t, golden(t, "agent_card.json"))
	for _, path := range []string{"/a2a/agent-card.json", "/a2a/.well-known/agent-card.json"} {
		rp := r.get(path)
		if rp.Status != http.StatusOK || rp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("GET %s: HTTP %d, %q", path, rp.Status, rp.Header.Get("Content-Type"))
		}
		if got := canonical(t, rp.Body); got != want {
			t.Fatalf("GET %s:\n got %s\nwant %s", path, got, want)
		}
	}
}

func TestAgentCardCaching(t *testing.T) {
	r := newRig(t)
	rp := r.get("/a2a/agent-card.json")
	etag := rp.Header.Get("ETag")
	if etag == "" || rp.Header.Get("Cache-Control") == "" {
		t.Fatalf("card lacks caching headers: %v", rp.Header)
	}
	if rp := r.get("/a2a/agent-card.json", "If-None-Match", etag); rp.Status != http.StatusNotModified {
		t.Fatalf("conditional GET: HTTP %d, want 304", rp.Status)
	}
	if rp := r.get("/a2a/agent-card.json", "If-None-Match", `"other"`); rp.Status != http.StatusOK {
		t.Fatalf("stale validator: HTTP %d, want 200", rp.Status)
	}
}

// The card declares no streaming, push or extended card, whatever the
// configuration says.
func TestAgentCardCapabilitiesFalse(t *testing.T) {
	r := newRig(t)
	var card struct {
		Capabilities map[string]bool `json:"capabilities"`
		Interfaces   []struct {
			URL, ProtocolBinding, ProtocolVersion string
		} `json:"supportedInterfaces"`
	}
	if err := json.Unmarshal(r.get("/a2a/agent-card.json").Body, &card); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"streaming", "pushNotifications", "extendedAgentCard"} {
		if v, ok := card.Capabilities[k]; !ok || v {
			t.Errorf("capabilities.%s = %v (present %v), want an explicit false", k, v, ok)
		}
	}
	if len(card.Interfaces) != 1 || card.Interfaces[0].ProtocolBinding != "JSONRPC" || card.Interfaces[0].ProtocolVersion != "1.0" {
		t.Fatalf("interfaces = %+v, want exactly one JSONRPC 1.0", card.Interfaces)
	}
}
