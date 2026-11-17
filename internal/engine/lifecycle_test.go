package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store/memory"
)

func TestCloseIsIdempotentAndStopsNewWork(t *testing.T) {
	e := enginetest.MustNew(t, enginetest.ValidConfig())
	c, err := e.For(enginetest.PrincipalA())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := e.Close(ctx); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	_, err = e.For(enginetest.PrincipalA())
	enginetest.RequireCode(t, err, model.Unavailable, "engine closed")

	r := conversationRef(enginetest.TenantA, enginetest.InboxOne)
	enginetest.RequireCode(t, c.Authorize(ctx, readConversation(r), r), model.Unavailable, "engine closed")
	// Metadata stays readable after Close: it performs no work.
	_ = e.Describe()
}

func TestClosedErrorAdvisesNoRetry(t *testing.T) {
	e := enginetest.MustNew(t, enginetest.ValidConfig())
	_ = e.Close(context.Background())
	_, err := e.For(enginetest.PrincipalA())
	var me *model.Error
	if !errors.As(err, &me) || me.Retry != model.NoRetry {
		t.Fatalf("err = %#v, want retry advice none", err)
	}
}

// Close waits for in-flight work only until its context ends, then returns
// the context error; a later Close completes once the work finishes.
func TestCloseHonoursDeadline(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	cfg := enginetest.ValidConfig()
	cfg.Authorizer = policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
		close(entered)
		<-release
		return policy.Decision{Allow: true}, nil
	})
	e := enginetest.MustNew(t, cfg)
	c, err := e.For(enginetest.PrincipalA())
	if err != nil {
		t.Fatal(err)
	}
	r := conversationRef(enginetest.TenantA, enginetest.InboxOne)
	done := make(chan error, 1)
	go func() { done <- c.Authorize(context.Background(), readConversation(r), r) }()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := e.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v past a 20ms deadline", d)
	}
	_, err = e.For(enginetest.PrincipalA())
	enginetest.RequireCode(t, err, model.Unavailable, "engine closed")

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("in-flight work: %v", err)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("Close after work finished: %v", err)
	}
}

func TestCloseNeverClosesHostResources(t *testing.T) {
	log := &enginetest.CloseLog{}
	base := enginetest.ValidConfig()
	cfg := engine.Config{
		Store:                enginetest.OwnedStore{Store: memory.New(memory.Options{}), Log: log},
		Authorizer:           &enginetest.OwnedAuthorizer{Log: log},
		IdentityVerifiers:    map[string]policy.IdentityVerifier{"attestation": enginetest.OwnedVerifier{Log: log}},
		Selectors:            map[string]policy.ConversationSelector{"default": enginetest.OwnedSelector{Log: log}},
		InteractionValidator: enginetest.OwnedValidator{Log: log},
		Adapters:             []adapter.Factory{enginetest.OwnedFactory{Factory: enginetest.NewFactory(enginetest.ReceiveDescriptor(enginetest.ThirdPartyKey)), Log: log}},
		Credentials:          enginetest.OwnedCredentials{Log: log},
		HTTP:                 enginetest.OwnedHTTP{Log: log},
		Clock:                enginetest.OwnedClock{Clock: base.Clock, Log: log},
		IDs:                  enginetest.OwnedIDs{IDs: base.IDs, Log: log},
		Blobs:                enginetest.OwnedBlobs{Log: log},
		Observer:             enginetest.OwnedObserver{Log: log},
	}
	e := enginetest.MustNew(t, cfg)
	if _, err := e.For(enginetest.PrincipalA()); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := log.All(); len(got) != 0 {
		t.Fatalf("Close closed host-owned resources: %v", got)
	}
}
