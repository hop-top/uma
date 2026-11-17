package zendesk_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
)

// A webhook writes nothing until its signature and timestamp verify.
func TestWebhookVerifiesBeforeAnyWrite(t *testing.T) {
	h := newHost(t)
	body := commentEvent(t, "evt-1", "342757", "8659716087550", "8659716080510", false, ptr(true), "Help")
	for _, c := range []struct {
		name string
		code int
		send func() int
	}{
		{"wrong secret", http.StatusUnauthorized, func() int { return h.post(body, "not-the-secret") }},
		{"stale timestamp", http.StatusUnauthorized, func() int { return h.postAt(body, webhookSecret, hostStart.Add(-301*time.Second)) }},
		{"future timestamp", http.StatusUnauthorized, func() int { return h.postAt(body, webhookSecret, hostStart.Add(301*time.Second)) }},
		{"unsigned", http.StatusUnauthorized, func() int {
			rec := httptest.NewRecorder()
			h.webhook.ServeHTTP(rec, httptest.NewRequestWithContext(h.ctx, http.MethodPost, "/zendesk", strings.NewReader(string(body))))
			return rec.Code
		}},
		{"tampered body", http.StatusUnauthorized, func() int {
			stamp := hostStart.Format(time.RFC3339)
			req := httptest.NewRequestWithContext(h.ctx, http.MethodPost, "/zendesk", strings.NewReader(strings.Replace(string(body), "Help", "Pwnd", 1)))
			req.Header.Set(zendesk.SignatureHeader, signature(webhookSecret, stamp, body))
			req.Header.Set(zendesk.SignatureTimestampHeader, stamp)
			rec := httptest.NewRecorder()
			h.webhook.ServeHTTP(rec, req)
			return rec.Code
		}},
		{"GET", http.StatusMethodNotAllowed, func() int {
			rec := httptest.NewRecorder()
			h.webhook.ServeHTTP(rec, httptest.NewRequestWithContext(h.ctx, http.MethodGet, "/zendesk", nil))
			return rec.Code
		}},
	} {
		if got := c.send(); got != c.code {
			t.Errorf("%s: status %d, want %d", c.name, got, c.code)
		}
	}
	if n := len(h.messages()); n != 0 {
		t.Fatalf("messages = %d after unverified webhooks, want 0", n)
	}
	if n := len(h.conversations()); n != 0 {
		t.Fatalf("conversations = %d after unverified webhooks, want 0", n)
	}
	if code := h.post(body, webhookSecret); code != http.StatusOK {
		t.Fatalf("signed webhook: status %d, want 200", code)
	}
	if code := h.post(body, webhookSecret); code != http.StatusOK {
		t.Fatalf("redelivered webhook: status %d, want 200", code)
	}
	msgs := h.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d after a signed webhook and its redelivery, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Author.External != (model.ExternalRef{Channel: hostChannel, Namespace: zendesk.Namespace, Kind: zendesk.RefKindUser, ID: "8659716080510"}) {
		t.Errorf("author = %+v, want the comment author", m.Author.External)
	}
	if m.Audience.Visibility != model.Public || m.Content.Parts[0].Text != "Help" {
		t.Errorf("message = %+v, want public text Help", m)
	}
	if c := h.conversation("342757"); c.Mode != model.ModePerWorkItem {
		t.Errorf("conversation mode = %q, want per work item", c.Mode)
	}
}

// Only an explicitly public comment is public: an internal note and a
// comment whose visibility is not stated are restricted.
func TestPrivateCommentsAreRestricted(t *testing.T) {
	h := newHost(t)
	for _, c := range []struct {
		id     string
		public *bool
		want   model.Visibility
	}{
		{"c-public", ptr(true), model.Public},
		{"c-private", ptr(false), model.Restricted},
		{"c-unstated", nil, model.Restricted},
	} {
		if code := h.post(commentEvent(t, "evt-"+c.id, "100", c.id, "42", false, c.public, "body "+c.id), webhookSecret); code != http.StatusOK {
			t.Fatalf("%s: status %d", c.id, code)
		}
		var found bool
		for _, m := range h.messages() {
			for _, x := range m.External {
				if x == model.ProviderMessageRef(hostChannel, c.id) {
					found = true
					if m.Audience.Visibility != c.want {
						t.Errorf("%s: audience %+v, want %s", c.id, m.Audience, c.want)
					}
				}
			}
		}
		if !found {
			t.Fatalf("%s: no message", c.id)
		}
	}
	if n := len(h.conversations()); n != 1 {
		t.Fatalf("conversations = %d for one ticket, want 1", n)
	}
	// A comment made private later narrows its message, content kept.
	if code := h.post(event(t, "evt-private-later", "comment_made_private", "100", "OPEN", map[string]any{"comment": map[string]any{"id": "c-public"}}), webhookSecret); code != http.StatusOK {
		t.Fatalf("comment_made_private: status %d", code)
	}
	for _, m := range h.messages() {
		for _, x := range m.External {
			if x == model.ProviderMessageRef(hostChannel, "c-public") && (m.Redacted || m.Audience.Visibility != model.Restricted || len(m.Content.Parts) == 0) {
				t.Fatalf("comment made private: %+v, want it restricted with its content", m)
			}
		}
	}
}

// Ticket events report the ticket's state; a merge binds the ticket merged
// into its target and a deletion deleted, never dropping the binding.
func TestTicketEventsMapWorkItemState(t *testing.T) {
	h := newHost(t)
	if code := h.post(commentEvent(t, "evt-c", "78729", "c-1", "26298442", false, ptr(true), "source ticket 1"), webhookSecret); code != http.StatusOK {
		t.Fatalf("comment: status %d", code)
	}
	item := func() model.WorkItem {
		t.Helper()
		items := h.workItems()
		if len(items) != 1 {
			t.Fatalf("work items = %d, want 1", len(items))
		}
		return items[0]
	}
	if w := item(); w.Status != model.WorkItemOpen || w.RawStatus != "open" || w.Binding != model.WorkItemBindingActive ||
		w.URL != "https://acme.zendesk.com/agent/tickets/78729" || w.Subject != "Printer on fire" ||
		w.Requester.ID != "8659716080510" || len(w.Assignees) != 1 || w.Assignees[0].ID != "7489421176190" {
		t.Fatalf("work item after the first comment = %+v", w)
	}
	h.clock.Advance(time.Second)
	if code := h.post(event(t, "evt-s", "status_changed", "78729", "PENDING", map[string]any{"current": "PENDING", "previous": "OPEN"}), webhookSecret); code != http.StatusOK {
		t.Fatalf("status_changed: status %d", code)
	}
	if w := item(); w.Status != model.WorkItemPending || w.RawStatus != "pending" {
		t.Fatalf("work item after status_changed = %+v, want pending", w)
	}
	if code := h.post(event(t, "evt-m", "merged", "78729", "CLOSED", map[string]any{"target_ticket_id": "78731"}), webhookSecret); code != http.StatusOK {
		t.Fatalf("merged: status %d", code)
	}
	w := item()
	if w.Binding != model.WorkItemBindingMerged || w.Successor == nil || *w.Successor != route(hostChannel, "78731") || w.Status != model.WorkItemClosed {
		t.Fatalf("work item after merge = %+v, want merged into 78731 and closed", w)
	}
	if w.External != route(hostChannel, "78729") {
		t.Fatalf("binding changed to %+v; the merged ticket keeps its binding", w.External)
	}
	if code := h.post(event(t, "evt-d", "soft_deleted", "78729", "DELETED", map[string]any{}), webhookSecret); code != http.StatusOK {
		t.Fatalf("soft_deleted: status %d", code)
	}
	if w := item(); w.Binding != model.WorkItemBindingDeleted || w.RawStatus != "deleted" || w.Status != "" {
		t.Fatalf("work item after soft delete = %+v, want deleted", w)
	}
	// Events outside the ticket domain are acknowledged and ignored.
	other := []byte(`{"account_id":1,"detail":{"id":"5"},"event":{},"id":"evt-u","subject":"zen:user:5","time":"2025-01-15T06:13:26Z","type":"zen:event-type:user.created","zendesk_event_version":"2022-11-06"}`)
	if code := h.post(other, webhookSecret); code != http.StatusOK {
		t.Fatalf("user event: status %d, want 200", code)
	}
}
