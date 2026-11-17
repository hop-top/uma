package conformance

import (
	"errors"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// historyWindowCases run Runtime.Import over the scripted provider for the
// window's upper bound and for a reader that fails mid-import.
var historyWindowCases = []struct {
	name string
	run  func(*testing.T, *historyEnv)
}{
	{"UntilEndsTheWindowExclusively", historyUntil},
	{"UntilNotAfterSinceIsValidation", historyUntilInvalid},
	{"ReaderErrorCarriesNext", historyReaderError},
}

// historyUntil: Until reaches the provider as given and bounds the window
// half-open: a message at Until is out, one just before it is in.
func historyUntil(t *testing.T, env *historyEnv) {
	at := env.start.Add(-3 * time.Hour)
	env.hold(
		env.message("room-1", "pm-1", at),
		env.message("room-1", "pm-2", at.Add(time.Minute)),
		env.message("room-1", "pm-3", at.Add(time.Minute+time.Nanosecond)),
	)
	until := at.Add(time.Minute + time.Nanosecond)
	rep, err := env.importNow(env.importReq(func(r *uma.ImportRequest) { r.Until = until }))
	if err != nil || rep != (uma.ImportReport{Imported: 2, Complete: true}) {
		t.Fatalf("bounded import = %+v, %v; want pm-1 and pm-2, complete", rep, err)
	}
	for _, r := range env.provider.requests() {
		if !r.Until.Equal(until) {
			t.Fatalf("provider asked %+v, want Until %v", r, until)
		}
	}
	rest, err := env.importNow(env.importReq(nil))
	if err != nil || rest != (uma.ImportReport{Imported: 1, Skipped: 2, Complete: true}) {
		t.Fatalf("open import = %+v, %v; want only pm-3 new", rest, err)
	}
}

// historyUntilInvalid: an Until at or before Since is refused before any
// read.
func historyUntilInvalid(t *testing.T, env *historyEnv) {
	since := env.start.Add(-24 * time.Hour)
	for name, until := range map[string]time.Time{"equal": since, "before": since.Add(-time.Second)} {
		_, err := env.importNow(env.importReq(func(r *uma.ImportRequest) { r.Until = until }))
		if model.CodeOf(err) != model.Validation {
			t.Errorf("until %s since: Import error = %v, want validation", name, err)
		}
	}
	if r := env.provider.requests(); len(r) != 0 {
		t.Fatalf("provider asked %d times by bad requests", len(r))
	}
}

// historyReaderError: a reader that fails returns its error as it is,
// RetryAfter included, and the report carries the failed page's cursor, so
// a later call resumes there; the engine does not retry.
func historyReaderError(t *testing.T, env *historyEnv) {
	at := env.start.Add(-3 * time.Hour)
	for i, id := range []string{"pm-1", "pm-2", "pm-3", "pm-4", "pm-5"} {
		env.hold(env.message("room-1", id, at.Add(time.Duration(i)*time.Minute)))
	}
	env.provider.mu.Lock()
	env.provider.failAt, env.provider.failErr = "2", &model.Error{Code: model.RateLimited, SafeMessage: "slow down", RetryAfter: 3 * time.Second}
	env.provider.mu.Unlock()
	rep, err := env.importNow(env.importReq(nil))
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.RateLimited || me.RetryAfter != 3*time.Second {
		t.Fatalf("Import error = %v, want the reader's rate limit with its RetryAfter", err)
	}
	if rep.Imported != 2 || rep.Next != "2" || rep.Complete {
		t.Fatalf("report = %+v, want 2 imported and the failed page's cursor", rep)
	}
	if n := len(env.provider.requests()); n != 2 {
		t.Fatalf("provider asked %d times, want 2 and no retry", n)
	}
	env.provider.mu.Lock()
	env.provider.failErr = nil
	env.provider.mu.Unlock()
	rest, err := env.importNow(env.importReq(func(r *uma.ImportRequest) { r.After = rep.Next }))
	if err != nil || rest != (uma.ImportReport{Imported: 3, Complete: true}) {
		t.Fatalf("resumed import = %+v, %v; want the other 3", rest, err)
	}
}

// historySenderChosen: for an adapter whose message IDs the sender
// chooses, as mail does, a message whose ID the inbox holds under other
// bytes is imported, and so is one with no message ID; neither is deduped
// by ID. A second run imports nothing.
func historySenderChosen(t *testing.T, factory StoreFactory) {
	env := newHistoryEnvIDs(t, factory, adapter.MessageIDsSender)
	at := env.start.Add(-3 * time.Hour)
	repeated := env.message("room-1", "pm-live", at)
	repeated.Content.Parts[0].Text = "other bytes under the same Message-ID"
	env.hold(repeated, env.message("room-1", "", at.Add(time.Minute)))
	rep, err := env.importNow(env.importReq(nil))
	if err != nil || rep != (uma.ImportReport{Imported: 2, Complete: true}) {
		t.Fatalf("import = %+v, %v; want both imported", rep, err)
	}
	if n := env.messageCount(); n != 3 {
		t.Fatalf("conversation holds %d messages, want the live one and both imported", n)
	}
	again, err := env.importNow(env.importReq(nil))
	if err != nil || again != (uma.ImportReport{Skipped: 2, Complete: true}) {
		t.Fatalf("second run = %+v, %v; want nothing imported", again, err)
	}
	if n := env.messageCount(); n != 3 {
		t.Fatalf("conversation holds %d messages after the second run, want 3", n)
	}
}

// historySenderChosenNoIdentity: an envelope with neither a message ID nor
// an event ID names nothing and is skipped, under any authority.
func historySenderChosenNoIdentity(t *testing.T, factory StoreFactory) {
	env := newHistoryEnvIDs(t, factory, adapter.MessageIDsSender)
	nameless := env.message("room-1", "", env.start.Add(-3*time.Hour))
	nameless.EventID = ""
	env.provider.mu.Lock()
	env.provider.held = append(env.provider.held, nameless)
	env.provider.mu.Unlock()
	rep, err := env.importNow(env.importReq(nil))
	if err != nil || rep != (uma.ImportReport{Skipped: 1, Complete: true}) {
		t.Fatalf("import = %+v, %v; want the nameless envelope skipped", rep, err)
	}
}

// messageCount is the number of messages the conversation holds.
func (env *historyEnv) messageCount() int {
	env.t.Helper()
	conv := env.conv.Ref
	n := 0
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		p, err := r.Messages(env.ctx, model.ListRequest{Filter: model.Filter{Conversation: &conv}, Page: model.PageRequest{Limit: 100}})
		n = len(p.Items)
		return err
	})
	if err != nil {
		env.t.Fatal(err)
	}
	return n
}
