package livemail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// Captured is one message the catcher accepted.
type Captured struct {
	ID string
	// Username is the account that authenticated the submission, as the
	// catcher recorded it; empty for an unauthenticated one.
	Username string
	Raw      []byte
}

// Captured lists every message the catcher holds, oldest first.
func (e Env) Captured(ctx context.Context) ([]Captured, error) {
	var list struct {
		Messages []struct {
			ID       string
			Username string
		}
	}
	if err := e.get(ctx, "/api/v1/messages?limit=500", &list); err != nil {
		return nil, err
	}
	out := make([]Captured, 0, len(list.Messages))
	for i := len(list.Messages) - 1; i >= 0; i-- {
		m := list.Messages[i]
		raw, err := e.getRaw(ctx, "/api/v1/message/"+m.ID+"/raw")
		if err != nil {
			return nil, err
		}
		out = append(out, Captured{ID: m.ID, Username: m.Username, Raw: raw})
	}
	return out, nil
}

// AwaitCaptured waits until n messages whose raw text contains token have
// been caught and returns them. A catcher that stays empty fails the test.
func (e Env) AwaitCaptured(t testing.TB, token string, n int) []Captured {
	t.Helper()
	var got []Captured
	deadline := time.Now().Add(20 * time.Second)
	for {
		all, err := e.Captured(t.Context())
		if err != nil {
			t.Fatalf("catcher: %v", err)
		}
		got = got[:0]
		for _, c := range all {
			if bytes.Contains(c.Raw, []byte(token)) {
				got = append(got, c)
			}
		}
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("catcher holds %d messages carrying %q, want %d", len(got), token, n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// CountCaptured returns how many caught messages contain token.
func (e Env) CountCaptured(t testing.TB, token string) int {
	t.Helper()
	all, err := e.Captured(t.Context())
	if err != nil {
		t.Fatalf("catcher: %v", err)
	}
	n := 0
	for _, c := range all {
		if bytes.Contains(c.Raw, []byte(token)) {
			n++
		}
	}
	return n
}

func (e Env) get(ctx context.Context, path string, into any) error {
	b, err := e.getRaw(ctx, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

func (e Env) getRaw(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.SMTPAPI+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return b, nil
}
