package bottest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter/web/widget"
)

// Post sends a JSON request to the widget receiver under base and decodes
// the answer into out when the status is below 300. It returns the status.
func Post(t *testing.T, base, path, token string, in, out any) int {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/widget/"+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 300 {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return res.StatusCode
}

// Stream opens the visitor's event stream under base and returns its
// events. The stream closes when the test ends.
func Stream(t *testing.T, base, token string) <-chan widget.Event {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/widget/events?token="+url.QueryEscape(token), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("event stream = %d", res.StatusCode)
	}
	events, done := make(chan widget.Event, 16), make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = res.Body.Close() }()
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev widget.Event
			if err := json.Unmarshal([]byte(data), &ev); err == nil {
				events <- ev
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return events
}

// Next returns the next event of the visitor's stream, failing the test if
// none arrives within 10 seconds. what names the awaited event.
func Next(t *testing.T, events <-chan widget.Event, what string) widget.Event {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s within 10s", what)
	}
	return widget.Event{}
}
