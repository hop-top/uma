package conformance

import (
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// historyCases run Runtime.Import over a scripted history provider: a
// conversation's window imported once, paging and resuming, a channel
// import, gaps, authorization, an adapter without history, bad requests,
// and imported history left silent for snooze and unread.
var historyCases = []struct {
	name string
	run  func(*testing.T, *historyEnv)
}{
	{"ConversationWindowImportedOnce", historyImportedOnce},
	{"BoundedAndResumable", historyBounded},
	{"ChannelImportFillsEachConversation", historyChannel},
	{"GapsAreCounted", historyGaps},
	{"MisbehavingProviderEndsTheCall", historyMisbehaving},
	{"UnauthorizedRefusedBeforeTheProvider", historyUnauthorized},
	{"BadRequests", historyBadRequests},
	{"ImportDoesNotWakeSnooze", historySilentSnooze},
	{"ImportedSnoozeStillExpires", historySilentSnoozeExpiry},
	{"ImportIntoAwakeConversation", historySilentAwake},
	{"ImportAfterMarkerStaysRead", historySilentRead},
	{"ImportBeforeMarkerIsRead", historySilentBeforeMarker},
	{"ImportedOnlyConversationNeverUnread", historySilentImportedOnly},
	{"MarkUnreadSkipsImported", historySilentMarkUnread},
	{"ReaderIsOfferedTheChannelsStager", historyStager},
}

// historyImportedOnce: the window's messages land in the conversation in
// one call, oldest first; the message ingress already holds is skipped and
// left as it is, an older one outside the window is never asked for, and
// a second run imports nothing.
func historyImportedOnce(t *testing.T, env *historyEnv) {
	at := env.start.Add(-3 * time.Hour)
	edit := env.message("room-1", "pm-edited", at.Add(30*time.Second))
	edit.Kind, edit.SourceRevision = model.EnvelopeMessageEdited, "2"
	anonymous := env.message("room-1", "", at.Add(40*time.Second))
	elsewhere := env.message("room-1", "pm-elsewhere", at.Add(50*time.Second))
	elsewhere.Destination = historyDest2
	env.hold(
		env.message("room-1", "pm-old", env.start.Add(-48*time.Hour)),
		env.message("room-1", "pm-1", at),
		edit, anonymous, elsewhere,
		env.message("room-1", "pm-2", at.Add(time.Minute)),
		env.message("room-1", "pm-live", env.start.Add(-time.Hour)),
		env.message("room-1", "pm-3", at.Add(2*time.Minute)),
	)
	head := env.received()
	rep, err := env.importNow(env.importReq(nil))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	// Skipped: an edit (no new message), one without a provider message
	// ID, one bound for another inbox, and the live message already held.
	if rep != (uma.ImportReport{Imported: 3, Skipped: 4, Complete: true}) {
		t.Fatalf("report = %+v, want 3 imported, 4 skipped, complete", rep)
	}
	if got := env.providerIDs(env.conv.Ref); !slices.Equal(got, []string{"pm-live", "pm-1", "pm-2", "pm-3"}) {
		t.Fatalf("conversation messages = %v, want the live one then the window, once each", got)
	}
	if n := env.received() - head; n != 3 {
		t.Fatalf("%d message.received events, want one per imported message", n)
	}
	for _, r := range env.provider.requests() {
		if r.Route == nil || *r.Route != env.conv.ReplyRoute || !r.Since.Equal(env.start.Add(-24*time.Hour)) || r.Limit < 1 {
			t.Fatalf("provider asked %+v, want the conversation's route and the window", r)
		}
	}
	again, err := env.importNow(env.importReq(nil))
	if err != nil || again != (uma.ImportReport{Skipped: 7, Complete: true}) {
		t.Fatalf("second run = %+v, %v; want nothing imported, all 7 skipped", again, err)
	}
	if got := env.providerIDs(env.conv.Ref); len(got) != 4 {
		t.Fatalf("conversation holds %d messages after the second run, want 4", len(got))
	}
}

// received counts the tenant's message.received events.
func (env *historyEnv) received() int {
	env.t.Helper()
	n := 0
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		after, err := r.EventBase(env.ctx)
		if err != nil {
			return err
		}
		for {
			p, err := r.Events(env.ctx, model.Filter{Kinds: []string{model.EventMessageReceived}}, after, 100)
			if err != nil {
				return err
			}
			n += len(p.Items)
			if p.Complete || len(p.Items) == 0 {
				return nil
			}
			after = p.Next
		}
	})
	if err != nil {
		env.t.Fatal(err)
	}
	return n
}

// historyBounded: a call reads at most Limit envelopes, skipped ones
// included; Next resumes where it stopped, and the last call completes.
func historyBounded(t *testing.T, env *historyEnv) {
	at := env.start.Add(-3 * time.Hour)
	for i, id := range []string{"pm-1", "pm-2", "pm-3", "pm-4", "pm-5"} {
		env.hold(env.message("room-1", id, at.Add(time.Duration(i)*time.Minute)))
	}
	first, err := env.importNow(env.importReq(func(r *uma.ImportRequest) { r.Limit = 3 }))
	if err != nil || first.Imported != 3 || first.Complete || first.Next == "" {
		t.Fatalf("first call = %+v, %v; want 3 imported and a cursor", first, err)
	}
	rest, err := env.importNow(env.importReq(func(r *uma.ImportRequest) { r.Limit = 3; r.After = first.Next }))
	if err != nil || rest != (uma.ImportReport{Imported: 2, Complete: true}) {
		t.Fatalf("resumed call = %+v, %v; want the other 2, complete", rest, err)
	}
	if got := env.providerIDs(env.conv.Ref); !slices.Equal(got, []string{"pm-live", "pm-1", "pm-2", "pm-3", "pm-4", "pm-5"}) {
		t.Fatalf("conversation messages = %v", got)
	}
	// A page larger than what is left of the limit is cut: Next names the
	// same page again (here the first, the empty cursor), and what was
	// read from it is skipped next time.
	env.provider.mu.Lock()
	env.provider.pageSize, env.provider.overfill = 3, true
	env.provider.mu.Unlock()
	cut, err := env.importNow(env.importReq(func(r *uma.ImportRequest) { r.Limit = 1 }))
	if err != nil || cut != (uma.ImportReport{Skipped: 1}) {
		t.Fatalf("cut call = %+v, %v; want 1 skipped and the first page to read again", cut, err)
	}
}

// historyChannel: a channel import asks for the whole channel and fills
// every conversation its messages select, creating one for a room ingress
// had not seen.
func historyChannel(t *testing.T, env *historyEnv) {
	at := env.start.Add(-3 * time.Hour)
	env.hold(env.message("room-1", "pm-1", at), env.message("room-2", "pm-2", at.Add(time.Minute)))
	rep, err := env.importNow(env.importReq(func(r *uma.ImportRequest) { r.Conversation, r.Channel = nil, &env.channel }))
	if err != nil || rep != (uma.ImportReport{Imported: 2, Complete: true}) {
		t.Fatalf("channel import = %+v, %v; want both imported", rep, err)
	}
	if r := env.provider.requests(); len(r) == 0 || r[0].Route != nil {
		t.Fatalf("provider asked %+v, want the whole channel", r)
	}
	if got := env.providerIDs(env.conv.Ref); !slices.Equal(got, []string{"pm-live", "pm-1"}) {
		t.Fatalf("room-1 messages = %v", got)
	}
	var convs []model.Conversation
	err = env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		p, err := r.Conversations(env.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{env.inbox}}, Page: model.PageRequest{Limit: 10}})
		convs = p.Items
		return err
	})
	if err != nil || len(convs) != 2 {
		t.Fatalf("conversations = %d, %v; want room-1 and a new room-2", len(convs), err)
	}
}

// historyGaps: a page the provider marks as missing part of the window
// counts as a gap; what it did return is imported.
func historyGaps(t *testing.T, env *historyEnv) {
	at := env.start.Add(-3 * time.Hour)
	env.hold(env.message("room-1", "pm-1", at), env.message("room-1", "pm-2", at.Add(time.Minute)), env.message("room-1", "pm-3", at.Add(2*time.Minute)))
	env.provider.mu.Lock()
	env.provider.gapAt = 2
	env.provider.mu.Unlock()
	rep, err := env.importNow(env.importReq(nil))
	if err != nil || rep != (uma.ImportReport{Imported: 3, Gaps: 1, Complete: true}) {
		t.Fatalf("report = %+v, %v; want 3 imported and 1 gap", rep, err)
	}
}

// historyMisbehaving: pages without envelopes cannot keep a call going
// past its limit, and a cursor that does not advance ends it with a
// permanent error.
func historyMisbehaving(t *testing.T, env *historyEnv) {
	env.provider.mu.Lock()
	env.provider.endless = true
	env.provider.mu.Unlock()
	rep, err := env.importNow(env.importReq(func(r *uma.ImportRequest) { r.Limit = 3 }))
	if err != nil || rep.Complete || rep.Next == "" || len(env.provider.requests()) != 3 {
		t.Fatalf("endless empty pages = %+v, %v after %d reads; want 3 reads and a cursor", rep, err, len(env.provider.requests()))
	}
	at := env.start.Add(-3 * time.Hour)
	env.hold(env.message("room-1", "pm-1", at), env.message("room-1", "pm-2", at.Add(time.Minute)), env.message("room-1", "pm-3", at.Add(2*time.Minute)))
	env.provider.mu.Lock()
	env.provider.endless, env.provider.stall, env.provider.pageSize = false, true, 1
	env.provider.mu.Unlock()
	_, err = env.importNow(env.importReq(func(r *uma.ImportRequest) { r.After = "1" }))
	serviceCode(t, err, model.Permanent)
}

// historyUnauthorized: a requester without channel.import, or a service
// principal without channel.receive, is refused before the provider is
// asked, and nothing is imported.
func historyUnauthorized(t *testing.T, env *historyEnv) {
	env.hold(env.message("room-1", "pm-1", env.start.Add(-3*time.Hour)))
	env.policy.denyTo(historyStaff, model.ActionChannelImport)
	_, err := env.importNow(env.importReq(nil))
	serviceCode(t, err, model.Unauthorized)
	env.policy.denyTo(historyStaff, "")
	env.policy.denyTo(env.service.Principal.Actor, model.ActionChannelReceive)
	_, err = env.importNow(env.importReq(nil))
	serviceCode(t, err, model.Unauthorized)
	if r := env.provider.requests(); len(r) != 0 {
		t.Fatalf("provider asked %d times by refused imports", len(r))
	}
	if got := env.providerIDs(env.conv.Ref); len(got) != 1 {
		t.Fatalf("refused imports recorded messages: %v", got)
	}
}

// historyBadRequests: malformed requests are refused before any read, and
// references outside the tenant are not found.
func historyBadRequests(t *testing.T, env *historyEnv) {
	foreignConv := model.ConversationRef{Inbox: model.InboxRef{Tenant: "conformance-other", ID: env.inbox.ID}, ID: env.conv.Ref.ID}
	foreignChan := model.ChannelRef{Tenant: "conformance-other", ID: env.channel.ID}
	unknown := model.ConversationRef{Inbox: env.inbox, ID: "00000000-0000-7000-8000-00000000abcd"}
	for name, c := range map[string]struct {
		mut  func(*uma.ImportRequest)
		code model.ErrorCode
	}{
		"no scope":             {func(r *uma.ImportRequest) { r.Conversation = nil }, model.Validation},
		"both scopes":          {func(r *uma.ImportRequest) { r.Channel = &env.channel }, model.Validation},
		"no since":             {func(r *uma.ImportRequest) { r.Since = time.Time{} }, model.Validation},
		"negative limit":       {func(r *uma.ImportRequest) { r.Limit = -1 }, model.Validation},
		"no requester":         {func(r *uma.ImportRequest) { r.Requester = "" }, model.Validation},
		"foreign conversation": {func(r *uma.ImportRequest) { r.Conversation = &foreignConv }, model.NotFound},
		"foreign channel":      {func(r *uma.ImportRequest) { r.Conversation, r.Channel = nil, &foreignChan }, model.NotFound},
		"unknown conversation": {func(r *uma.ImportRequest) { r.Conversation = &unknown }, model.NotFound},
	} {
		_, err := env.importNow(env.importReq(c.mut))
		if model.CodeOf(err) != c.code {
			t.Errorf("%s: Import error = %v, want %s", name, err, c.code)
		}
	}
	if r := env.provider.requests(); len(r) != 0 {
		t.Fatalf("provider asked %d times by bad requests", len(r))
	}
}

// historyUndeclared: an adapter that does not declare history is refused
// unsupported, before any read.
func historyUndeclared(t *testing.T, factory StoreFactory) {
	env := newMutationEnv(t, factory)
	conv := env.target.Ref.Conversation
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "worker"}, Call: model.Call{Correlation: "import"}}
	_, err := env.engine.Runtime().Import(env.ctx, call, uma.ImportRequest{Requester: mutationActor, Conversation: &conv, Since: env.now().Add(-time.Hour)})
	serviceCode(t, err, model.Unsupported)
}

// historyStager: every read is offered the channel's attachment staging, the
// sink's own: bytes staged through it land in the inbox the destination
// resolves to, once per key however often they are staged, and a
// destination of no inbox is refused.
func historyStager(t *testing.T, env *historyEnv) {
	env.hold(env.message("room-1", "pm-1", env.start.Add(-3*time.Hour)))
	if _, err := env.importNow(env.importReq(nil)); err != nil {
		t.Fatalf("Import: %v", err)
	}
	reqs := env.provider.requests()
	if len(reqs) == 0 {
		t.Fatal("the provider was not asked")
	}
	for _, r := range reqs {
		if r.Stager == nil {
			t.Fatalf("read %+v offered no stager", r)
		}
	}
	req := adapter.StageRequest{Key: "att-1", Destination: historyDest, Metadata: model.AttachmentMetadata{
		Name: "a.txt", MediaType: "text/plain", Audience: model.Audience{Visibility: model.Public},
	}}
	first, err := reqs[0].Stager.StageAttachment(env.ctx, req, strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("StageAttachment: %v", err)
	}
	if !first.In(env.inbox) {
		t.Errorf("staged %+v, want it in the destination's inbox %v", first, env.inbox)
	}
	again, err := reqs[len(reqs)-1].Stager.StageAttachment(env.ctx, req, strings.NewReader("bytes"))
	if err != nil || again != first {
		t.Errorf("restaged = %+v, %v; want the same attachment %+v", again, err, first)
	}
	req.Destination = "nowhere"
	if _, err := reqs[0].Stager.StageAttachment(env.ctx, req, strings.NewReader("bytes")); model.CodeOf(err) != model.NotFound {
		t.Errorf("staging for a destination of no inbox = %v, want not found", err)
	}
}
