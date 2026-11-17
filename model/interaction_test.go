package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	interactionA = InteractionRef{Conversation: convA, ID: idC}
	actorA       = ExternalRef{Channel: channel, Namespace: "chat.user", Kind: "user", ID: "u-1"}
	formFields   = Extension{Name: "org.example.form", Version: 1, Data: json.RawMessage(`{"rating":5}`)}
)

func validInput(kind string) InteractionInput {
	in := InteractionInput{
		ID: "i-1", Kind: kind, Actor: actorA,
		Deadline: time.Unix(1_700_000_003, 0),
	}
	switch kind {
	case InteractionCommand:
		in.Command, in.Arguments = "help", "billing please"
	case InteractionAction:
		in.Action = "approve"
	case InteractionSubmission:
		in.Fields = formFields
	}
	return in
}

func withInput(kind string, f func(*InteractionInput)) InteractionInput {
	in := validInput(kind)
	f(&in)
	return in
}

// InteractionInput.Validate checks the shape every adapter output must
// have: identity, closed kind, deadline, actor, per-kind identifiers, and a
// response token only with its expiry.
func TestInteractionInputValidate(t *testing.T) {
	expiry := time.Unix(1_700_001_800, 0)
	bad := ExternalRef{Channel: channel, Namespace: "chat.user", Kind: "user"}
	runRequestCases(t, map[string]requestCase{
		"command":    {validInput(InteractionCommand), ""},
		"action":     {validInput(InteractionAction), ""},
		"submission": {validInput(InteractionSubmission), ""},
		"options":    {validInput(InteractionOptions), ""},
		"full": {withInput(InteractionAction, func(i *InteractionInput) {
			i.Bot, i.Message = &recipientA, &recipientA
			i.Context = []ExternalRef{recipientA}
			i.ResponseToken, i.ResponseExpiresAt = "tok", expiry
		}), ""},
		"window without token": {withInput(InteractionAction, func(i *InteractionInput) { i.ResponseExpiresAt = expiry }), ""},
		"missing id":           {withInput(InteractionAction, func(i *InteractionInput) { i.ID = "" }), "interaction.id"},
		"missing kind":         {withInput(InteractionAction, func(i *InteractionInput) { i.Kind = "" }), "interaction.kind"},
		"unknown kind":         {withInput(InteractionAction, func(i *InteractionInput) { i.Kind = "click" }), "interaction.kind"},
		"missing deadline":     {withInput(InteractionAction, func(i *InteractionInput) { i.Deadline = time.Time{} }), "interaction.deadline"},
		"missing actor":        {withInput(InteractionAction, func(i *InteractionInput) { i.Actor = ExternalRef{} }), "interaction.actor.channel.tenant"},
		"bad bot":              {withInput(InteractionAction, func(i *InteractionInput) { i.Bot = &bad }), "interaction.bot.id"},
		"bad message":          {withInput(InteractionAction, func(i *InteractionInput) { i.Message = &bad }), "interaction.message.id"},
		"bad context":          {withInput(InteractionAction, func(i *InteractionInput) { i.Context = []ExternalRef{bad} }), "interaction.context[0].id"},
		"command no command":   {withInput(InteractionCommand, func(i *InteractionInput) { i.Command = "" }), "interaction.command"},
		"action no action":     {withInput(InteractionAction, func(i *InteractionInput) { i.Action = "" }), "interaction.action"},
		"submission no form":   {withInput(InteractionSubmission, func(i *InteractionInput) { i.Fields = Extension{} }), "interaction.fields.name"},
		"bad fields": {withInput(InteractionAction, func(i *InteractionInput) {
			i.Fields = Extension{Name: "org.example.form"}
		}), "interaction.fields.version"},
		"token without expiry": {withInput(InteractionAction, func(i *InteractionInput) { i.ResponseToken = "tok" }), "interaction.response_expires_at"},
	})
}

// A validation error never echoes the response token.
func TestInteractionInputErrorsHideToken(t *testing.T) {
	in := withInput(InteractionAction, func(i *InteractionInput) { i.ResponseToken = strings.Repeat("s", 5000) })
	in.ResponseExpiresAt = time.Unix(1, 0)
	err := in.Validate()
	requireError(t, err, Validation, "interaction.response_token")
	if strings.Contains(err.Error(), "sss") {
		t.Fatalf("error %q echoes the token", err)
	}
}

func TestIsInteractionKind(t *testing.T) {
	for _, k := range []string{"command", "action", "submission", "options"} {
		if !IsInteractionKind(k) {
			t.Errorf("IsInteractionKind(%q) = false", k)
		}
	}
	for _, k := range []string{"", "mention", "Command", "click"} {
		if IsInteractionKind(k) {
			t.Errorf("IsInteractionKind(%q) = true", k)
		}
	}
	if !IsResponseMode(ResponseModeMessage) || !IsResponseMode(ResponseModeEphemeral) ||
		!IsResponseMode(ResponseModeUpdate) || IsResponseMode("toast") {
		t.Error("IsResponseMode does not match the closed set message, ephemeral, update")
	}
}

// InteractionRef is conversation scoped under resource kind interaction.
func TestInteractionRefScope(t *testing.T) {
	if KindInteraction != "interaction" {
		t.Fatalf("KindInteraction = %q", KindInteraction)
	}
	r, err := NewInteractionRef(convA, idC)
	requireOK(t, err)
	if r != interactionA || r.Scope() != tenantA || !r.In(convA) || r.In(convB) {
		t.Fatalf("ref %+v: scope %q", r, r.Scope())
	}
	want := Ref{Tenant: tenantA, Kind: KindInteraction, Inbox: inboxA.ID, ID: idC}
	if got := r.Ref(); got != want {
		t.Fatalf("Ref() = %+v, want %+v", got, want)
	}
	requireOK(t, r.Ref().Validate())
	requireError(t, Ref{Tenant: tenantA, Kind: KindInteraction, ID: idC}.Validate(), Validation, "inbox")
	if _, err := NewInteractionRef(ConversationRef{Inbox: inboxA}, idC); err == nil {
		t.Fatal("ref without conversation accepted")
	}
	requireOK(t, InteractionRef{Conversation: convA}.ValidateScope())
	requireOK(t, Lookup[InteractionRef]{Call: call, Ref: interactionA}.Validate())
	requireError(t, Lookup[InteractionRef]{Call: call, Ref: InteractionRef{Conversation: convA}}.Validate(), Validation, "ref.id")
}

func TestEnvelopeKindInteraction(t *testing.T) {
	if EnvelopeInteraction != "interaction" || !IsEnvelopeKind(EnvelopeInteraction) {
		t.Fatalf("envelope kind %q not declared", EnvelopeInteraction)
	}
	if IsMessageUpdateKind(EnvelopeInteraction) {
		t.Fatal("interaction is a message update kind")
	}
}

func TestRespondValidate(t *testing.T) {
	soon := time.Unix(1_700_000_000, 0)
	restricted := Audience{Visibility: Restricted}
	ok := func(f func(*Respond)) Respond {
		r := Respond{Command: cmd, Interaction: interactionA, Mode: ResponseModeMessage, Audience: public, Content: text, ExpiresAt: soon}
		if f != nil {
			f(&r)
		}
		return r
	}
	if (Respond{}).RevisionRule() != RevisionAbsent {
		t.Fatal("Respond revision rule is not absent")
	}
	runRequestCases(t, map[string]requestCase{
		"message":            {ok(nil), ""},
		"update":             {ok(func(r *Respond) { r.Mode = ResponseModeUpdate }), ""},
		"ephemeral":          {ok(func(r *Respond) { r.Mode, r.Audience = ResponseModeEphemeral, restricted }), ""},
		"follow up":          {ok(func(r *Respond) { r.FollowUp = true }), ""},
		"revision rejected":  {ok(func(r *Respond) { r.Command = cmdRev }), "expected_revision"},
		"no operation":       {ok(func(r *Respond) { r.Command = Command{Call: call} }), "operation"},
		"bad interaction":    {ok(func(r *Respond) { r.Interaction = InteractionRef{Conversation: convA} }), "interaction.id"},
		"no mode":            {ok(func(r *Respond) { r.Mode = "" }), "mode"},
		"open mode":          {ok(func(r *Respond) { r.Mode = "toast" }), "mode"},
		"no audience":        {ok(func(r *Respond) { r.Audience = Audience{} }), "audience.visibility"},
		"public ephemeral":   {ok(func(r *Respond) { r.Mode = ResponseModeEphemeral }), "audience.visibility"},
		"no content":         {ok(func(r *Respond) { r.Content = Content{} }), "content.parts"},
		"foreign attachment": {ok(func(r *Respond) { r.Content = contentWithAttachment(inboxB) }), "content.parts[0].attachment"},
		"no expiry":          {ok(func(r *Respond) { r.ExpiresAt = time.Time{} }), "expires_at"},
	})
}

// A reply's InResponseTo correlates it with an interaction of the same
// conversation.
func TestReplyInResponseToSameConversation(t *testing.T) {
	other := InteractionRef{Conversation: convB, ID: idC}
	runRequestCases(t, map[string]requestCase{
		"none":         {Reply{Command: cmd, Conversation: convA, Content: text}, ""},
		"same":         {Reply{Command: cmd, Conversation: convA, Content: text, InResponseTo: &interactionA}, ""},
		"foreign":      {Reply{Command: cmd, Conversation: convA, Content: text, InResponseTo: &other}, "in_response_to"},
		"invalid":      {Reply{Command: cmd, Conversation: convA, Content: text, InResponseTo: &InteractionRef{Conversation: convA}}, "in_response_to.id"},
		"with replyto": {Reply{Command: cmd, Conversation: convA, Content: text, InReplyTo: &msgA, InResponseTo: &interactionA}, ""},
	})
}

// Ephemeral works without an interaction; a named one must be in the same
// conversation.
func TestEphemeralInteractionOptional(t *testing.T) {
	soon := time.Unix(1_700_000_000, 0)
	base := Ephemeral{Command: cmd, Conversation: convA, Recipient: ident, Content: text, ExpiresAt: soon}
	with := func(r *InteractionRef) Ephemeral {
		e := base
		e.Interaction = r
		return e
	}
	runRequestCases(t, map[string]requestCase{
		"nil":     {with(nil), ""},
		"same":    {with(&interactionA), ""},
		"foreign": {with(&InteractionRef{Conversation: convB, ID: idC}), "interaction"},
		"invalid": {with(&InteractionRef{Conversation: convA}), "interaction.id"},
	})
}

// Redact names an interaction the way it names a message: Target plus the
// typed ref, which must agree.
func TestRedactInteractionTarget(t *testing.T) {
	other := InteractionRef{Conversation: convA, ID: idD}
	runRequestCases(t, map[string]requestCase{
		"interaction":         {Redact{Command: cmdRev, Target: interactionA.Ref(), Interaction: &interactionA}, ""},
		"no interaction":      {Redact{Command: cmdRev, Target: interactionA.Ref()}, "interaction"},
		"other interaction":   {Redact{Command: cmdRev, Target: interactionA.Ref(), Interaction: &other}, "interaction"},
		"stray interaction":   {Redact{Command: cmdRev, Target: msgA.Ref(), Message: &msgA, Interaction: &interactionA}, "interaction"},
		"bad interaction":     {Redact{Command: cmdRev, Target: interactionA.Ref(), Interaction: &InteractionRef{Conversation: convA}}, "interaction.id"},
		"interaction cascade": {Redact{Command: cmdRev, Target: interactionA.Ref(), Interaction: &interactionA, Cascade: true}, "cascade"},
	})
}

// jsonKeys returns every object key reachable in the JSON encoding of v.
func jsonKeys(t *testing.T, v any) map[string]bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			for k, v := range x {
				keys[k] = true
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(doc)
	return keys
}

// populated returns a T with every field set, as the wire golden test fills it.
func populated[T any](t *testing.T) T {
	t.Helper()
	var v T
	populate(t, reflect.ValueOf(&v).Elem(), reflect.TypeFor[T]().Name())
	return v
}

// Interaction payloads carry refs and identifiers: never submitted fields,
// arguments, the response token or the respond window.
func TestInteractionPayloadsWire(t *testing.T) {
	for name, p := range map[string]any{
		"received": populated[InteractionReceivedPayload](t),
		"updated":  populated[InteractionUpdatedPayload](t),
	} {
		keys := jsonKeys(t, p)
		for _, k := range []string{"fields", "arguments", "token", "response_token", "respond_until", "response_expires_at"} {
			if keys[k] {
				t.Errorf("%s payload encodes %q", name, k)
			}
		}
		if !keys["interaction"] || !keys["trigger"] {
			t.Errorf("%s payload lacks interaction or trigger: %v", name, keys)
		}
	}
	if keys := jsonKeys(t, populated[InteractionReceivedPayload](t)); !keys["actor_kind"] {
		t.Error("interaction.received payload lacks actor_kind")
	}
	keys := jsonKeys(t, populated[MessageCreatedPayload](t))
	if !keys["in_response_to"] {
		t.Error("message.created payload lacks in_response_to")
	}
}

// The interaction record never holds the response token: the store keeps
// it beside the record, and only the token reader returns it.
func TestInteractionRecordCarriesNoToken(t *testing.T) {
	for k := range jsonKeys(t, populated[Interaction](t)) {
		if strings.Contains(k, "token") {
			t.Errorf("model.Interaction encodes %q", k)
		}
	}
	for i := range reflect.TypeFor[Interaction]().NumField() {
		if f := reflect.TypeFor[Interaction]().Field(i); strings.Contains(strings.ToLower(f.Name), "token") {
			t.Errorf("model.Interaction has field %s", f.Name)
		}
	}
}

func TestInteractionNames(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{EventInteractionReceived, "interaction.received"},
		{EventInteractionUpdated, "interaction.updated"},
		{SchemaInteractionReceived, "uma.interaction.received"},
		{SchemaInteractionUpdated, "uma.interaction.updated"},
		{ActionInteractionRead, "interaction.read"},
		{ActionInteractionRespond, "interaction.respond"},
		{string(CapabilityRespond), "respond"},
		{string(CapabilityRespondFollowUp), "respond_follow_up"},
		{MessageOriginResponse, "response"},
		{InteractionChangeResponded, "responded"},
		{InteractionChangeFollowUp, "follow_up"},
		{InteractionChangeRedacted, "redacted"},
		{ExportLineInteraction, "interaction"},
		{InteractionCommand, "command"},
		{InteractionAction, "action"},
		{InteractionSubmission, "submission"},
		{InteractionOptions, "options"},
		{ResponseModeMessage, "message"},
		{ResponseModeEphemeral, "ephemeral"},
		{ResponseModeUpdate, "update"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
