package teams

import (
	"context"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/teams/botauth"
	"hop.top/uma/model"
)

// tokenStub is a TokenSource that answers a stale token until asked for a
// fresh one.
type tokenStub struct {
	mu    sync.Mutex
	asked []bool
	stale bool
	err   error
}

func (s *tokenStub) Token(_ context.Context, fresh bool) (botauth.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, fresh)
	if s.err != nil {
		return botauth.Token{}, s.err
	}
	v := testToken
	if s.stale && !fresh {
		v = "stale-token-not-real"
	}
	return botauth.Token{Value: v, Expiry: testNow.Add(time.Hour)}, nil
}

func openWithSource(t *testing.T, src TokenSource) *rig {
	t.Helper()
	r := newRig(t, func(d *adapter.Dependencies) { d.Credentials = nil })
	inst, err := NewFactory(Options{OpenIDMetadataURL: fakeMetadataURL, Tokens: func(model.Channel) TokenSource { return src }}).
		Open(t.Context(), model.Channel{Ref: testChannel, Type: Type, Configuration: testConfig(r.f).Extension()},
			adapter.Dependencies{HTTP: r.f.client(), Clock: r.clock})
	if err != nil {
		t.Fatal(err)
	}
	r.x = inst.(*Instance)
	r.seed(testGroupChat)
	return r
}

func TestTokenSourceSuppliesTheConnectorToken(t *testing.T) {
	src := &tokenStub{}
	r := openWithSource(t, src)
	if _, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testGroupChat)), Content: text("hi")}); err != nil {
		t.Fatalf("Send without a credentials port: %v", err)
	}
	if c := r.lastCall(t); c.Token != testToken {
		t.Fatalf("token on the wire = %q", c.Token)
	}
}

func TestTokenSourceAskedFreshAfterA401(t *testing.T) {
	src := &tokenStub{stale: true}
	r := openWithSource(t, src)
	out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testGroupChat)), Content: text("hi")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send: %+v, %v", out, err)
	}
	if len(src.asked) != 2 || src.asked[0] || !src.asked[1] {
		t.Fatalf("asked fresh = %v, want [false true]", src.asked)
	}
}

func TestTokenSourceFailureSurfaces(t *testing.T) {
	src := &tokenStub{err: model.NewError(model.Unauthorized, "refused")}
	r := openWithSource(t, src)
	_, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testGroupChat)), Content: text("hi")})
	if me, ok := err.(*model.Error); !ok || me.Code != model.Unauthorized {
		t.Fatalf("err = %v", err)
	}
	if len(r.f.calls(testPrefix)) != 0 {
		t.Fatal("a Connector call was made without a token")
	}
}

func TestNilTokensAnswerKeepsCredentialsPort(t *testing.T) {
	r := newRig(t)
	f := NewFactory(Options{OpenIDMetadataURL: fakeMetadataURL, Tokens: func(model.Channel) TokenSource { return nil }})
	inst, err := f.Open(t.Context(), model.Channel{Ref: testChannel, Type: Type, Configuration: testConfig(r.f).Extension(), CredentialRef: "teams"},
		adapter.Dependencies{Credentials: r.creds, HTTP: r.f.client(), Clock: r.clock})
	if err != nil {
		t.Fatal(err)
	}
	if inst.(*Instance).src != nil {
		t.Fatal("a nil source was kept as non-nil")
	}
}
