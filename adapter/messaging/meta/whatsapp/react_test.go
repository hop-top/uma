package whatsapp_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/model"
)

// react sends a reaction on the message wamid to testUser.
func react(t *testing.T, inst *whatsapp.Instance, wamid, emoji string, remove bool) (adapter.Outcome, error) {
	t.Helper()
	e := effect("00000000-0000-7000-8000-0000000000e2")
	target := model.ProviderMessageRef(testChannel, wamid)
	e.Target = &target
	return inst.React(t.Context(), adapter.ReactRequest{Effect: e, Emoji: emoji, Remove: remove})
}

// The documented reaction message: type reaction, with the wamid of the
// message reacted to and the emoji; no text, no reply context.
func TestReactSendsReactionType(t *testing.T) {
	f := fakeWithAccount(t)
	c := newCreds()
	inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: c})
	out, err := react(t, inst, "wamid.target-1", "👍", false)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0].Namespace != model.ProviderMessageNamespace || !strings.HasPrefix(out.References[0].ID, "wamid.") {
		t.Fatalf("outcome %+v, want provider_accepted naming the reaction message", out)
	}
	reqs := f.recordedRequests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Path != "/v24.0/"+testPNID+"/messages" || r.Auth != "Bearer EAAG-access-token" {
		t.Errorf("path %s auth %q", r.Path, r.Auth)
	}
	b := r.Body
	if b["messaging_product"] != "whatsapp" || b["recipient_type"] != "individual" || b["to"] != testUser || b["type"] != "reaction" {
		t.Errorf("body %v", b)
	}
	rx, _ := b["reaction"].(map[string]any)
	if rx["message_id"] != "wamid.target-1" || rx["emoji"] != "👍" || len(rx) != 2 {
		t.Errorf("reaction %v", b["reaction"])
	}
	if b["text"] != nil || b["context"] != nil || b["template"] != nil {
		t.Errorf("body %v: a reaction carries nothing else", b)
	}
	op, at, ok := whatsapp.ParseCallbackData(b["biz_opaque_callback_data"].(string))
	if !ok || op != "00000000-0000-7000-8000-0000000000e2" || at != "00000000-0000-7000-8000-0000000000a1" {
		t.Errorf("callback data %v, want the attempt", b["biz_opaque_callback_data"])
	}
	if reqsC := c.requests(); len(reqsC) != 1 || reqsC[0].Purpose != whatsapp.PurposeGraph {
		t.Errorf("credential requests %+v", reqsC)
	}
}

// A removal is a reaction message with an empty emoji, whatever emoji the
// request names: the account holds one reaction per message.
func TestReactRemoveSendsEmptyEmoji(t *testing.T) {
	f := fakeWithAccount(t)
	inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: newCreds()})
	out, err := react(t, inst, "wamid.target-1", "👍", true)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("React remove = %+v, %v", out, err)
	}
	rx, _ := f.recordedRequests()[0].Body["reaction"].(map[string]any)
	if emoji, present := rx["emoji"]; !present || emoji != "" || rx["message_id"] != "wamid.target-1" {
		t.Fatalf("reaction %v, want message_id and an empty emoji", rx)
	}
}

// A reaction may target a message the user sent and one the business sent.
func TestReactOnInboundMessage(t *testing.T) {
	f := fakeWithAccount(t)
	inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: newCreds()})
	for _, wamid := range []string{"wamid.HBgLMTY1MDM4Nzk0MzkVAgASGBQzQUFERjg0NDEzNDdFODU3MUMxMAA=", "wamid.HBgLMTY1MDM4Nzk0MzkVAgARGBI0000"} {
		if out, err := react(t, inst, wamid, "❤️", false); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("React on %s = %+v, %v", wamid, out, err)
		}
	}
	reqs := f.recordedRequests()
	if len(reqs) != 2 || reqs[0].Body["reaction"].(map[string]any)["message_id"] == reqs[1].Body["reaction"].(map[string]any)["message_id"] {
		t.Fatalf("requests %+v, want one reaction per message", reqs)
	}
}

// What the adapter cannot address is refused before any call.
func TestReactRefusesWithoutCall(t *testing.T) {
	f := fakeWithAccount(t)
	inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: newCreds()})
	good := effect("op")
	t1 := model.ProviderMessageRef(testChannel, "wamid.t")
	good.Target = &t1
	noTarget := effect("op")
	foreignTarget := effect("op")
	ft := model.ProviderMessageRef(model.ChannelRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-00000000c0b2"}, "wamid.t")
	foreignTarget.Target = &ft
	notMessage := effect("op")
	nm := model.ExternalRef{Channel: testChannel, Namespace: whatsapp.Namespace, Kind: whatsapp.RouteKind, ID: testUser}
	notMessage.Target = &nm
	emptyID := effect("op")
	ei := model.ProviderMessageRef(testChannel, "")
	emptyID.Target = &ei
	foreignRoute := good
	foreignRoute.Route.Channel = model.ChannelRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-00000000c0b2"}
	for name, e := range map[string]adapter.Effect{
		"no target": noTarget, "foreign target": foreignTarget, "not a message": notMessage, "empty id": emptyID, "foreign route": foreignRoute,
	} {
		if _, err := inst.React(t.Context(), adapter.ReactRequest{Effect: e, Emoji: "👍"}); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
	if n := len(f.recordedRequests()); n != 0 {
		t.Fatalf("provider requests = %d, want none", n)
	}
}

// A reaction message is sent inside the customer service window only, as a
// free-form message is; Meta decides when the adapter knows no window.
func TestReactFollowsServiceWindow(t *testing.T) {
	for name, c := range map[string]struct {
		last     time.Duration
		remove   bool
		wantSent bool
	}{
		"no inbound recorded":  {0, false, true},
		"inbound 1h ago":       {time.Hour, false, true},
		"inbound 25h ago":      {25 * time.Hour, false, false},
		"removal 25h ago":      {25 * time.Hour, true, false},
		"removal 23h ago":      {23 * time.Hour, true, true},
		"inbound exactly 24h":  {24 * time.Hour, false, false},
		"inbound 23h59m ago":   {24*time.Hour - time.Minute, false, true},
		"removal, none stored": {0, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := fakeWithAccount(t)
			p := newProtocol()
			if c.last > 0 {
				p.setLastInbound(testUser, testNow.Add(-c.last))
			}
			cr := newCreds()
			inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: cr, Protocol: p})
			out, err := react(t, inst, "wamid.t", "👍", c.remove)
			if err != nil {
				t.Fatal(err)
			}
			if sent := len(f.recordedRequests()) == 1; sent != c.wantSent {
				t.Fatalf("sent = %v, want %v (outcome %+v)", sent, c.wantSent, out)
			}
			if c.wantSent {
				return
			}
			if out.State != model.Rejected || out.Failure == nil || out.Failure.Retry != model.NoRetry || len(cr.requests()) != 0 {
				t.Fatalf("outcome %+v, want rejected before any call", out)
			}
		})
	}
}

// Graph API errors map as for a send: refusals rejected, rate limits and
// refused tokens errors, a server fault outcome unknown.
func TestReactErrorOutcomes(t *testing.T) {
	for name, c := range map[string]struct {
		a     answer
		state model.DeliveryState
		code  model.ErrorCode
	}{
		"invalid target 100":   {answer{400, errBody(100), nil}, model.Rejected, model.Permanent},
		"window closed 131047": {answer{400, errBody(131047), nil}, model.Rejected, model.Permanent},
		"rate 130429":          {answer{400, errBody(130429), nil}, "", model.RateLimited},
		"token 190":            {answer{401, errBody(190), nil}, "", model.Unauthorized},
		"server 131000":        {answer{500, errBody(131000), nil}, "", model.OutcomeUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			inst, _ := answering(t, c.a)
			out, err := react(t, inst, "wamid.t", "👍", false)
			if c.state != "" {
				if err != nil || out.State != c.state || out.Failure == nil || out.Failure.Code != c.code || out.Failure.Retry != model.NoRetry {
					t.Fatalf("React = %+v, %v; want %s %s", out, err, c.state, c.code)
				}
				return
			}
			var me *model.Error
			if !errors.As(err, &me) || me.Code != c.code {
				t.Fatalf("React = %+v, %v; want error %s", out, err, c.code)
			}
		})
	}
	t.Run("answer without message id", func(t *testing.T) {
		inst, _ := answering(t, answer{200, `{"messaging_product":"whatsapp","messages":[]}`, nil})
		if _, err := react(t, inst, "wamid.t", "👍", false); model.CodeOf(err) != model.OutcomeUnknown {
			t.Fatalf("err = %v, want outcome_unknown", err)
		}
	})
}

// Reactions are declared, one per user and message; editing and deleting
// are not: the Cloud API has no endpoint for either.
func TestDescriptorDeclaresReactions(t *testing.T) {
	f := fakeWithAccount(t)
	d := f.factory().Describe()
	found := false
	for _, c := range d.Capabilities {
		found = found || c == model.CapabilityReact
		if c == model.CapabilityEdit || c == model.CapabilityDelete {
			t.Errorf("capability %s declared", c)
		}
	}
	if !found {
		t.Errorf("capabilities %v, want react", d.Capabilities)
	}
	if d.Exchange.Reactions != adapter.ReactionsSingle {
		t.Errorf("reaction cardinality %q, want single: a user holds one reaction per message", d.Exchange.Reactions)
	}
	events := strings.Join(d.Events, " ")
	if !strings.Contains(events, model.EventMessageUpdated) {
		t.Errorf("events %v, want message.updated for reactions", d.Events)
	}
	inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: newCreds()})
	if _, ok := any(inst).(adapter.Editor); ok {
		t.Error("instance implements Editor")
	}
	if _, ok := any(inst).(adapter.Deleter); ok {
		t.Error("instance implements Deleter")
	}
	st, err := inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if s.Name == model.CapabilityReact && !s.Available {
			t.Errorf("react status %+v", s)
		}
	}
}

// Without a client or a credential the instance makes no call.
func TestReactWithoutHTTPClientMakesNoCall(t *testing.T) {
	inst := openInstance(t, whatsapp.NewFactory(whatsapp.Options{}), adapter.Dependencies{Credentials: newCreds()})
	if _, err := react(t, inst, "wamid.t", "👍", false); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("err = %v", err)
	}
}
