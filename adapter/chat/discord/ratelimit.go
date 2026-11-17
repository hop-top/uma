package discord

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"hop.top/uma/model"
)

// Rate limits, as "Rate Limits" documents them
// (https://docs.discord.com/developers/topics/rate-limits):
//
//   - A bot makes at most 50 requests a second across routes (the global
//     limit); interaction endpoints are exempt.
//   - Each route has buckets per top-level resource (a channel, a guild, a
//     webhook), described by the answer's headers: X-RateLimit-Bucket names
//     the bucket, X-RateLimit-Remaining what is left, X-RateLimit-Reset-After
//     the seconds until it refills. "rate limits should not be hard coded".
//   - A 429 carries retry_after (seconds, fractional) and global in its
//     body, and X-RateLimit-Scope (user, global or shared).
//   - 401, 403 and 429 answers count as invalid requests, a 429 of scope
//     shared excepted; 10,000 in 10 minutes restricts the IP address.
//
// The limiter waits for a bucket or the global limit inline when the wait
// is short (maxInlineWait) and otherwise refuses the request before it is
// made, model.RateLimited with the wait, so nothing is applied and the
// engine retries the operation later. A 429 is never retried inline: it is
// recorded and returned. Once the invalid answers of the last ten minutes
// reach invalidCeiling, every request is refused until they age out.

const (
	globalPerSecond = 50
	// maxInlineWait is the longest the limiter sleeps before a request.
	maxInlineWait = time.Second
	// invalidCeiling stays a tenth below Discord's 10,000 invalid requests
	// per 10 minutes.
	invalidCeiling = 9000
	invalidWindow  = 10 * time.Minute
)

type limiter struct {
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	mu sync.Mutex
	// routes maps a route to the bucket Discord named for it; buckets holds
	// each bucket's state per top-level resource.
	routes  map[string]string
	buckets map[string]*bucket
	// globalUntil is when a global 429 ends; second and sent count the
	// requests of the current second.
	globalUntil time.Time
	second      time.Time
	sent        int
	// invalid counts invalid answers per minute of the last ten.
	invalid [10]invalidMinute
}

type bucket struct {
	remaining int
	reset     time.Time
}

type invalidMinute struct {
	minute int64
	n      int
}

func newLimiter(now func() time.Time, sleep func(context.Context, time.Duration) error) *limiter {
	return &limiter{now: now, sleep: sleep, routes: map[string]string{}, buckets: map[string]*bucket{}}
}

// route is a request's rate limit identity: the route (method and path,
// minor IDs and emoji elided), its top-level resource, and whether the
// global limit applies.
type route struct {
	key, major string
	exempt     bool
}

// routeOf derives the route of method and path (relative to the API root).
func routeOf(method, path string) route {
	path, _, _ = strings.Cut(path, "?")
	segs := strings.Split(path, "/")
	r := route{}
	switch {
	case len(segs) >= 2 && (segs[0] == "channels" || segs[0] == "guilds"):
		r.major = segs[0] + "/" + segs[1]
	case len(segs) >= 3 && segs[0] == "webhooks":
		r.major, r.exempt = segs[0]+"/"+segs[1]+"/"+segs[2], true
		segs[2] = "{token}"
	case len(segs) >= 3 && segs[0] == "interactions":
		r.major, r.exempt = segs[0]+"/"+segs[1], true
		segs[2] = "{token}"
	}
	for i := 2; i < len(segs); i++ {
		switch {
		case segs[i-1] == "reactions":
			segs[i] = "{emoji}"
		case isSnowflake(segs[i]):
			segs[i] = "{id}"
		}
	}
	if len(segs) >= 2 && r.major != "" {
		segs[1] = "{major}"
	}
	r.key = method + " " + strings.Join(segs, "/")
	return r
}

// acquire waits for r's turn, or refuses with model.RateLimited.
func (l *limiter) acquire(ctx context.Context, r route) error {
	for {
		l.mu.Lock()
		now := l.now()
		wait := l.blocked(now, r)
		if wait <= 0 {
			l.take(now, r)
			l.mu.Unlock()
			return nil
		}
		invalid := l.invalidCount(now) >= invalidCeiling
		l.mu.Unlock()
		if invalid || wait > maxInlineWait {
			return rateLimited(wait)
		}
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// blocked is how long r must wait at now; zero or less to go.
func (l *limiter) blocked(now time.Time, r route) time.Duration {
	if l.invalidCount(now) >= invalidCeiling {
		return l.invalidAge(now)
	}
	var wait time.Duration
	if !r.exempt {
		wait = l.globalUntil.Sub(now)
		if now.Sub(l.second) < time.Second && l.sent >= globalPerSecond {
			wait = max(wait, l.second.Add(time.Second).Sub(now))
		}
	}
	if b := l.bucketOf(r); b != nil && b.remaining <= 0 && now.Before(b.reset) {
		wait = max(wait, b.reset.Sub(now))
	}
	return wait
}

func (l *limiter) take(now time.Time, r route) {
	if !r.exempt {
		if now.Sub(l.second) >= time.Second {
			l.second, l.sent = now, 0
		}
		l.sent++
	}
	if b := l.bucketOf(r); b != nil && now.Before(b.reset) {
		b.remaining--
	}
}

func (l *limiter) bucketOf(r route) *bucket {
	hash, ok := l.routes[r.key]
	if !ok {
		return nil
	}
	return l.buckets[hash+"|"+r.major]
}

// update records what an answer said about r's limits.
func (l *limiter) update(r route, h http.Header, status int, body []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if hash := strings.TrimSpace(h.Get("X-RateLimit-Bucket")); hash != "" && len(hash) <= 64 {
		b := l.ensureBucket(r, hash)
		if n, err := strconv.Atoi(h.Get("X-RateLimit-Remaining")); err == nil {
			b.remaining = n
		}
		if d := seconds(h.Get("X-RateLimit-Reset-After")); d > 0 {
			b.reset = now.Add(d)
		}
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		l.countInvalid(now)
	case http.StatusTooManyRequests:
		wait, global := retryAfter(h, body)
		if !strings.EqualFold(h.Get("X-RateLimit-Scope"), "shared") {
			l.countInvalid(now)
		}
		if global {
			l.globalUntil = now.Add(wait)
			return
		}
		b := l.bucketOf(r)
		if b == nil {
			// A route Discord named no bucket for: its own.
			b = l.ensureBucket(r, "route:"+r.key)
		}
		b.remaining, b.reset = 0, now.Add(wait)
	}
}

// ensureBucket names hash as r's bucket and returns its state for r's
// top-level resource.
func (l *limiter) ensureBucket(r route, hash string) *bucket {
	if len(l.routes) >= maxBuckets {
		clear(l.routes)
	}
	l.routes[r.key] = hash
	key := hash + "|" + r.major
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxBuckets {
			clear(l.buckets)
		}
		b = &bucket{}
		l.buckets[key] = b
	}
	return b
}

// maxBuckets bounds the route and bucket tables; past it a table starts
// over.
const maxBuckets = 4096

func (l *limiter) countInvalid(now time.Time) {
	m := now.Unix() / 60
	slot := &l.invalid[m%int64(len(l.invalid))]
	if slot.minute != m {
		*slot = invalidMinute{minute: m}
	}
	slot.n++
}

func (l *limiter) invalidCount(now time.Time) int {
	m := now.Unix() / 60
	n := 0
	for _, s := range l.invalid {
		if s.n > 0 && m-s.minute < int64(len(l.invalid)) {
			n += s.n
		}
	}
	return n
}

// invalidAge is how long until the oldest counted minute ages out.
func (l *limiter) invalidAge(now time.Time) time.Duration {
	m := now.Unix() / 60
	oldest := m
	for _, s := range l.invalid {
		if s.n > 0 && m-s.minute < int64(len(l.invalid)) && s.minute < oldest {
			oldest = s.minute
		}
	}
	return time.Unix((oldest+int64(len(l.invalid)))*60, 0).Sub(now)
}

// retryAfter reads a 429's wait: the body's retry_after, else Retry-After;
// and whether the limit is global.
func retryAfter(h http.Header, body []byte) (time.Duration, bool) {
	var b struct {
		RetryAfter float64 `json:"retry_after"`
		Global     bool    `json:"global"`
	}
	_ = json.Unmarshal(body, &b)
	wait := secondsOf(b.RetryAfter)
	if wait <= 0 {
		wait = seconds(h.Get("Retry-After"))
	}
	if wait <= 0 {
		wait = defaultRateWait
	}
	return wait, b.Global || strings.EqualFold(h.Get("X-RateLimit-Global"), "true")
}

// defaultRateWait is the wait asked for when a rate limit states none.
const defaultRateWait = 5 * time.Second

// seconds parses a count of seconds, fractional; zero for anything else.
func seconds(s string) time.Duration {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return secondsOf(f)
}

func secondsOf(f float64) time.Duration {
	if math.IsNaN(f) || f <= 0 || f > 24*3600 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

func rateLimited(wait time.Duration) *model.Error {
	if wait <= 0 {
		wait = defaultRateWait
	}
	return &model.Error{Code: model.RateLimited, SafeMessage: "Discord rate limit", Retry: model.RetrySameOperation, RetryAfter: wait}
}
