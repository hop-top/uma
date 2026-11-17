package bot_test

import (
	"regexp"
	"slices"
	"testing"

	"hop.top/uma/companions/bot"
	"hop.top/uma/model"
)

// Fallback runs for a message only when no earlier route claims it.
func TestFallbackIsLastMessageRoute(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Patterns = []bot.Pattern{{Name: "ping", Match: regexp.MustCompile(`^ping`), Handle: replying("pong")}}
	cfg.Fallback = replying("fallback")
	b := mustNew(t, cfg)
	c.ev.push(c.message(t, 1, conv(1), "ping"), c.message(t, 2, conv(1), "other"))
	r := step(t, b)
	if got := routes(r); !slices.Equal(got, []bot.Route{bot.RoutePattern, bot.RouteFallback}) {
		t.Fatalf("routes = %v", got)
	}
	if r.Results[1].Handler != "fallback" {
		t.Fatalf("handler = %q, want fallback", r.Results[1].Handler)
	}
	var texts []string
	for _, rep := range c.msgs.sent() {
		texts = append(texts, bot.Text(rep.Content))
	}
	if !slices.Equal(texts, []string{"pong", "fallback"}) {
		t.Fatalf("replies = %v", texts)
	}
}

// Fallback alone is a complete message route set.
func TestFallbackWithoutOtherRoutes(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Fallback = replying("fallback")
	b := mustNew(t, cfg)
	c.ev.push(c.message(t, 1, conv(1), "anything"))
	if got := routes(step(t, b)); !slices.Equal(got, []bot.Route{bot.RouteFallback}) {
		t.Fatalf("routes = %v", got)
	}
}

// Fallback never preempts Direct: a direct exchange keeps its own route.
func TestFallbackDoesNotPreemptDirect(t *testing.T) {
	c := newFake()
	dm := conv(1)
	c.direct(dm)
	cfg := config(c)
	cfg.Direct, cfg.IsDirect = replying("dm"), func(v model.Conversation) bool { return v.Mode == model.ModePerContact }
	cfg.Fallback = replying("fallback")
	b := mustNew(t, cfg)
	c.ev.push(c.message(t, 1, dm, "anything"))
	if got := routes(step(t, b)); !slices.Equal(got, []bot.Route{bot.RouteDirect}) {
		t.Fatalf("routes = %v", got)
	}
}

// Fallback does not claim interactions; they keep their own routes.
func TestFallbackDoesNotClaimInteractions(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Commands = map[string]bot.Handler{"deploy": replying("deployed")}
	cfg.Fallback = replying("fallback")
	b := mustNew(t, cfg)
	c.ev.push(c.interaction(t, 1, conv(1), asCommand("deploy")))
	r := step(t, b)
	if r.Results[0].Route != bot.RouteCommand {
		t.Fatalf("route = %q, want %q (result %+v)", r.Results[0].Route, bot.RouteCommand, r.Results[0])
	}
	if r.Results[0].Handler != "command/deploy" {
		t.Fatalf("handler = %q, want command/deploy", r.Results[0].Handler)
	}
}
