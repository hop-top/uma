package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type validator interface{ Validate() error }

type requestCase struct {
	req   validator
	field string // empty means valid
}

func runRequestCases(t *testing.T, cases map[string]requestCase) {
	t.Helper()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.field == "" {
				requireOK(t, err)
				return
			}
			requireError(t, err, Validation, tc.field)
		})
	}
}

var (
	call       = Call{Correlation: "req-1"}
	cmd        = Command{Call: call, Operation: "op-1"}
	cmdRev     = Command{Call: call, Operation: "op-1", ExpectedRevision: "r1"}
	noCall     = Command{Operation: "op-1"}
	text       = Content{Parts: []Part{{Kind: "text", Text: "hello"}}}
	ext        = Extension{Name: "org.example.action", Version: 1, Data: json.RawMessage(`{}`)}
	public     = Audience{Visibility: Public}
	channel    = ChannelRef{Tenant: tenantA, ID: idD}
	sub        = SubscriptionRef{Tenant: tenantA, ID: idA}
	task       = TaskRef{Conversation: convA, ID: idC}
	work       = WorkItemRef{Conversation: convA, ID: idC}
	stream     = StreamRef{Conversation: convA, ID: idC}
	ident      = IdentityRef{Inbox: inboxA, ID: idC}
	contact    = ContactRef{Tenant: tenantA, ID: idD}
	recipientA = ExternalRef{Channel: channel, Namespace: "chat.user", Kind: "user", ID: "u-1"}
)

func contentWithAttachment(in InboxRef) Content {
	return Content{Parts: []Part{{Kind: "attachment", Attachment: &AttachmentRef{Inbox: in, ID: idD}}}}
}

func TestRevisionRules(t *testing.T) {
	cases := map[string]struct {
		got, want RevisionRule
	}{
		"configure channel create": {ConfigureChannel{}.RevisionRule(), RevisionAbsent},
		"configure channel update": {ConfigureChannel{Channel: Channel{Ref: channel}}.RevisionRule(), RevisionRequired},
		"configure inbox create":   {ConfigureInbox{}.RevisionRule(), RevisionAbsent},
		"configure inbox update":   {ConfigureInbox{Inbox: Inbox{Ref: inboxA}}.RevisionRule(), RevisionRequired},
		"link":                     {LinkIdentity{}.RevisionRule(), RevisionRequired},
		"unlink":                   {UnlinkIdentity{}.RevisionRule(), RevisionRequired},
		"create contact":           {CreateContact{}.RevisionRule(), RevisionAbsent},
		"update contact":           {UpdateContact{}.RevisionRule(), RevisionRequired},
		"reply":                    {Reply{}.RevisionRule(), RevisionOptional},
		"start conversation":       {StartConversation{}.RevisionRule(), RevisionAbsent},
		"note":                     {InternalNote{}.RevisionRule(), RevisionOptional},
		"edit":                     {EditMessage{}.RevisionRule(), RevisionRequired},
		"delete":                   {DeleteMessage{}.RevisionRule(), RevisionRequired},
		"react":                    {Reaction{}.RevisionRule(), RevisionOptional},
		"transition":               {Transition{}.RevisionRule(), RevisionRequired},
		"work item act":            {WorkItemAction{}.RevisionRule(), RevisionRequired},
		"task act":                 {TaskAction{}.RevisionRule(), RevisionOptional},
		"task open":                {TaskAction{Action: Extension{Name: SchemaTaskOpen}}.RevisionRule(), RevisionAbsent},
		"task update":              {TaskAction{Action: Extension{Name: SchemaTaskUpdate}}.RevisionRule(), RevisionRequired},
		"execute":                  {ExecuteExtension{}.RevisionRule(), RevisionOptional},
		"redact":                   {Redact{}.RevisionRule(), RevisionRequired},
		"open subscription":        {OpenSubscription{}.RevisionRule(), RevisionAbsent},
		"ack":                      {Acknowledge{}.RevisionRule(), RevisionAbsent},
		"prepare attachment":       {PrepareAttachment{}.RevisionRule(), RevisionAbsent},
		"export":                   {ExportRequest{}.RevisionRule(), RevisionAbsent},
		"start stream":             {StartStream{}.RevisionRule(), RevisionAbsent},
		"stream update":            {StreamUpdate{}.RevisionRule(), RevisionOptional},
		"end stream":               {EndStream{}.RevisionRule(), RevisionOptional},
		"ephemeral":                {Ephemeral{}.RevisionRule(), RevisionOptional},
		"respond":                  {Respond{}.RevisionRule(), RevisionAbsent},
	}
	for name, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: rule = %d, want %d", name, tc.got, tc.want)
		}
	}
}

func TestConfigureRequests(t *testing.T) {
	newChannel := Channel{Ref: ChannelRef{Tenant: tenantA}, Type: "org.example.chat"}
	newInbox := Inbox{Ref: InboxRef{Tenant: tenantA}, Channel: channel}
	runRequestCases(t, map[string]requestCase{
		"channel create":            {ConfigureChannel{Command: cmd, Channel: newChannel}, ""},
		"channel create revision":   {ConfigureChannel{Command: cmdRev, Channel: newChannel}, "expected_revision"},
		"channel update":            {ConfigureChannel{Command: cmdRev, Channel: Channel{Ref: channel, Type: "org.example.chat"}}, ""},
		"channel update no rev":     {ConfigureChannel{Command: cmd, Channel: Channel{Ref: channel, Type: "org.example.chat"}}, "expected_revision"},
		"channel no tenant":         {ConfigureChannel{Command: cmd, Channel: Channel{Type: "org.example.chat"}}, "channel.ref.tenant"},
		"channel bad id":            {ConfigureChannel{Command: cmdRev, Channel: Channel{Ref: ChannelRef{Tenant: tenantA, ID: badID}, Type: "x.y"}}, "channel.ref.id"},
		"channel no type":           {ConfigureChannel{Command: cmd, Channel: Channel{Ref: ChannelRef{Tenant: tenantA}}}, "channel.type"},
		"channel bad configuration": {ConfigureChannel{Command: cmd, Channel: Channel{Ref: ChannelRef{Tenant: tenantA}, Type: "x.y", Configuration: Extension{Name: "a.b"}}}, "channel.configuration.version"},
		"channel no correlation":    {ConfigureChannel{Command: noCall, Channel: newChannel}, "correlation"},
		"inbox create":              {ConfigureInbox{Command: cmd, Inbox: newInbox}, ""},
		"inbox update":              {ConfigureInbox{Command: cmdRev, Inbox: Inbox{Ref: inboxA, Channel: channel}}, ""},
		"inbox update no rev":       {ConfigureInbox{Command: cmd, Inbox: Inbox{Ref: inboxA, Channel: channel}}, "expected_revision"},
		"inbox no tenant":           {ConfigureInbox{Command: cmd, Inbox: Inbox{Channel: channel}}, "inbox.ref.tenant"},
		"inbox no channel":          {ConfigureInbox{Command: cmd, Inbox: Inbox{Ref: InboxRef{Tenant: tenantA}}}, "inbox.channel.tenant"},
		"channel key":               {ConfigureChannel{Command: cmd, Channel: Channel{Ref: ChannelRef{Tenant: tenantA}, Key: "support mail", Type: "x.y"}}, ""},
		"channel bad key":           {ConfigureChannel{Command: cmd, Channel: Channel{Ref: ChannelRef{Tenant: tenantA}, Key: " lead", Type: "x.y"}}, "channel.key"},
		"inbox key":                 {ConfigureInbox{Command: cmd, Inbox: Inbox{Ref: InboxRef{Tenant: tenantA}, Channel: channel, Key: "front-desk"}}, ""},
		"inbox bad key":             {ConfigureInbox{Command: cmd, Inbox: Inbox{Ref: InboxRef{Tenant: tenantA}, Channel: channel, Key: "a\x00b"}}, "inbox.key"},
	})
}

func TestLinkIdentity(t *testing.T) {
	ev := Evidence{Verifier: "attestation", Reference: "ticket-9"}
	runRequestCases(t, map[string]requestCase{
		"valid":        {LinkIdentity{Command: cmdRev, Identity: ident, Contact: contact, Evidence: ev}, ""},
		"no revision":  {LinkIdentity{Command: cmd, Identity: ident, Contact: contact, Evidence: ev}, "expected_revision"},
		"bad identity": {LinkIdentity{Command: cmdRev, Identity: IdentityRef{Inbox: inboxA}, Contact: contact, Evidence: ev}, "identity.id"},
		"no contact":   {LinkIdentity{Command: cmdRev, Identity: ident, Evidence: ev}, "contact.tenant"},
		"no verifier":  {LinkIdentity{Command: cmdRev, Identity: ident, Contact: contact}, "evidence.verifier"},
		"bad facts":    {LinkIdentity{Command: cmdRev, Identity: ident, Contact: contact, Evidence: Evidence{Verifier: "attestation", Facts: Extension{Name: "a.b"}}}, "evidence.facts.version"},
		"bad assurance": {LinkIdentity{Command: cmdRev, Identity: ident, Contact: contact,
			Evidence: Evidence{Verifier: "attestation", Assurance: "certain"}}, "evidence.assurance"},
		"declared assurance": {LinkIdentity{Command: cmdRev, Identity: ident, Contact: contact,
			Evidence: Evidence{Verifier: "attestation", Assurance: LinkAssuranceAsserted}}, "evidence.assurance"},
	})
}

// An empty target contact ID is never an unlink sentinel: Link requires a
// full contact reference.
func TestLinkIdentityEmptyContactIDIsValidation(t *testing.T) {
	ev := Evidence{Verifier: "attestation"}
	req := LinkIdentity{Command: cmdRev, Identity: ident, Contact: ContactRef{Tenant: tenantA}, Evidence: ev}
	requireError(t, req.Validate(), Validation, "contact.id")
}

func TestUnlinkIdentityValidate(t *testing.T) {
	ev := Evidence{Verifier: "attestation", Reference: "ticket-9"}
	runRequestCases(t, map[string]requestCase{
		"valid":             {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "wrong_person"}, ""},
		"with evidence":     {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "wrong_person", Evidence: ev}, ""},
		"no revision":       {UnlinkIdentity{Command: cmd, Identity: ident, Reason: "wrong_person"}, "expected_revision"},
		"no correlation":    {UnlinkIdentity{Command: noCall, Identity: ident, Reason: "wrong_person"}, "correlation"},
		"bad identity":      {UnlinkIdentity{Command: cmdRev, Identity: IdentityRef{Inbox: inboxA}, Reason: "x"}, "identity.id"},
		"no reason":         {UnlinkIdentity{Command: cmdRev, Identity: ident}, "reason"},
		"free text reason":  {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "Jane asked"}, "reason"},
		"dotted reason":     {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "gdpr.request"}, "reason"},
		"edge dash reason":  {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "-split"}, "reason"},
		"long reason":       {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: strings.Repeat("a", 64)}, "reason"},
		"max reason":        {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: strings.Repeat("a", 63)}, ""},
		"bad verifier":      {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "x", Evidence: Evidence{Verifier: " attestation"}}, "evidence.verifier"},
		"reference only":    {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "x", Evidence: Evidence{Reference: "ticket-9"}}, "evidence.verifier"},
		"bad facts":         {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "x", Evidence: Evidence{Verifier: "attestation", Facts: Extension{Name: "a.b"}}}, "evidence.facts.version"},
		"bad reference":     {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "x", Evidence: Evidence{Verifier: "attestation", Reference: "\x00"}}, "evidence.reference"},
		"facts without key": {UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "x", Evidence: Evidence{Facts: ext}}, "evidence.verifier"},
	})
}

// No request field declares or waives evidence: assurance comes from the
// verifier only, so any assurance on a request is refused.
func TestRequestEvidenceAssuranceMustBeEmpty(t *testing.T) {
	for _, a := range []LinkAssurance{LinkAssuranceNone, LinkAssuranceAsserted, LinkAssuranceProven, "certain"} {
		ev := Evidence{Verifier: "attestation", Assurance: a}
		link := LinkIdentity{Command: cmdRev, Identity: ident, Contact: contact, Evidence: ev}
		requireError(t, link.Validate(), Validation, "evidence.assurance")
		unlink := UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "x", Evidence: ev}
		requireError(t, unlink.Validate(), Validation, "evidence.assurance")
		bare := UnlinkIdentity{Command: cmdRev, Identity: ident, Reason: "x", Evidence: Evidence{Assurance: a}}
		requireError(t, bare.Validate(), Validation, "evidence.assurance")
		req := LinkRequest{Intent: LinkClassUnlink, Identity: ident, Current: contact, ExpectedRevision: "r1", Reason: "x", Evidence: ev}
		requireError(t, req.Validate(), Validation, "evidence.assurance")
	}
}

func TestEvidenceIsZero(t *testing.T) {
	if !(Evidence{}).IsZero() {
		t.Fatal("zero evidence not zero")
	}
	for name, ev := range map[string]Evidence{
		"verifier":  {Verifier: "attestation"},
		"reference": {Reference: "r"},
		"assurance": {Assurance: LinkAssuranceNone},
		"name":      {Facts: Extension{Name: "a.b"}},
		"version":   {Facts: Extension{Version: 1}},
		"data":      {Facts: Extension{Data: json.RawMessage(`{}`)}},
	} {
		if ev.IsZero() {
			t.Errorf("%s: evidence reported zero", name)
		}
	}
}

func TestLinkRequestValidate(t *testing.T) {
	target := ContactRef{Tenant: tenantA, ID: idA}
	ev := Evidence{Verifier: "attestation"}
	base := func(intent LinkClass) LinkRequest {
		r := LinkRequest{Intent: intent, Identity: ident, Current: contact, ExpectedRevision: "r1", Evidence: ev}
		if intent == LinkClassUnlink {
			r.Reason, r.Evidence = "wrong_person", Evidence{}
		} else {
			r.Target = &target
		}
		return r
	}
	mutate := func(intent LinkClass, f func(*LinkRequest)) LinkRequest { r := base(intent); f(&r); return r }
	runRequestCases(t, map[string]requestCase{
		"link":                 {base(LinkClassLink), ""},
		"relink":               {base(LinkClassRelink), ""},
		"unlink":               {base(LinkClassUnlink), ""},
		"unlink with evidence": {mutate(LinkClassUnlink, func(r *LinkRequest) { r.Evidence = ev }), ""},
		"no intent":            {mutate(LinkClassLink, func(r *LinkRequest) { r.Intent = "" }), "intent"},
		"bad intent":           {mutate(LinkClassLink, func(r *LinkRequest) { r.Intent = "merge" }), "intent"},
		"bad identity":         {mutate(LinkClassLink, func(r *LinkRequest) { r.Identity.ID = "" }), "identity.id"},
		"bad current":          {mutate(LinkClassLink, func(r *LinkRequest) { r.Current = ContactRef{} }), "current.tenant"},
		"no revision":          {mutate(LinkClassRelink, func(r *LinkRequest) { r.ExpectedRevision = "" }), "expected_revision"},
		"bad revision":         {mutate(LinkClassRelink, func(r *LinkRequest) { r.ExpectedRevision = " r" }), "expected_revision"},
		"link no target":       {mutate(LinkClassLink, func(r *LinkRequest) { r.Target = nil }), "target"},
		"relink no target":     {mutate(LinkClassRelink, func(r *LinkRequest) { r.Target = nil }), "target"},
		"bad target":           {mutate(LinkClassLink, func(r *LinkRequest) { r.Target = &ContactRef{Tenant: tenantA} }), "target.id"},
		"target is current":    {mutate(LinkClassRelink, func(r *LinkRequest) { r.Target = &contact }), "target"},
		"unlink with target":   {mutate(LinkClassUnlink, func(r *LinkRequest) { r.Target = &target }), "target"},
		"link with reason":     {mutate(LinkClassLink, func(r *LinkRequest) { r.Reason = "x" }), "reason"},
		"unlink no reason":     {mutate(LinkClassUnlink, func(r *LinkRequest) { r.Reason = "" }), "reason"},
		"unlink bad reason":    {mutate(LinkClassUnlink, func(r *LinkRequest) { r.Reason = "Free text" }), "reason"},
		"link no evidence":     {mutate(LinkClassLink, func(r *LinkRequest) { r.Evidence = Evidence{} }), "evidence.verifier"},
		"unlink bad evidence":  {mutate(LinkClassUnlink, func(r *LinkRequest) { r.Evidence = Evidence{Reference: "r"} }), "evidence.verifier"},
	})
}

func TestMessageRequests(t *testing.T) {
	otherMsg := MessageRef{Conversation: convB, ID: idC}
	foreignInbox := InboxRef{Tenant: tenantA, ID: idC}
	runRequestCases(t, map[string]requestCase{
		"reply":                    {Reply{Command: cmd, Conversation: convA, Content: text}, ""},
		"reply in thread":          {Reply{Command: cmd, Conversation: convA, InReplyTo: &msgA, Content: text}, ""},
		"reply with revision":      {Reply{Command: cmdRev, Conversation: convA, Content: text}, ""},
		"reply foreign parent":     {Reply{Command: cmd, Conversation: convA, InReplyTo: &otherMsg, Content: text}, "in_reply_to"},
		"reply all and cc":         {Reply{Command: cmd, Conversation: convA, Content: text, Recipients: &ReplyRecipients{ReplyAll: true, Cc: []string{"a@example.com"}}}, ""},
		"reply bad cc":             {Reply{Command: cmd, Conversation: convA, Content: text, Recipients: &ReplyRecipients{Cc: []string{"A <a@example.com>"}}}, "recipients.cc"},
		"reply bad parent":         {Reply{Command: cmd, Conversation: convA, InReplyTo: &MessageRef{Conversation: convA}, Content: text}, "in_reply_to.id"},
		"reply no conversation":    {Reply{Command: cmd, Content: text}, "conversation.inbox.tenant"},
		"reply no content":         {Reply{Command: cmd, Conversation: convA}, "content.parts"},
		"reply own attachment":     {Reply{Command: cmd, Conversation: convA, Content: contentWithAttachment(inboxA)}, ""},
		"reply foreign attachment": {Reply{Command: cmd, Conversation: convA, Content: contentWithAttachment(foreignInbox)}, "content.parts[0].attachment"},
		"reply no operation":       {Reply{Command: Command{Call: call}, Conversation: convA, Content: text}, "operation"},
		"start":                    {StartConversation{Command: cmd, Inbox: inboxA, Recipient: recipientA, Content: text}, ""},
		"start revision":           {StartConversation{Command: cmdRev, Inbox: inboxA, Recipient: recipientA, Content: text}, "expected_revision"},
		"start no recipient":       {StartConversation{Command: cmd, Inbox: inboxA, Content: text}, "recipient.channel.tenant"},
		"start foreign recipient":  {StartConversation{Command: cmd, Inbox: inboxA, Recipient: ExternalRef{Channel: ChannelRef{Tenant: "tenant-b", ID: idD}, Namespace: "chat.user", Kind: "user", ID: "u-1"}, Content: text}, "recipient.channel.tenant"},
		"start no inbox":           {StartConversation{Command: cmd, Recipient: recipientA, Content: text}, "inbox.tenant"},
		"start foreign attachment": {StartConversation{Command: cmd, Inbox: inboxA, Recipient: recipientA, Content: contentWithAttachment(foreignInbox)}, "content.parts[0].attachment"},
		"note":                     {InternalNote{Command: cmd, Conversation: convA, Content: text, PolicyRef: "desk.internal"}, ""},
		"note bad policy":          {InternalNote{Command: cmd, Conversation: convA, Content: text, PolicyRef: "\x00"}, "policy_ref"},
		"note no content":          {InternalNote{Command: cmd, Conversation: convA}, "content.parts"},
		"note bad conversation":    {InternalNote{Command: cmd, Conversation: ConversationRef{Inbox: inboxA}, Content: text}, "conversation.id"},
		"edit":                     {EditMessage{Command: cmdRev, Message: msgA, Content: text}, ""},
		"edit no revision":         {EditMessage{Command: cmd, Message: msgA, Content: text}, "expected_revision"},
		"edit bad message":         {EditMessage{Command: cmdRev, Message: MessageRef{Conversation: convA}, Content: text}, "message.id"},
		"edit foreign attachment":  {EditMessage{Command: cmdRev, Message: msgA, Content: contentWithAttachment(foreignInbox)}, "content.parts[0].attachment"},
		"delete":                   {DeleteMessage{Command: cmdRev, Message: msgA}, ""},
		"delete no revision":       {DeleteMessage{Command: cmd, Message: msgA}, "expected_revision"},
		"delete bad message":       {DeleteMessage{Command: cmdRev}, "message.conversation.inbox.tenant"},
		"react":                    {Reaction{Command: cmd, Message: msgA, Emoji: "👍"}, ""},
		"react remove":             {Reaction{Command: cmd, Message: msgA, Emoji: ":+1:", Remove: true}, ""},
		"react no emoji":           {Reaction{Command: cmd, Message: msgA}, "emoji"},
		"react bad message":        {Reaction{Command: cmd, Emoji: "x"}, "message.conversation.inbox.tenant"},
	})
}

func TestConversationAndActionRequests(t *testing.T) {
	msgB := MessageRef{Conversation: convA, ID: idD}
	attA := AttachmentRef{Inbox: inboxA, ID: idC}
	runRequestCases(t, map[string]requestCase{
		"transition":             {Transition{Command: cmdRev, Conversation: convA, State: ConversationResolved}, ""},
		"transition no revision": {Transition{Command: cmd, Conversation: convA, State: ConversationClosed}, "expected_revision"},
		"transition bad state":   {Transition{Command: cmdRev, Conversation: convA, State: "snoozed"}, "state"},
		"transition bad conv":    {Transition{Command: cmdRev, State: ConversationOpen}, "conversation.inbox.tenant"},
		"work item":              {WorkItemAction{Command: cmdRev, Target: work, Action: ext}, ""},
		"work item no revision":  {WorkItemAction{Command: cmd, Target: work, Action: ext}, "expected_revision"},
		"work item bad target":   {WorkItemAction{Command: cmdRev, Action: ext}, "target.conversation.inbox.tenant"},
		"work item no action":    {WorkItemAction{Command: cmdRev, Target: work}, "action.name"},
		"task":                   {TaskAction{Command: cmd, Target: task, Action: ext}, ""},
		"task bad target":        {TaskAction{Command: cmd, Target: TaskRef{Conversation: convA}, Action: ext}, "target.id"},
		"task no action":         {TaskAction{Command: cmd, Target: task}, "action.name"},
		"execute":                {ExecuteExtension{Command: cmd, Target: msgA.Ref(), Audience: public, Action: ext}, ""},
		"execute bad target":     {ExecuteExtension{Command: cmd, Target: Ref{Tenant: tenantA}, Audience: public, Action: ext}, "target.kind"},
		"execute no audience":    {ExecuteExtension{Command: cmd, Target: msgA.Ref(), Action: ext}, "audience.visibility"},
		"execute no action":      {ExecuteExtension{Command: cmd, Target: msgA.Ref(), Audience: public}, "action.name"},
		"query":                  {QueryExtension{Call: call, Target: convA.Ref(), Audience: public, Query: ext}, ""},
		"query no correlation":   {QueryExtension{Target: convA.Ref(), Audience: public, Query: ext}, "correlation"},
		"query bad target":       {QueryExtension{Call: call, Target: Ref{Tenant: tenantA, Kind: resourceConversation, ID: idB}, Audience: public, Query: ext}, "target.inbox"},
		"query no audience":      {QueryExtension{Call: call, Target: convA.Ref(), Query: ext}, "audience.visibility"},
		"query no query":         {QueryExtension{Call: call, Target: convA.Ref(), Audience: public}, "query.name"},
		"redact":                 {Redact{Command: cmdRev, Target: msgA.Ref(), Message: &msgA, Reason: "gdpr"}, ""},
		"redact attachment":      {Redact{Command: cmdRev, Target: attA.Ref()}, ""},
		"redact no revision":     {Redact{Command: cmd, Target: msgA.Ref(), Message: &msgA}, "expected_revision"},
		"redact bad target":      {Redact{Command: cmdRev}, "target.tenant"},
		"redact bad reason":      {Redact{Command: cmdRev, Target: msgA.Ref(), Message: &msgA, Reason: " x"}, "reason"},
		"redact no message":      {Redact{Command: cmdRev, Target: msgA.Ref()}, "message"},
		"redact other message":   {Redact{Command: cmdRev, Target: msgA.Ref(), Message: &msgB}, "message"},
		"redact stray message":   {Redact{Command: cmdRev, Target: attA.Ref(), Message: &msgA}, "message"},
		"redact bad message":     {Redact{Command: cmdRev, Target: msgA.Ref(), Message: &MessageRef{Conversation: convA}}, "message.id"},
		"redact cascade":         {Redact{Command: cmdRev, Target: convA.Ref(), Cascade: true}, ""},
		"redact contact cascade": {Redact{Command: cmdRev, Target: ContactRef{Tenant: tenantA, ID: idA}.Ref(), Cascade: true}, ""},
		"redact no cascade":      {Redact{Command: cmdRev, Target: convA.Ref()}, "cascade"},
		"redact message cascade": {Redact{Command: cmdRev, Target: msgA.Ref(), Message: &msgA, Cascade: true}, "cascade"},
		"redact attach cascade":  {Redact{Command: cmdRev, Target: attA.Ref(), Cascade: true}, "cascade"},
	})
}

func TestSubscriptionRequests(t *testing.T) {
	filter := Filter{Inboxes: []InboxRef{inboxA}, Kinds: []string{"message.received"}}
	runRequestCases(t, map[string]requestCase{
		"open":               {OpenSubscription{Command: cmd, Consumer: "bot", Filter: filter, Start: "tail", PayloadMajor: 1}, ""},
		"open beginning":     {OpenSubscription{Command: cmd, Consumer: "bot", Start: StartBeginning, PayloadMajor: 1}, ""},
		"open checkpoint":    {OpenSubscription{Command: cmd, Consumer: "bot", Start: StartCheckpoint, After: "c1", PayloadMajor: 1}, ""},
		"open revision":      {OpenSubscription{Command: cmdRev, Consumer: "bot", Start: "tail", PayloadMajor: 1}, "expected_revision"},
		"open no consumer":   {OpenSubscription{Command: cmd, Start: "tail", PayloadMajor: 1}, "consumer"},
		"open no start":      {OpenSubscription{Command: cmd, Consumer: "bot", PayloadMajor: 1}, "start"},
		"open bad start":     {OpenSubscription{Command: cmd, Consumer: "bot", Start: "latest", PayloadMajor: 1}, "start"},
		"open no after":      {OpenSubscription{Command: cmd, Consumer: "bot", Start: StartCheckpoint, PayloadMajor: 1}, "after"},
		"open stray after":   {OpenSubscription{Command: cmd, Consumer: "bot", Start: StartTail, After: "c1", PayloadMajor: 1}, "after"},
		"open no major":      {OpenSubscription{Command: cmd, Consumer: "bot", Start: StartTail}, "payload_major"},
		"open bad filter":    {OpenSubscription{Command: cmd, Consumer: "bot", Start: "tail", Filter: Filter{Kinds: []string{""}}, PayloadMajor: 1}, "filter.kinds[0]"},
		"poll":               {Poll{Call: call, Subscription: sub, Limit: 20, Wait: time.Second}, ""},
		"poll no call":       {Poll{Subscription: sub}, "correlation"},
		"poll bad sub":       {Poll{Call: call}, "subscription.tenant"},
		"poll negative":      {Poll{Call: call, Subscription: sub, Limit: -1}, "limit"},
		"poll negative wait": {Poll{Call: call, Subscription: sub, Wait: -time.Second}, "wait"},
		"ack":                {Acknowledge{Command: cmd, Subscription: sub, Token: "t"}, ""},
		"ack revision":       {Acknowledge{Command: cmdRev, Subscription: sub, Token: "t"}, "expected_revision"},
		"ack no token":       {Acknowledge{Command: cmd, Subscription: sub}, "token"},
		"ack bad sub":        {Acknowledge{Command: cmd, Token: "t"}, "subscription.tenant"},
	})
}

func TestAttachmentAndExportRequests(t *testing.T) {
	meta := AttachmentMetadata{Name: "a.pdf", MediaType: "application/pdf", Size: 10, Audience: public}
	runRequestCases(t, map[string]requestCase{
		"prepare":             {PrepareAttachment{Command: cmd, Inbox: inboxA, Metadata: meta}, ""},
		"prepare revision":    {PrepareAttachment{Command: cmdRev, Inbox: inboxA, Metadata: meta}, "expected_revision"},
		"prepare bad inbox":   {PrepareAttachment{Command: cmd, Metadata: meta}, "inbox.tenant"},
		"prepare no audience": {PrepareAttachment{Command: cmd, Inbox: inboxA, Metadata: AttachmentMetadata{Size: 1}}, "metadata.audience.visibility"},
		"prepare negative":    {PrepareAttachment{Command: cmd, Inbox: inboxA, Metadata: AttachmentMetadata{Size: -1, Audience: public}}, "metadata.size"},
		"export":              {ExportRequest{Command: cmd, Filter: Filter{Contact: &contact}, Format: "jsonl"}, ""},
		"export revision":     {ExportRequest{Command: cmdRev, Format: "jsonl"}, "expected_revision"},
		"export no format":    {ExportRequest{Command: cmd}, "format"},
		"export bad filter":   {ExportRequest{Command: cmd, Format: "jsonl", Filter: Filter{Contact: &ContactRef{Tenant: tenantA}}}, "filter.contact.id"},
	})
}

func TestStreamRequests(t *testing.T) {
	soon := time.Unix(1700000000, 0)
	runRequestCases(t, map[string]requestCase{
		"start":                   {StartStream{Command: cmd, Conversation: convA, Audience: public, Mode: "edit"}, ""},
		"start revision":          {StartStream{Command: cmdRev, Conversation: convA, Audience: public, Mode: "edit"}, "expected_revision"},
		"start no conversation":   {StartStream{Command: cmd, Audience: public, Mode: "edit"}, "conversation.inbox.tenant"},
		"start no audience":       {StartStream{Command: cmd, Conversation: convA, Mode: "edit"}, "audience.visibility"},
		"start no mode":           {StartStream{Command: cmd, Conversation: convA, Audience: public}, "mode"},
		"start deadline":          {StartStream{Command: cmd, Conversation: convA, Audience: public, Mode: "edit", Deadline: MaxStreamDeadline}, ""},
		"start negative deadline": {StartStream{Command: cmd, Conversation: convA, Audience: public, Mode: "edit", Deadline: -time.Second}, "deadline"},
		"start deadline too long": {StartStream{Command: cmd, Conversation: convA, Audience: public, Mode: "edit", Deadline: MaxStreamDeadline + time.Millisecond}, "deadline"},
		"update":                  {StreamUpdate{Command: cmd, Stream: stream, Sequence: 1, Content: text}, ""},
		"update bad stream":       {StreamUpdate{Command: cmd, Stream: StreamRef{Conversation: convA}, Content: text}, "stream.id"},
		"update no content":       {StreamUpdate{Command: cmd, Stream: stream}, "content.parts"},
		"update foreign attach":   {StreamUpdate{Command: cmd, Stream: stream, Content: contentWithAttachment(inboxB)}, "content.parts[0].attachment"},
		"end":                     {EndStream{Command: cmd, Stream: stream, FinalSequence: 3}, ""},
		"end bad stream":          {EndStream{Command: cmd}, "stream.conversation.inbox.tenant"},
		"end no operation":        {EndStream{Command: Command{Call: call}, Stream: stream}, "operation"},
		"signal":                  {Signal{Call: call, Conversation: convA, Kind: "typing", Audience: public, ExpiresAt: soon}, ""},
		"signal no call":          {Signal{Conversation: convA, Kind: "typing", Audience: public, ExpiresAt: soon}, "correlation"},
		"signal bad conv":         {Signal{Call: call, Kind: "typing", Audience: public, ExpiresAt: soon}, "conversation.inbox.tenant"},
		"signal no kind":          {Signal{Call: call, Conversation: convA, Audience: public, ExpiresAt: soon}, "kind"},
		"signal no audience":      {Signal{Call: call, Conversation: convA, Kind: "typing", ExpiresAt: soon}, "audience.visibility"},
		"signal no expiry":        {Signal{Call: call, Conversation: convA, Kind: "typing", Audience: public}, "expires_at"},
		"ephemeral":               {Ephemeral{Command: cmd, Conversation: convA, Recipient: ident, Content: text, ExpiresAt: soon}, ""},
		"ephemeral bad conv":      {Ephemeral{Command: cmd, Recipient: ident, Content: text, ExpiresAt: soon}, "conversation.inbox.tenant"},
		"ephemeral bad rcpt":      {Ephemeral{Command: cmd, Conversation: convA, Recipient: IdentityRef{Inbox: inboxA}, Content: text, ExpiresAt: soon}, "recipient.id"},
		"ephemeral foreign rcpt":  {Ephemeral{Command: cmd, Conversation: convA, Recipient: IdentityRef{Inbox: inboxB, ID: idC}, Content: text, ExpiresAt: soon}, "recipient"},
		"ephemeral no content":    {Ephemeral{Command: cmd, Conversation: convA, Recipient: ident, ExpiresAt: soon}, "content.parts"},
		"ephemeral no expiry":     {Ephemeral{Command: cmd, Conversation: convA, Recipient: ident, Content: text}, "expires_at"},
	})
}

func TestQueryRequests(t *testing.T) {
	conv := convA
	runRequestCases(t, map[string]requestCase{
		"list":                {ListRequest{Call: call, Filter: Filter{Inboxes: []InboxRef{inboxA}, Conversation: &conv, Statuses: []string{"open"}}, Page: PageRequest{Limit: 50, After: "c"}}, ""},
		"list no call":        {ListRequest{}, "correlation"},
		"list bad inbox":      {ListRequest{Call: call, Filter: Filter{Inboxes: []InboxRef{inboxA, {Tenant: tenantA}}}}, "filter.inboxes[1].id"},
		"list bad conv":       {ListRequest{Call: call, Filter: Filter{Conversation: &ConversationRef{Inbox: inboxA}}}, "filter.conversation.id"},
		"list bad status":     {ListRequest{Call: call, Filter: Filter{Statuses: []string{"open", " "}}}, "filter.statuses[1]"},
		"list negative limit": {ListRequest{Call: call, Page: PageRequest{Limit: -1}}, "page.limit"},
		"lookup":              {Lookup[MessageRef]{Call: call, Ref: msgA}, ""},
		"lookup no call":      {Lookup[MessageRef]{Ref: msgA}, "correlation"},
		"lookup bad ref":      {Lookup[ConversationRef]{Call: call, Ref: ConversationRef{Inbox: inboxA}}, "ref.id"},
		"lookup channel":      {Lookup[ChannelRef]{Call: call, Ref: channel}, ""},
		"lookup unvalidated":  {Lookup[string]{Call: call, Ref: "anything"}, ""},
		"lookup key":          {LookupKey{Call: call, Key: "front-desk"}, ""},
		"lookup no key":       {LookupKey{Call: call}, "key"},
		"lookup key no call":  {LookupKey{Key: "front-desk"}, "correlation"},
	})
}

func TestFilterValidate(t *testing.T) {
	conv := convA
	requireOK(t, Filter{}.Validate())
	requireOK(t, Filter{Inboxes: []InboxRef{inboxA}, Conversation: &conv, Contact: &contact, Kinds: []string{"message"}}.Validate())
	requireError(t, Filter{Inboxes: []InboxRef{{}}}.Validate(), Validation, "inboxes[0].tenant")
	requireError(t, Filter{Kinds: []string{"ok", ""}}.Validate(), Validation, "kinds[1]")
}
