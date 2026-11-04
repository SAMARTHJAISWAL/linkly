// Package analytics records link clicks off the request path. Redirect
// handlers hand clicks to a Recorder, which queues them in a bounded buffer
// and writes them to storage in batches from a single background worker.
package analytics

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Click is one recorded redirect. It deliberately holds no IP address,
// user agent or full referrer URL: only the referring host.
type Click struct {
	Code    string
	At      time.Time
	RefHost string // empty for direct traffic
}

// Sink persists a batch of clicks and reports how many were stored.
type Sink interface {
	InsertClicks(ctx context.Context, clicks []Click) (int, error)
}

// Options tunes a Recorder.
type Options struct {
	Buffer        int           // queue capacity; Record drops clicks once it is full
	BatchSize     int           // max clicks per InsertClicks call
	FlushInterval time.Duration // max time a click waits before being flushed
	WriteTimeout  time.Duration // deadline for each InsertClicks call (default 5s)
	Logger        *slog.Logger
}

// Recorder batches clicks into a Sink. Record never blocks.
type Recorder struct {
	sink Sink
	opts Options
	log  *slog.Logger

	mu     sync.RWMutex // guards closed and the send on queue
	closed bool
	queue  chan Click
	done   chan struct{}

	pending  atomic.Int64 // accepted but not yet written (queued or in the worker's batch)
	recorded atomic.Uint64
	dropped  atomic.Uint64
	failed   atomic.Uint64
}

// ErrClosed is returned by Close when called more than once.
var ErrClosed = errors.New("analytics: recorder already closed")

// NewRecorder starts the background worker. Call Close to flush and stop it.
func NewRecorder(sink Sink, opts Options) *Recorder {
	if opts.Buffer < 1 {
		opts.Buffer = 1
	}
	if opts.BatchSize < 1 {
		opts.BatchSize = 1
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = time.Second
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 5 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Recorder{
		sink:  sink,
		opts:  opts,
		log:   log,
		queue: make(chan Click, opts.Buffer),
		done:  make(chan struct{}),
	}
	go r.run()
	return r
}

// Record enqueues a click and reports whether it was accepted. When the
// queue is full, or the recorder is closed, the click is dropped and
// counted instead: a slow database must never slow down redirects.
func (r *Recorder) Record(c Click) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		r.dropped.Add(1)
		return false
	}
	// Count before sending so the worker can never decrement first.
	r.pending.Add(1)
	select {
	case r.queue <- c:
		return true
	default:
		r.pending.Add(-1)
		r.dropped.Add(1)
		return false
	}
}

// Close stops accepting clicks, flushes everything still queued and waits
// for the worker to finish or ctx to expire, whichever comes first.
func (r *Recorder) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	r.closed = true
	close(r.queue)
	r.mu.Unlock()

	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recorded, Dropped and Failed are monotonic counters for metrics.
func (r *Recorder) Recorded() uint64 { return r.recorded.Load() }
func (r *Recorder) Dropped() uint64  { return r.dropped.Load() }
func (r *Recorder) Failed() uint64   { return r.failed.Load() }

// Pending reports how many accepted clicks have not been written yet,
// whether still in the queue or in the batch being assembled.
func (r *Recorder) Pending() int { return int(r.pending.Load()) }

func (r *Recorder) run() {
	defer close(r.done)

	ticker := time.NewTicker(r.opts.FlushInterval)
	defer ticker.Stop()

	batch := make([]Click, 0, r.opts.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		r.write(batch)
		batch = batch[:0]
	}

	for {
		select {
		case c, ok := <-r.queue:
			if !ok { // closed: everything buffered has now been received
				flush()
				return
			}
			batch = append(batch, c)
			if len(batch) >= r.opts.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (r *Recorder) write(batch []Click) {
	ctx, cancel := context.WithTimeout(context.Background(), r.opts.WriteTimeout)
	defer cancel()

	n, err := r.sink.InsertClicks(ctx, batch)
	r.pending.Add(-int64(len(batch)))
	r.recorded.Add(uint64(n))
	if err != nil {
		r.failed.Add(uint64(len(batch) - n))
		r.log.Error("writing clicks", "batch", len(batch), "err", err)
	}
}

// RefererHost extracts the lower-cased host from a Referer header value,
// dropping the scheme, port, path, query and any credentials. It returns
// "" when the header is empty or not an absolute http(s) URL.
func RefererHost(referer string) string {
	if referer == "" {
		return ""
	}
	u, err := url.Parse(referer)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if len(host) > 253 {
		return ""
	}
	return host
}
