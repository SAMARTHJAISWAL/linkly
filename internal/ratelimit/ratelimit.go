// Package ratelimit implements per-key token-bucket rate limiting with
// eviction of idle buckets, so memory stays bounded by the number of
// recently active clients rather than every client ever seen.
package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"
)

// Limiter holds one token bucket per key. Each bucket starts full with
// Burst tokens, refills continuously at Rate tokens per second and each
// allowed request spends one token. The zero value is not usable; use New.
type Limiter struct {
	rate  float64 // tokens per second
	burst float64
	ttl   time.Duration
	now   func() time.Time

	sweepEvery time.Duration // >0: Allow sweeps inline this often (WithAutoSweep)

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time // when tokens was last brought up to date
}

// Option customises a Limiter.
type Option func(*Limiter)

// WithClock replaces time.Now, which lets tests control refill timing.
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) { l.now = now }
}

// WithAutoSweep makes Allow evict idle buckets itself, at most once per
// interval, for limiters that have no goroutine running Run. Memory then
// stays bounded without a lifecycle to manage.
func WithAutoSweep(interval time.Duration) Option {
	return func(l *Limiter) { l.sweepEvery = interval }
}

// New returns a limiter allowing rate requests per second with bursts of
// up to burst. Buckets idle for longer than idleTTL are dropped by Sweep.
//
// A bucket that has been idle long enough to refill completely is
// indistinguishable from a brand-new one, so the effective TTL is never
// shorter than the full-refill time: eviction can never hand a client
// extra tokens.
func New(rate float64, burst int, idleTTL time.Duration, opts ...Option) *Limiter {
	l := &Limiter{
		rate:    rate,
		burst:   float64(burst),
		ttl:     idleTTL,
		now:     time.Now,
		buckets: make(map[string]*bucket),
	}
	if rate > 0 {
		if refill := time.Duration(float64(burst) / rate * float64(time.Second)); refill > l.ttl {
			l.ttl = refill
		}
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Allow spends a token from key's bucket. When the bucket is empty it
// returns false and how long the caller must wait for the next token.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.sweepEvery > 0 && now.Sub(l.lastSweep) >= l.sweepEvery {
		l.sweepLocked(now)
		l.lastSweep = now
	}

	b, found := l.buckets[key]
	if !found {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed.Seconds()*l.rate)
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, l.wait(b.tokens)
}

// Peek reports what Allow would answer for key without spending a token
// or creating a bucket.
func (l *Limiter) Peek(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, found := l.buckets[key]
	if !found {
		return l.burst >= 1, 0
	}
	tokens := b.tokens
	if elapsed := now.Sub(b.last); elapsed > 0 {
		tokens = math.Min(l.burst, tokens+elapsed.Seconds()*l.rate)
	}
	if tokens >= 1 {
		return true, 0
	}
	return false, l.wait(tokens)
}

// wait is how long a bucket holding tokens (< 1) needs to earn one.
func (l *Limiter) wait(tokens float64) time.Duration {
	if l.rate <= 0 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration((1 - tokens) / l.rate * float64(time.Second))
}

// Sweep evicts buckets that have been idle for longer than the TTL and
// reports how many were removed.
func (l *Limiter) Sweep() int {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sweepLocked(now)
}

func (l *Limiter) sweepLocked(now time.Time) int {
	cutoff := now.Add(-l.ttl)
	n := 0
	for k, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, k)
			n++
		}
	}
	return n
}

// Len reports the number of tracked buckets.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Run calls Sweep every interval until ctx is cancelled.
func (l *Limiter) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Sweep()
		}
	}
}

// RetryAfterSeconds converts a wait into a Retry-After header value,
// rounding up so a client that honours it is never refused again for the
// same reason.
func RetryAfterSeconds(d time.Duration) int {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		return 1
	}
	return s
}
