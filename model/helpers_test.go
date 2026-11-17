package model

import (
	"errors"
	"strings"
	"testing"
)

const (
	tenantA TenantID = "tenant-a"
	tenantB TenantID = "tenant-b"

	idA ID = "00000000-0000-7000-8000-000000000001"
	idB ID = "00000000-0000-7000-8000-000000000002"
	idC ID = "00000000-0000-7000-8000-000000000003"
	idD ID = "00000000-0000-7000-8000-000000000004"

	// badID differs from idA only by case.
	badID ID = "00000000-0000-7000-8000-00000000000A"
)

var (
	inboxA = InboxRef{Tenant: tenantA, ID: idA}
	inboxB = InboxRef{Tenant: tenantA, ID: idD}
	convA  = ConversationRef{Inbox: inboxA, ID: idB}
	convB  = ConversationRef{Inbox: inboxA, ID: idD}
	msgA   = MessageRef{Conversation: convA, ID: idC}
)

// requireError asserts err is a *Error with code and, when field is set, a
// SafeMessage naming that field.
func requireError(t *testing.T, err error, code ErrorCode, field string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %v (%T), want *model.Error", err, err)
	}
	if e.Code != code {
		t.Fatalf("code = %q, want %q (message %q)", e.Code, code, e.SafeMessage)
	}
	if field != "" && !strings.HasPrefix(e.SafeMessage, field+": ") {
		t.Fatalf("message = %q, want field %q", e.SafeMessage, field)
	}
}

func requireOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
