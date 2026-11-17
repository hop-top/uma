package ingress_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// The approved contact-lifecycle truth table, decoded strictly so schema
// drift fails loudly. Shapes follow the fixture's documented schema.

type lcFixture struct {
	Schema     string   `json:"schema"`
	RuleSet    string   `json:"rule_set"`
	Status     string   `json:"status"`
	TimeFormat string   `json:"time_format"`
	Cases      []lcCase `json:"cases"`
}

type lcCase struct {
	ID                 string            `json:"id"`
	Group              string            `json:"group"`
	Mode               string            `json:"mode"`
	Flag               *bool             `json:"flag"`
	LifecycleRules     string            `json:"lifecycle_rules"`
	Event              string            `json:"event,omitempty"`
	Candidates         []lcCandidate     `json:"candidates,omitempty"`
	TargetConversation *lcTarget         `json:"target_conversation,omitempty"`
	SelectorResult     *lcSelectorResult `json:"selector_result,omitempty"`
	Email              *lcEmail          `json:"email,omitempty"`
	Expect             lcExpect          `json:"expect"`
	Judgment           bool              `json:"judgment"`
	Note               string            `json:"note"`
}

type lcCandidate struct {
	ID                  string    `json:"id"`
	Status              string    `json:"status"`
	CreatedAt           time.Time `json:"created_at"`
	Owner               string    `json:"owner"`
	SenderIsParticipant bool      `json:"sender_is_participant"`
}

type lcTarget struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type lcSelectorResult struct {
	Existing  string `json:"existing,omitempty"`
	CreateKey string `json:"create_key,omitempty"`
	Reopen    bool   `json:"reopen,omitempty"`
}

type lcEmail struct {
	MessageID      string            `json:"message_id"`
	InReplyTo      string            `json:"in_reply_to"`
	References     string            `json:"references"`
	Subject        string            `json:"subject"`
	ListID         string            `json:"list_id"`
	AutoSubmitted  string            `json:"auto_submitted"`
	ExtraHeaders   map[string]string `json:"extra_headers"`
	StoredMessages []lcStored        `json:"stored_messages"`
}

type lcStored struct {
	MessageID    string `json:"message_id"`
	Conversation string `json:"conversation"`
	Inbox        string `json:"inbox"`
	Direction    string `json:"direction"`
}

type lcExpect struct {
	Outcome         string `json:"outcome"`
	Conversation    string `json:"conversation,omitempty"`
	Error           string `json:"error,omitempty"`
	Reopen          bool   `json:"reopen,omitempty"`
	AddParticipant  bool   `json:"add_participant,omitempty"`
	SameCreateKeyAs string `json:"same_create_key_as,omitempty"`
}

func loadLifecycleFixture(t *testing.T) lcFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "conformance", "testdata", "contact_lifecycle.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f lcFixture
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if f.Status != "approved" || f.RuleSet != model.RuleSetContactLifecycleV1 || len(f.Cases) != 84 {
		t.Fatalf("fixture status %q rule set %q cases %d; want approved %s 84",
			f.Status, f.RuleSet, len(f.Cases), model.RuleSetContactLifecycleV1)
	}
	return f
}

// fixtureConvID maps a fixture conversation name ("c1") to a UUID whose
// byte order follows the name's number, so ID tie-breaks match the table.
func fixtureConvID(name string) model.ID {
	n, err := strconv.Atoi(name[1:])
	if err != nil || name[0] != 'c' {
		panic("fixture conversation name " + name)
	}
	return model.ID(fmt.Sprintf("0000c0c0-0000-7000-8000-%012x", n))
}

var (
	fixtureInbox  = model.InboxRef{Tenant: tenantA, ID: "0000c0c0-0000-7000-8000-00000000aaaa"}
	fixtureSender = model.IdentityRef{Inbox: fixtureInbox, ID: "0000c0c0-0000-7000-8000-00000000bbbb"}
	fixtureOther  = model.IdentityRef{Inbox: fixtureInbox, ID: "0000c0c0-0000-7000-8000-00000000cccc"}
)

// selectionInput builds the selector snapshot of a routing case without
// email matches.
func selectionInput(c lcCase) policy.SelectionInput {
	in := policy.SelectionInput{
		Inbox: model.Inbox{Ref: fixtureInbox, Selection: model.Selection{
			Mode: c.Mode, LockToSingle: c.Flag, LifecycleRules: c.LifecycleRules,
		}},
		Sender: &model.Identity{Ref: fixtureSender},
		Event:  policy.EventClass(c.Event),
	}
	for _, cand := range c.Candidates {
		in.Candidates = append(in.Candidates, fixtureConversation(cand))
	}
	return in
}

func fixtureConversation(cand lcCandidate) model.Conversation {
	owner := fixtureSender
	if cand.Owner == "other" {
		owner = fixtureOther
	}
	conv := model.Conversation{
		Ref:  model.ConversationRef{Inbox: fixtureInbox, ID: fixtureConvID(cand.ID)},
		Mode: model.ModePerContact, Status: cand.Status, Owner: &owner,
		Participants: []model.Participant{{Identity: owner, Role: ingress.RoleContact}},
		CreatedAt:    cand.CreatedAt, Revision: "r1",
	}
	if cand.SenderIsParticipant && owner != fixtureSender {
		conv.Participants = append(conv.Participants, model.Participant{Identity: fixtureSender, Role: ingress.RoleParticipant})
	}
	return conv
}

func expectedCode(c lcCase) model.ErrorCode { return model.ErrorCode(c.Expect.Error) }
