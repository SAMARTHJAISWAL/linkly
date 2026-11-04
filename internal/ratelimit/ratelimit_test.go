package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestAllowRefill(t *testing.T) {
	// Each step advances the clock, then makes one request.
	type step struct {
		advance   time.Duration
		wantOK    bool
		wantRetry time.Duration
	}
	tests := []struct {
		name  string
		rate  float64
		burst int
		steps []step
	}{
		{
			name: "burst then refuse", rate: 1, burst: 3,
			steps: []step{
				{0, true, 0}, {0, true, 0}, {0, true, 0},
				{0, false, time.Second},
			},
		},
		{
			name: "partial refill shortens the wait", rate: 1, burst: 1,
			steps: []step{
				{0, true, 0},
				{250 * time.Millisecond, false, 750 * time.Millisecond},
				{500 * time.Millisecond, false, 250 * time.Millisecond},
				{250 * time.Millisecond, true, 0},
			},
		},
		{
			name: "refill is capped at burst", rate: 10, burst: 2,
			steps: []step{
				{0, true, 0}, {0, true, 0},
				{time.Hour, true, 0}, {0, true, 0},
				{0, false, 100 * time.Millisecond},
			},
		},
		{
			name: "fractional rate", rate: 0.5, burst: 1,
			steps: []step{
				{0, true, 0},
				{time.Second, false, time.Second},
				{time.Second, true, 0},
			},
		},
		{
			name: "refused requests do not spend tokens", rate: 1, burst: 1,
			steps: []step{
				{0, true, 0},
				{0, false, time.Second}, {0, false, time.Second}, {0, false, time.Second},
				{time.Second, true, 0},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := newFakeClock()
			l := New(tt.rate, tt.burst, time.Minute, WithClock(clk.Now))
			for i, s := range tt.steps {
				clk.Advance(s.advance)
				ok, retry := l.Allow("k")
				if ok != s.wantOK || retry != s.wantRetry {
					t.Fatalf("step %d: Allow = (%v, %v), want (%v, %v)", i, ok, retry, s.wantOK, s.wantRetry)
				}
			}
		})
	}
}

func TestKeysAreIndependent(t *testing.T) {
	clk := newFakeClock()
	l := New(1, 1, time.Minute, WithClock(clk.Now))
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("first request for a refused")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("second request for a allowed")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("b was limited by a's bucket")
	}
}

func TestSweepEvictsIdleBuckets(t *testing.T) {
	clk := newFakeClock()
	l := New(1, 5, time.Minute, WithClock(clk.Now))

	l.Allow("old")
	clk.Advance(30 * time.Second)
	l.Allow("recent")

	if n := l.Sweep(); n != 0 {
		t.Fatalf("Sweep evicted %d buckets before any were idle", n)
	}
	clk.Advance(31 * time.Second) // old idle 61s, recent idle 31s
	if n := l.Sweep(); n != 1 {
		t.Fatalf("Sweep evicted %d, want 1", n)
	}
	if l.Len() != 1 {
		t.Fatalf("Len = %d, want 1", l.Len())
	}
	clk.Advance(time.Minute)
	l.Sweep()
	if l.Len() != 0 {
		t.Fatalf("Len = %d, want 0", l.Len())
	}
}

// Evicting a bucket that has not yet refilled would hand its client a fresh
// burst, so the TTL is stretched to at least the full-refill time.
func TestTTLNeverShorterThanRefill(t *testing.T) {
	clk := newFakeClock()
	l := New(0.1, 10, time.Second, WithClock(clk.Now)) // refill takes 100s
	for range 10 {
		l.Allow("k")
	}
	clk.Advance(50 * time.Second)
	if n := l.Sweep(); n != 0 {
		t.Fatalf("evicted a bucket that was only half refilled")
	}
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("expected one of the five refilled tokens")
	}
	clk.Advance(101 * time.Second)
	if n := l.Sweep(); n != 1 {
		t.Fatalf("Sweep evicted %d, want 1 after full refill", n)
	}
}

func TestRunSweepsUntilCancelled(t *testing.T) {
	clk := newFakeClock()
	l := New(1, 1, time.Millisecond, WithClock(clk.Now))
	l.Allow("k")
	clk.Advance(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx, time.Millisecond); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for l.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not sweep")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestConcurrentAllow(t *testing.T) {
	l := New(0.001, 100, time.Minute) // effectively no refill during the test
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if ok, _ := l.Allow("shared"); ok {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	if allowed != 100 {
		t.Fatalf("allowed %d requests, want exactly the burst of 100", allowed)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want int
	}{
		{0, 1},
		{time.Millisecond, 1},
		{time.Second, 1},
		{1001 * time.Millisecond, 2},
		{90 * time.Second, 90},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.in), func(t *testing.T) {
			if got := RetryAfterSeconds(tt.in); got != tt.want {
				t.Fatalf("RetryAfterSeconds(%v) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func BenchmarkAllow(b *testing.B) {
	l := New(1e9, 1e9, time.Minute)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.Allow("10.0.0.1")
		}
	})
}

func TestPeekDoesNotSpend(t *testing.T) {
	clk := newFakeClock()
	l := New(1, 2, time.Minute, WithClock(clk.Now))
	for range 3 {
		if ok, _ := l.Peek("a"); !ok {
			t.Fatal("Peek on a fresh key should allow")
		}
	}
	if l.Len() != 0 {
		t.Fatalf("Peek created %d buckets", l.Len())
	}
	l.Allow("a")
	l.Allow("a")
	ok, wait := l.Peek("a")
	if ok || wait != time.Second {
		t.Fatalf("Peek on an empty bucket = %v, %v; want false, 1s", ok, wait)
	}
	clk.Advance(time.Second)
	if ok, _ := l.Peek("a"); !ok {
		t.Fatal("Peek should see the refilled token")
	}
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("Peek must not have spent the refilled token")
	}
}

func TestAutoSweep(t *testing.T) {
	clk := newFakeClock()
	l := New(1, 1, time.Minute, WithClock(clk.Now), WithAutoSweep(time.Minute))
	for i := range 100 {
		l.Allow(fmt.Sprint(i))
	}
	clk.Advance(2 * time.Minute)
	l.Allow("fresh") // sweeps the idle buckets inline
	if n := l.Len(); n != 1 {
		t.Fatalf("Len after auto-sweep = %d, want 1", n)
	}
}
