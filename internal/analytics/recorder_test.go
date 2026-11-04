package analytics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// memSink records every batch it receives. If gate is non-nil each insert
// waits for it, which lets tests hold the worker still.
type memSink struct {
	mu      sync.Mutex
	batches [][]Click
	gate    chan struct{}
	err     error
}

func (s *memSink) InsertClicks(ctx context.Context, clicks []Click) (int, error) {
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.batches = append(s.batches, append([]Click(nil), clicks...))
	return len(clicks), nil
}

func (s *memSink) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.batches {
		n += len(b)
	}
	return n
}

func (s *memSink) sizes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for _, b := range s.batches {
		out = append(out, len(b))
	}
	return out
}

func click(i int) Click {
	return Click{Code: fmt.Sprintf("c%d", i), At: time.Unix(int64(i), 0)}
}

func TestCloseFlushesPendingClicks(t *testing.T) {
	sink := &memSink{}
	// A flush interval of an hour means nothing is written until Close.
	r := NewRecorder(sink, Options{Buffer: 100, BatchSize: 1000, FlushInterval: time.Hour})
	for i := range 42 {
		if !r.Record(click(i)) {
			t.Fatalf("click %d dropped", i)
		}
	}
	if got := sink.total(); got != 0 {
		t.Fatalf("%d clicks written before Close, want 0", got)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := sink.total(); got != 42 {
		t.Fatalf("after Close sink has %d clicks, want 42", got)
	}
	if r.Recorded() != 42 || r.Dropped() != 0 || r.Pending() != 0 {
		t.Fatalf("recorded=%d dropped=%d pending=%d", r.Recorded(), r.Dropped(), r.Pending())
	}
}

func TestBatchesAreCappedAtBatchSize(t *testing.T) {
	sink := &memSink{}
	r := NewRecorder(sink, Options{Buffer: 100, BatchSize: 10, FlushInterval: time.Hour})
	for i := range 25 {
		r.Record(click(i))
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	sizes := sink.sizes()
	if fmt.Sprint(sizes) != "[10 10 5]" {
		t.Fatalf("batch sizes %v, want [10 10 5]", sizes)
	}
}

func TestFlushIntervalWritesPartialBatches(t *testing.T) {
	sink := &memSink{}
	r := NewRecorder(sink, Options{Buffer: 100, BatchSize: 1000, FlushInterval: 10 * time.Millisecond})
	defer r.Close(context.Background())

	r.Record(click(1))
	r.Record(click(2))
	deadline := time.Now().Add(2 * time.Second)
	for sink.total() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("ticker never flushed the partial batch")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRecordNeverBlocksWhenFull(t *testing.T) {
	sink := &memSink{gate: make(chan struct{})}
	r := NewRecorder(sink, Options{Buffer: 2, BatchSize: 1, FlushInterval: time.Hour})

	// The worker takes the first click and blocks inside the sink; the next
	// two fill the buffer; everything after that must be dropped at once.
	r.Record(click(0))
	waitFor(t, func() bool { return len(r.queue) == 0 })
	accepted := 0
	start := time.Now()
	for i := 1; i <= 10; i++ {
		if r.Record(click(i)) {
			accepted++
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Record blocked for %v with a full buffer", elapsed)
	}
	if r.Pending() != 3 {
		t.Fatalf("Pending = %d, want 3 (one in the sink, two queued)", r.Pending())
	}
	if accepted != 2 || r.Dropped() != 8 {
		t.Fatalf("accepted=%d dropped=%d, want 2 and 8", accepted, r.Dropped())
	}

	close(sink.gate)
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sink.total(); got != 3 {
		t.Fatalf("sink has %d clicks, want 3", got)
	}
}

func TestRecordAfterCloseIsDropped(t *testing.T) {
	r := NewRecorder(&memSink{}, Options{Buffer: 1})
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Record(click(1)) {
		t.Fatal("Record accepted a click after Close")
	}
	if !errors.Is(r.Close(context.Background()), ErrClosed) {
		t.Fatal("second Close should return ErrClosed")
	}
}

func TestCloseHonoursContext(t *testing.T) {
	sink := &memSink{gate: make(chan struct{})}
	defer close(sink.gate)
	r := NewRecorder(sink, Options{Buffer: 10, BatchSize: 1, FlushInterval: time.Hour, WriteTimeout: time.Minute})
	r.Record(click(1))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded", err)
	}
}

func TestSinkErrorsAreCounted(t *testing.T) {
	sink := &memSink{err: errors.New("disk full")}
	r := NewRecorder(sink, Options{Buffer: 10, BatchSize: 10, FlushInterval: time.Hour})
	for i := range 3 {
		r.Record(click(i))
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Failed() != 3 || r.Recorded() != 0 {
		t.Fatalf("failed=%d recorded=%d, want 3 and 0", r.Failed(), r.Recorded())
	}
}

func TestConcurrentRecordAndClose(t *testing.T) {
	sink := &memSink{}
	r := NewRecorder(sink, Options{Buffer: 64, BatchSize: 16, FlushInterval: time.Millisecond})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 200 {
				r.Record(click(g*1000 + i))
			}
		})
	}
	time.Sleep(time.Millisecond)
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	// Every click is accounted for exactly once: written or dropped.
	if got := uint64(sink.total()) + r.Dropped(); got != 1600 {
		t.Fatalf("written+dropped = %d, want 1600", got)
	}
}

func TestRefererHost(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"https://news.ycombinator.com/item?id=1", "news.ycombinator.com"},
		{"http://User:pw@Example.COM:8443/a/b#frag", "example.com"},
		{"https://example.com./", "example.com"},
		{"android-app://com.google.android.gm/", ""},
		{"not a url", ""},
		{"//example.com/path", ""},
		{"http://[::1]:8080/", "::1"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := RefererHost(tt.in); got != tt.want {
				t.Fatalf("RefererHost(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestCloseWaitsForWorkerAfterDeadline: when ctx expires, Close abandons
// what is left but still returns only after the worker has stopped, so the
// caller can close the sink (the database) without a write in progress.
func TestCloseWaitsForWorkerAfterDeadline(t *testing.T) {
	sink := &memSink{gate: make(chan struct{})} // never opened: writes block until their ctx ends
	r := NewRecorder(sink, Options{Buffer: 100, BatchSize: 1, FlushInterval: time.Hour, WriteTimeout: time.Minute})
	for i := range 5 {
		r.Record(click(i))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v; the in-flight write should have been cancelled", d)
	}
	select {
	case <-r.done:
	default:
		t.Fatal("Close returned while the worker was still running")
	}
	// Nothing was written and every click is accounted for.
	if r.Pending() != 0 || r.Recorded()+r.Failed()+r.Dropped() != 5 {
		t.Fatalf("pending=%d recorded=%d failed=%d dropped=%d", r.Pending(), r.Recorded(), r.Failed(), r.Dropped())
	}
}

// skipSink stores only clicks with a positive LinkID, the way the stores
// skip clicks whose link has been deleted.
type skipSink struct{ memSink }

func (s *skipSink) InsertClicks(ctx context.Context, clicks []Click) (int, error) {
	var keep []Click
	for _, c := range clicks {
		if c.LinkID > 0 {
			keep = append(keep, c)
		}
	}
	return s.memSink.InsertClicks(ctx, keep)
}

func TestOrphanedClicksAreCounted(t *testing.T) {
	sink := &skipSink{}
	r := NewRecorder(sink, Options{Buffer: 10, BatchSize: 10, FlushInterval: time.Hour})
	r.Record(Click{LinkID: 1})
	r.Record(Click{LinkID: 0}) // its link was deleted before the flush
	r.Record(Click{LinkID: 2})
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Recorded() != 2 || r.Orphaned() != 1 || r.Failed() != 0 {
		t.Fatalf("recorded=%d orphaned=%d failed=%d, want 2, 1, 0", r.Recorded(), r.Orphaned(), r.Failed())
	}
}
