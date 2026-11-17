package model

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	reactor  = IdentityRef{Inbox: inboxA, ID: idD}
	opRef    = OperationRef{Tenant: tenantA, ID: idA}
	reactAt  = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	extReact = ExternalRef{Channel: channel, Namespace: ProviderMessageNamespace, Kind: RefKindMessage, ID: "wamid.1"}
)

func TestMessageCloneCopiesReactionsAndLastEdit(t *testing.T) {
	by := reactor
	orig := Message{
		Reactions: []MessageReaction{{Emoji: "👍", By: &by, Count: 1, External: []ExternalRef{extReact}, At: reactAt}},
		LastEdit:  &Edit{Actor: "agent", By: &by, At: reactAt, Operation: &opRef},
	}
	c := orig.Clone()
	c.Reactions[0].Emoji = "👎"
	c.Reactions[0].By.ID = idA
	c.Reactions[0].External[0].ID = "other"
	c.LastEdit.Actor = "someone"
	c.LastEdit.By.ID = idB
	c.LastEdit.Operation.ID = idB
	if orig.Reactions[0].Emoji != "👍" || orig.Reactions[0].By.ID != idD || orig.Reactions[0].External[0].ID != "wamid.1" ||
		orig.LastEdit.Actor != "agent" || orig.LastEdit.By.ID != idD || orig.LastEdit.Operation.ID != idA {
		t.Fatalf("original mutated through clone: %+v %+v", orig.Reactions[0], orig.LastEdit)
	}
}

func TestReactionEmojiRuleMatchesIngress(t *testing.T) {
	for name, tc := range map[string]struct {
		emoji string
		ok    bool
	}{
		"emoji":            {"👍", true},
		"variation":        {"👍️", true},
		"skin tone":        {"👍🏽", true},
		"shortcode":        {":+1:", true},
		"64 bytes":         {strings.Repeat("a", 64), true},
		"65 bytes":         {strings.Repeat("a", 65), false},
		"empty":            {"", false},
		"inner space":      {"a b", false},
		"edge space":       {" 👍", false},
		"tab":              {"a\tb", false},
		"no-break space":   {"a b", false},
		"ideographic":      {"a　b", false},
		"control":          {"a\x01", false},
		"invalid utf-8":    {"\xff", false},
		"zero-width joins": {"👨‍👩", true},
	} {
		t.Run(name, func(t *testing.T) {
			check := func(what string, err error) {
				t.Helper()
				if tc.ok {
					requireOK(t, err)
					return
				}
				if err == nil {
					t.Fatalf("%s accepted %q", what, tc.emoji)
				}
				requireError(t, err, Validation, "emoji")
			}
			check("ValidateEmoji", ValidateEmoji(tc.emoji))
			check("Reaction.Validate", Reaction{Command: cmd, Message: msgA, Emoji: tc.emoji}.Validate())
			check("PendingReaction.Validate", PendingReaction{Emoji: tc.emoji}.Validate())
			check("MessageReaction.Validate", MessageReaction{Emoji: tc.emoji, Count: 2}.Validate())
		})
	}
}

func TestPayloadReactionByOmitted(t *testing.T) {
	b, err := json.Marshal(MessageUpdatedPayload{Change: MessageChangeReactionAdded, Reaction: &PayloadReaction{Emoji: "👍"}})
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Reaction map[string]json.RawMessage `json:"reaction"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Reaction["by"]; ok {
		t.Fatalf("by present without an identity: %s", b)
	}
	if _, ok := p.Reaction["emoji"]; !ok {
		t.Fatalf("emoji missing: %s", b)
	}
	by := reactor
	b, err = json.Marshal(PayloadReaction{Emoji: "👍", By: &by})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"by":{`) {
		t.Fatalf("by missing when set: %s", b)
	}
}

func TestCountOnlyReactionHasNoIdentity(t *testing.T) {
	by := reactor
	for name, tc := range map[string]struct {
		r     MessageReaction
		field string
	}{
		"count only":            {MessageReaction{Emoji: "👍", Count: 3}, ""},
		"count only one":        {MessageReaction{Emoji: "👍", Count: 1}, ""},
		"count only zero":       {MessageReaction{Emoji: "👍"}, "count"},
		"count only negative":   {MessageReaction{Emoji: "👍", Count: -1}, "count"},
		"by":                    {MessageReaction{Emoji: "👍", By: &by, Count: 1}, ""},
		"actor":                 {MessageReaction{Emoji: "👍", Actor: "agent", Count: 1}, ""},
		"by counted":            {MessageReaction{Emoji: "👍", By: &by, Count: 2}, "count"},
		"by uncounted":          {MessageReaction{Emoji: "👍", By: &by}, "count"},
		"actor counted":         {MessageReaction{Emoji: "👍", Actor: "agent", Count: 2}, "count"},
		"by and actor":          {MessageReaction{Emoji: "👍", By: &by, Actor: "agent", Count: 1}, "actor"},
		"bad by":                {MessageReaction{Emoji: "👍", By: &IdentityRef{Inbox: inboxA}, Count: 1}, "by.id"},
		"bad actor":             {MessageReaction{Emoji: "👍", Actor: " agent", Count: 1}, "actor"},
		"bad external":          {MessageReaction{Emoji: "👍", Count: 1, External: []ExternalRef{{}}}, "external[0].channel.tenant"},
		"external on attribute": {MessageReaction{Emoji: "👍", By: &by, Count: 1, External: []ExternalRef{extReact}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.r.Validate()
			if tc.field == "" {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, tc.field)
		})
	}
}

func TestValidateReactionsSet(t *testing.T) {
	by := reactor
	other := IdentityRef{Inbox: inboxA, ID: idC}
	attributed := func(n int) []MessageReaction {
		out := make([]MessageReaction, n)
		for i := range out {
			out[i] = MessageReaction{Emoji: "👍", Actor: ActorID("actor-" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26))), Count: 1}
		}
		return out
	}
	for name, tc := range map[string]struct {
		rs    []MessageReaction
		field string
	}{
		"empty":              {nil, ""},
		"distinct":           {[]MessageReaction{{Emoji: "👍", By: &by, Count: 1}, {Emoji: "👍", By: &other, Count: 1}, {Emoji: "👎", By: &by, Count: 1}}, ""},
		"by and actor":       {[]MessageReaction{{Emoji: "👍", By: &by, Count: 1}, {Emoji: "👍", Actor: "agent", Count: 1}}, ""},
		"with counts":        {[]MessageReaction{{Emoji: "👍", By: &by, Count: 1}, {Emoji: "👍", Count: 5}, {Emoji: "🎉", Count: 2}}, ""},
		"duplicate by":       {[]MessageReaction{{Emoji: "👍", By: &by, Count: 1}, {Emoji: "👍", By: &by, Count: 1}}, "reactions[1]"},
		"duplicate actor":    {[]MessageReaction{{Emoji: "👍", Actor: "agent", Count: 1}, {Emoji: "👍", Actor: "agent", Count: 1}}, "reactions[1]"},
		"duplicate count":    {[]MessageReaction{{Emoji: "👍", Count: 1}, {Emoji: "👍", Count: 2}}, "reactions[1]"},
		"invalid entry":      {[]MessageReaction{{Emoji: "a b", Count: 1}}, "reactions[0].emoji"},
		"at the cap":         {attributed(MaxReactionsPerMessage), ""},
		"over the cap":       {attributed(MaxReactionsPerMessage + 1), "reactions"},
		"counts beyond cap":  {append(attributed(MaxReactionsPerMessage), MessageReaction{Emoji: "👍", Count: 900}), ""},
		"byte-exact emoji":   {[]MessageReaction{{Emoji: "👍", Count: 1}, {Emoji: "👍️", Count: 1}}, ""},
		"byte-exact by pair": {[]MessageReaction{{Emoji: "👍", By: &by, Count: 1}, {Emoji: "👍🏽", By: &by, Count: 1}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateReactions(tc.rs)
			if tc.field == "" {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, tc.field)
		})
	}
}

func TestAddReactionDedupesAndFoldsOverflow(t *testing.T) {
	by := reactor
	var m Message
	m.ReactionsComplete = true
	if !m.AddReaction(MessageReaction{Emoji: "👍", By: &by, Count: 1, At: reactAt}) {
		t.Fatal("first add reported no change")
	}
	if m.AddReaction(MessageReaction{Emoji: "👍", By: &by, Count: 1, At: reactAt.Add(time.Minute)}) {
		t.Fatal("repeated add reported a change")
	}
	if len(m.Reactions) != 1 || !m.Reactions[0].At.Equal(reactAt) || !m.ReactionsComplete {
		t.Fatalf("after repeated add: %+v complete=%v", m.Reactions, m.ReactionsComplete)
	}
	for i := 1; i < MaxReactionsPerMessage; i++ {
		if !m.AddReaction(MessageReaction{Emoji: "🎉", Actor: ActorID("a" + string(rune('A'+i%26)) + string(rune('A'+i/26))), Count: 1}) {
			t.Fatalf("add %d reported no change", i)
		}
	}
	requireOK(t, ValidateReactions(m.Reactions))
	// The cap is reached: further attributed adds fold into counts.
	if !m.AddReaction(MessageReaction{Emoji: "👍", Actor: "overflow-1", Count: 1, At: reactAt}) {
		t.Fatal("overflow add reported no change")
	}
	if !m.AddReaction(MessageReaction{Emoji: "👍", Actor: "overflow-2", Count: 1, At: reactAt}) {
		t.Fatal("second overflow add reported no change")
	}
	requireOK(t, ValidateReactions(m.Reactions))
	if m.ReactionsComplete {
		t.Fatal("fold kept the set complete")
	}
	count := countOnly(m.Reactions, "👍")
	if count == nil || count.Count != 3 {
		t.Fatalf("folded count = %+v, want 3 (one attributed + two folded)", count)
	}
	if attributedCount(m.Reactions) != MaxReactionsPerMessage {
		t.Fatalf("attributed entries = %d, want %d", attributedCount(m.Reactions), MaxReactionsPerMessage)
	}
}

func TestRemoveReactionByKey(t *testing.T) {
	by := reactor
	other := IdentityRef{Inbox: inboxA, ID: idC}
	m := Message{Reactions: []MessageReaction{
		{Emoji: "👍", By: &by, Count: 1},
		{Emoji: "👍", By: &other, Count: 1},
		{Emoji: "👍", Count: 7},
	}}
	if m.RemoveReaction(MessageReaction{Emoji: "👎", By: &by}) {
		t.Fatal("removing an absent key reported a change")
	}
	if m.RemoveReaction(MessageReaction{Emoji: "👍", Actor: "agent"}) {
		t.Fatal("removing an absent actor reported a change")
	}
	if !m.RemoveReaction(MessageReaction{Emoji: "👍", By: &by}) {
		t.Fatal("removal reported no change")
	}
	if len(m.Reactions) != 2 || m.Reactions[0].By == nil || *m.Reactions[0].By != other || m.Reactions[1].Count != 6 {
		t.Fatalf("after removal: %+v", m.Reactions)
	}
	if m.RemoveReaction(MessageReaction{Emoji: "👍"}) {
		t.Fatal("removal without an identity changed counts")
	}
}

func TestOperationPendingOmittedWhenNil(t *testing.T) {
	b, err := json.Marshal(Operation{Ref: opRef, State: Queued})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "pending") {
		t.Fatalf("pending present when nil: %s", b)
	}
}

func TestPendingCarriesBaseAndOperation(t *testing.T) {
	p := Operation{Ref: opRef, State: Unknown, Pending: &PendingMutation{
		Operation: opRef, BaseRevision: "r7", BaseLastEdit: &Edit{Actor: "agent", At: reactAt}, Content: &text,
	}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Pending map[string]json.RawMessage `json:"pending"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"operation", "base_revision", "base_last_edit", "content", "delete"} {
		if _, ok := w.Pending[k]; !ok {
			t.Errorf("pending lacks %q: %s", k, b)
		}
	}
	if _, ok := w.Pending["reaction"]; ok {
		t.Errorf("pending carries an unset reaction: %s", b)
	}
	var back Operation
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Pending, p.Pending) {
		t.Fatalf("pending round trip: %+v, want %+v", back.Pending, p.Pending)
	}
}

func TestPendingMutationValidate(t *testing.T) {
	edit := &Edit{Actor: "agent", At: reactAt}
	for name, tc := range map[string]struct {
		p     PendingMutation
		field string
	}{
		"edit":          {PendingMutation{Operation: opRef, BaseRevision: "r1", BaseLastEdit: edit, Content: &text}, ""},
		"delete":        {PendingMutation{Operation: opRef, BaseRevision: "r1", Delete: true}, ""},
		"react":         {PendingMutation{Operation: opRef, BaseRevision: "r1", Reaction: &PendingReaction{Emoji: "👍", Remove: true}}, ""},
		"none":          {PendingMutation{Operation: opRef, BaseRevision: "r1"}, "pending"},
		"two":           {PendingMutation{Operation: opRef, BaseRevision: "r1", Content: &text, Delete: true}, "pending"},
		"empty content": {PendingMutation{Operation: opRef, BaseRevision: "r1", Content: &Content{}}, "pending.content.parts"},
		"bad emoji":     {PendingMutation{Operation: opRef, BaseRevision: "r1", Reaction: &PendingReaction{Emoji: "a b"}}, "pending.reaction.emoji"},
		"no operation":  {PendingMutation{BaseRevision: "r1", Delete: true}, "pending.operation.tenant"},
		"no revision":   {PendingMutation{Operation: opRef, Delete: true}, "pending.base_revision"},
		"bad last edit": {PendingMutation{Operation: opRef, BaseRevision: "r1", BaseLastEdit: &Edit{}, Delete: true}, "pending.base_last_edit.actor"},
		"inbound base":  {PendingMutation{Operation: opRef, BaseRevision: "r1", BaseLastEdit: &Edit{Actor: "sink", By: &reactor}, Delete: true}, ""},
		"bad edit by":   {PendingMutation{Operation: opRef, BaseRevision: "r1", BaseLastEdit: &Edit{Actor: "sink", By: &IdentityRef{}}, Delete: true}, "pending.base_last_edit.by.inbox.tenant"},
		"bad edit op":   {PendingMutation{Operation: opRef, BaseRevision: "r1", BaseLastEdit: &Edit{Actor: "a", Operation: &OperationRef{Tenant: tenantA}}, Delete: true}, "pending.base_last_edit.operation.id"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.p.Validate()
			if tc.field == "" {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, tc.field)
		})
	}
}

// TestMessageUpdatedPayloadCarriesNoContent: edited and edit_superseded
// carry references, revisions, the operation and the change kind only.
func TestMessageUpdatedPayloadCarriesNoContent(t *testing.T) {
	allowed := []string{"message", "change", "trigger", "previous_revision", "source_revision", "reaction", "operation"}
	for _, change := range []string{MessageChangeEdited, MessageChangeEditSuperseded} {
		b, err := json.Marshal(MessageUpdatedPayload{
			Message: msgA, Change: change, Trigger: TriggerCommand, PreviousRevision: "r1", Operation: &opRef,
		})
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(b, &keys); err != nil {
			t.Fatal(err)
		}
		for k := range keys {
			if !slices.Contains(allowed, k) {
				t.Errorf("%s payload carries %q: %s", change, k, b)
			}
		}
		if strings.Contains(string(b), `"content"`) || strings.Contains(string(b), `"parts"`) {
			t.Errorf("%s payload carries content: %s", change, b)
		}
	}
	typ := reflect.TypeFor[MessageUpdatedPayload]()
	for i := range typ.NumField() {
		key, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if !slices.Contains(allowed, key) {
			t.Errorf("MessageUpdatedPayload.%s encodes %q", typ.Field(i).Name, key)
		}
	}
}

func TestReactionSummary(t *testing.T) {
	ok := ReactionSummary{Counts: []ReactionCount{{Emoji: "👍", Count: 3}, {Emoji: "🎉", Count: 0}}, Complete: true}
	data, err := json.Marshal(ok)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeReactionSummary(Extension{Name: SchemaReactionSummary, Version: 1, Data: data})
	requireOK(t, err)
	if !reflect.DeepEqual(got, ok) {
		t.Fatalf("decoded %+v, want %+v", got, ok)
	}
	for name, tc := range map[string]struct {
		ext  Extension
		code ErrorCode
		msg  string
	}{
		"other name":      {Extension{Name: "org.example.summary", Version: 1, Data: data}, Unsupported, ""},
		"other version":   {Extension{Name: SchemaReactionSummary, Version: 2, Data: data}, SchemaMismatch, ""},
		"unknown field":   {Extension{Name: SchemaReactionSummary, Version: 1, Data: []byte(`{"counts":[],"complete":true,"x":1}`)}, Validation, "data"},
		"trailing data":   {Extension{Name: SchemaReactionSummary, Version: 1, Data: []byte(`{"complete":true}{}`)}, Validation, "data"},
		"bad emoji":       {Extension{Name: SchemaReactionSummary, Version: 1, Data: []byte(`{"counts":[{"emoji":"a b","count":1}],"complete":false}`)}, Validation, "data.counts[0].emoji"},
		"negative count":  {Extension{Name: SchemaReactionSummary, Version: 1, Data: []byte(`{"counts":[{"emoji":"x","count":-1}],"complete":false}`)}, Validation, "data.counts[0].count"},
		"duplicate emoji": {Extension{Name: SchemaReactionSummary, Version: 1, Data: []byte(`{"counts":[{"emoji":"x","count":1},{"emoji":"x","count":2}],"complete":false}`)}, Validation, "data.counts[1].emoji"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeReactionSummary(tc.ext)
			requireError(t, err, tc.code, tc.msg)
		})
	}
}

func countOnly(rs []MessageReaction, emoji string) *MessageReaction {
	for i := range rs {
		if rs[i].Emoji == emoji && !rs[i].Attributed() {
			return &rs[i]
		}
	}
	return nil
}

func attributedCount(rs []MessageReaction) int {
	n := 0
	for _, r := range rs {
		if r.Attributed() {
			n++
		}
	}
	return n
}

func TestReactionStepsByCardinality(t *testing.T) {
	by := reactor
	other := IdentityRef{Inbox: inboxA, ID: idC}
	m := Message{Reactions: []MessageReaction{
		{Emoji: "👍", By: &by, Count: 1},
		{Emoji: "👍", By: &other, Count: 1},
		{Emoji: "🎉", Actor: "agent", Count: 1},
		{Emoji: "👍", Count: 7},
	}}
	add := MessageReaction{Emoji: "🎉", By: &by, Count: 1}
	for _, tc := range []struct {
		name           string
		r              MessageReaction
		remove, single bool
		want           []ReactionStep
	}{
		{"multiple add", add, false, false, []ReactionStep{{Reaction: add}}},
		{"multiple remove", add, true, false, []ReactionStep{{Reaction: add, Remove: true}}},
		{"single add replaces", add, false, true, []ReactionStep{{Reaction: m.Reactions[0], Remove: true}, {Reaction: add}}},
		{"single same emoji", MessageReaction{Emoji: "👍", By: &by, Count: 1}, false, true, []ReactionStep{{Reaction: MessageReaction{Emoji: "👍", By: &by, Count: 1}}}},
		{"single remove any emoji", add, true, true, []ReactionStep{{Reaction: m.Reactions[0], Remove: true}}},
		{"single remove names none", MessageReaction{By: &by}, true, true, []ReactionStep{{Reaction: m.Reactions[0], Remove: true}}},
		{"single actor", MessageReaction{Emoji: "👍", Actor: "agent", Count: 1}, false, true, []ReactionStep{
			{Reaction: m.Reactions[2], Remove: true}, {Reaction: MessageReaction{Emoji: "👍", Actor: "agent", Count: 1}},
		}},
		{"single remove of nothing", MessageReaction{By: &IdentityRef{Inbox: inboxA, ID: idA}}, true, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.ReactionSteps(tc.r, tc.remove, tc.single); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("steps = %+v, want %+v", got, tc.want)
			}
		})
	}
	if len(m.Reactions) != 4 {
		t.Fatalf("ReactionSteps changed the message: %+v", m.Reactions)
	}
}
