package conformance_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// The contact-lifecycle truth table is the conformance contract for
// per-contact conversation reuse: config validation, lifecycle states, tie
// breaks, races, message updates and email threading. Ingress tests consume
// the rows; this file only guards the fixture's shape so a malformed edit
// fails here, not as a confusing behavioural failure elsewhere.

const (
	fixtureSchema  = "uma.conformance.contact_lifecycle/v1"
	fixtureRuleSet = "contact-lifecycle/v1"
	fixtureCases   = 84
)

// present records whether a key appeared in the JSON, so an explicit null is
// told apart from a missing key.
type present[T any] struct {
	Set   bool
	Value T
}

func (p *present[T]) UnmarshalJSON(b []byte) error {
	p.Set = true
	return json.Unmarshal(b, &p.Value)
}

type fixture struct {
	Schema     string        `json:"schema"`
	RuleSet    string        `json:"rule_set"`
	Status     string        `json:"status"`
	TimeFormat string        `json:"time_format"`
	Cases      []fixtureCase `json:"cases"`
}

type fixtureCase struct {
	ID                 string                       `json:"id"`
	Group              string                       `json:"group"`
	Mode               string                       `json:"mode"`
	Flag               present[*bool]               `json:"flag"`
	LifecycleRules     present[string]              `json:"lifecycle_rules"`
	Event              string                       `json:"event"`
	Candidates         []candidate                  `json:"candidates"`
	TargetConversation present[*targetConversation] `json:"target_conversation"`
	SelectorResult     *selectorResult              `json:"selector_result"`
	Email              *email                       `json:"email"`
	Expect             *expectation                 `json:"expect"`
	Note               present[string]              `json:"note"`
	Judgment           present[bool]                `json:"judgment"`
}

type candidate struct {
	ID                  string `json:"id"`
	Status              string `json:"status"`
	CreatedAt           string `json:"created_at"`
	Owner               string `json:"owner"`
	SenderIsParticipant *bool  `json:"sender_is_participant"`
}

type targetConversation struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type selectorResult struct {
	Existing  string `json:"existing"`
	CreateKey string `json:"create_key"`
	Reopen    bool   `json:"reopen"`
}

type email struct {
	Subject        string            `json:"subject"`
	MessageID      string            `json:"message_id"`
	InReplyTo      string            `json:"in_reply_to"`
	References     string            `json:"references"`
	AutoSubmitted  string            `json:"auto_submitted"`
	ListID         string            `json:"list_id"`
	ExtraHeaders   map[string]string `json:"extra_headers"`
	StoredMessages []storedMessage   `json:"stored_messages"`
}

type storedMessage struct {
	MessageID    string `json:"message_id"`
	Conversation string `json:"conversation"`
	Direction    string `json:"direction"`
	Inbox        string `json:"inbox"`
}

type expectation struct {
	Outcome         string `json:"outcome"`
	Error           string `json:"error"`
	Conversation    string `json:"conversation"`
	Reopen          *bool  `json:"reopen"`
	AddParticipant  bool   `json:"add_participant"`
	SameCreateKeyAs string `json:"same_create_key_as"`
}

// Closed vocabularies. A value outside these sets is a fixture defect.
var (
	groupPrefix = map[string]string{
		"config":         "CFG",
		"lifecycle":      "LC",
		"tie_break":      "TB",
		"race":           "RC",
		"message_update": "UP",
		"email":          "EM",
		"core_check":     "CK",
	}
	modes      = set("per_contact", "per_room", "per_thread", "per_work_item", "per_agent_context")
	states     = set("open", "pending", "resolved", "closed")
	events     = set("contact_message", "auto_message", "message_update")
	owners     = set("sender", "other")
	directions = set("inbound", "outbound")
	inboxes    = set("self", "other")
	// outcomeError pairs each expected outcome with the error it carries
	// ("" = success, no error).
	outcomeError = map[string]string{
		"accept":        "",
		"reject":        "validation",
		"create":        "",
		"reuse":         "",
		"reuse_reopen":  "",
		"follow_target": "",
		"conflict":      "conflict",
		"not_found":     "not_found",
	}
	configOutcomes = set("accept", "reject")
	caseIDPattern  = regexp.MustCompile(`^([A-Z]+)-[0-9]{2}[TF]?$`)
)

func set(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "contact_lifecycle.json"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := decodeStrict(raw)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return f
}

// decodeStrict rejects unknown fields and anything after the top-level value.
func decodeStrict(raw []byte) (fixture, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f fixture
	if err := dec.Decode(&f); err != nil {
		return fixture{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fixture{}, errors.New("trailing data after fixture object")
	}
	return f, nil
}

func TestContactLifecycleFixtureDecodeIsStrict(t *testing.T) {
	cases := map[string]string{
		"unknown top-level field": `{"schema":"x","extra":1}`,
		"unknown case field":      `{"cases":[{"id":"CFG-01","source":"x"}]}`,
		"unknown nested field":    `{"cases":[{"expect":{"outcome":"accept","why":"x"}}]}`,
		"trailing object":         `{"schema":"x"} {}`,
		"trailing garbage":        `{"schema":"x"} x`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeStrict([]byte(in)); err == nil {
				t.Fatalf("decodeStrict(%s) accepted invalid input", in)
			}
		})
	}
}

func TestContactLifecycleFixtureHeader(t *testing.T) {
	f := loadFixture(t)
	if f.Schema != fixtureSchema {
		t.Errorf("schema = %q, want %q", f.Schema, fixtureSchema)
	}
	if f.RuleSet != fixtureRuleSet {
		t.Errorf("rule_set = %q, want %q", f.RuleSet, fixtureRuleSet)
	}
	if f.Status != "approved" {
		t.Errorf("status = %q, want approved", f.Status)
	}
	if f.TimeFormat != "RFC3339" {
		t.Errorf("time_format = %q, want RFC3339", f.TimeFormat)
	}
	if len(f.Cases) != fixtureCases {
		t.Errorf("len(cases) = %d, want %d", len(f.Cases), fixtureCases)
	}
}

func TestContactLifecycleFixtureCases(t *testing.T) {
	f := loadFixture(t)
	byID := make(map[string]fixtureCase, len(f.Cases))
	for _, c := range f.Cases {
		if _, dup := byID[c.ID]; dup {
			t.Errorf("duplicate case id %q", c.ID)
		}
		byID[c.ID] = c
	}
	for _, c := range f.Cases {
		for _, problem := range checkCase(c, byID) {
			t.Errorf("%s: %s", c.ID, problem)
		}
	}
}

func checkCase(c fixtureCase, byID map[string]fixtureCase) []string {
	var p []string
	bad := func(format string, args ...any) {
		p = append(p, fmt.Sprintf(format, args...))
	}

	prefix, ok := groupPrefix[c.Group]
	if !ok {
		bad("unknown group %q", c.Group)
	}
	if m := caseIDPattern.FindStringSubmatch(c.ID); m == nil {
		bad("malformed id")
	} else if ok && m[1] != prefix {
		bad("id prefix %q does not match group %q", m[1], c.Group)
	}
	if !modes[c.Mode] {
		bad("unknown mode %q", c.Mode)
	}
	if !c.Flag.Set {
		bad("flag missing (null is explicit, absence is not)")
	}
	if !c.LifecycleRules.Set {
		bad("lifecycle_rules missing")
	}
	if !c.Note.Set {
		bad("note missing")
	}
	if !c.Judgment.Set {
		bad("judgment missing")
	}
	if c.Expect == nil {
		bad("expect missing")
		return p
	}
	p = append(p, checkExpect(c, byID)...)

	if c.Group == "config" {
		if !configOutcomes[c.Expect.Outcome] {
			bad("config outcome %q, want accept or reject", c.Expect.Outcome)
		}
		if c.Event != "" || c.Candidates != nil || c.Email != nil ||
			c.SelectorResult != nil || c.TargetConversation.Set {
			bad("config case carries routing input")
		}
		return p
	}

	// Routing cases run under a valid per-contact configuration.
	if c.Mode != "per_contact" {
		bad("routing case mode %q, want per_contact", c.Mode)
	}
	if c.Flag.Value == nil {
		bad("routing case flag is null")
	}
	if c.LifecycleRules.Value != fixtureRuleSet {
		bad("routing case lifecycle_rules %q, want %q", c.LifecycleRules.Value, fixtureRuleSet)
	}
	if !events[c.Event] {
		bad("unknown event %q", c.Event)
	}
	if c.Candidates == nil {
		bad("candidates missing")
	}
	seen := map[string]bool{}
	for _, cand := range c.Candidates {
		if cand.ID == "" || seen[cand.ID] {
			bad("candidate id %q empty or duplicated", cand.ID)
		}
		seen[cand.ID] = true
		if !states[cand.Status] {
			bad("candidate %s: unknown state %q", cand.ID, cand.Status)
		}
		if _, err := time.Parse(time.RFC3339, cand.CreatedAt); err != nil {
			bad("candidate %s: created_at: %v", cand.ID, err)
		}
		if !owners[cand.Owner] {
			bad("candidate %s: unknown owner %q", cand.ID, cand.Owner)
		}
		if cand.SenderIsParticipant == nil {
			bad("candidate %s: sender_is_participant missing", cand.ID)
		}
	}

	if (c.Group == "message_update") != c.TargetConversation.Set {
		bad("target_conversation present iff group message_update")
	}
	if (c.Group == "message_update") != (c.Event == "message_update") {
		bad("event message_update iff group message_update")
	}
	if tc := c.TargetConversation.Value; tc != nil {
		if tc.ID == "" || !states[tc.Status] {
			bad("target_conversation %+v invalid", *tc)
		}
	}
	if (c.Group == "core_check") != (c.SelectorResult != nil) {
		bad("selector_result present iff group core_check")
	}
	if (c.Group == "email") != (c.Email != nil) {
		bad("email present iff group email")
	}
	if c.Email != nil {
		if c.Email.StoredMessages == nil {
			bad("email.stored_messages missing")
		}
		for _, m := range c.Email.StoredMessages {
			if m.MessageID == "" || m.Conversation == "" ||
				!directions[m.Direction] || !inboxes[m.Inbox] {
				bad("stored message %+v invalid", m)
			}
		}
	}
	return p
}

func checkExpect(c fixtureCase, byID map[string]fixtureCase) []string {
	var p []string
	bad := func(format string, args ...any) {
		p = append(p, fmt.Sprintf(format, args...))
	}
	e := c.Expect
	wantErr, ok := outcomeError[e.Outcome]
	if !ok {
		bad("unknown outcome %q", e.Outcome)
		return p
	}
	if e.Error != wantErr {
		bad("outcome %q carries error %q, want %q", e.Outcome, e.Error, wantErr)
	}

	switch e.Outcome {
	case "reuse", "reuse_reopen":
		if !hasCandidate(c, e.Conversation) {
			bad("reused conversation %q is not a candidate", e.Conversation)
		}
		if e.Reopen == nil || *e.Reopen != (e.Outcome == "reuse_reopen") {
			bad("reopen must be explicit and true only for reuse_reopen")
		}
	case "follow_target":
		tc := c.TargetConversation.Value
		if tc == nil || e.Conversation != tc.ID {
			bad("follow_target conversation %q is not the target", e.Conversation)
		}
	default:
		if e.Conversation != "" || e.Reopen != nil {
			bad("outcome %q names a conversation or reopen", e.Outcome)
		}
	}
	if e.Outcome == "follow_target" && e.Reopen != nil {
		bad("follow_target never sets reopen")
	}
	if e.AddParticipant && e.Outcome != "reuse" {
		bad("add_participant only on reuse")
	}
	if ref := e.SameCreateKeyAs; ref != "" {
		other, ok := byID[ref]
		switch {
		case e.Outcome != "create":
			bad("same_create_key_as only on create")
		case !ok || ref == c.ID:
			bad("same_create_key_as %q is not another case", ref)
		case other.Expect == nil || other.Expect.Outcome != "create":
			bad("same_create_key_as %q does not create", ref)
		}
	}
	return p
}

func hasCandidate(c fixtureCase, id string) bool {
	for _, cand := range c.Candidates {
		if id != "" && cand.ID == id {
			return true
		}
	}
	return false
}
