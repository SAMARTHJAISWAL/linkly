// Package app wires linkly's components together and owns the process
// lifecycle: start the HTTP server, then on shutdown drain requests, flush
// queued clicks and close the database, in that order.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/analytics"
	"github.com/SAMARTHJAISWAL/linkly/internal/config"
	"github.com/SAMARTHJAISWAL/linkly/internal/httpapi"
	"github.com/SAMARTHJAISWAL/linkly/internal/metrics"
	"github.com/SAMARTHJAISWAL/linkly/internal/ratelimit"
	"github.com/SAMARTHJAISWAL/linkly/internal/service"
	"github.com/SAMARTHJAISWAL/linkly/internal/store"
)

// App is a fully wired linkly instance.
type App struct {
	cfg      config.Config
	log      *slog.Logger
	store    *store.Store
	recorder *analytics.Recorder
	limiter  *ratelimit.Limiter
	handler  *httpapi.Server
}

// New opens the database and builds every component. Call Run to serve.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	rec := analytics.NewRecorder(st, analytics.Options{
		Buffer:        cfg.ClickBuffer,
		BatchSize:     cfg.ClickBatch,
		FlushInterval: cfg.ClickFlushInterval,
		Logger:        log,
	})

	var lim *ratelimit.Limiter
	if cfg.RateLimit > 0 {
		lim = ratelimit.New(cfg.RateLimit, cfg.RateBurst, cfg.RateIdleTTL)
	}

	reg := metrics.NewRegistry()
	registerMetrics(reg, rec, lim)

	svc := service.New(st, service.Options{SelfHost: cfg.BaseHost()})
	h := httpapi.New(httpapi.Options{
		Service:    svc,
		Clicks:     rec,
		Health:     st,
		Limiter:    lim,
		Registry:   reg,
		Logger:     log,
		BaseURL:    cfg.BaseURL,
		AdminToken: cfg.AdminToken,
		TrustProxy: cfg.TrustProxy,
	})
	return &App{cfg: cfg, log: log, store: st, recorder: rec, limiter: lim, handler: h}, nil
}

func registerMetrics(reg *metrics.Registry, rec *analytics.Recorder, lim *ratelimit.Limiter) {
	reg.NewCounterFunc("linkly_clicks_recorded_total", "Clicks written to the database.",
		func() float64 { return float64(rec.Recorded()) })
	reg.NewCounterFunc("linkly_clicks_dropped_total", "Clicks discarded because the queue was full.",
		func() float64 { return float64(rec.Dropped()) })
	reg.NewCounterFunc("linkly_clicks_failed_total", "Clicks lost to database write errors.",
		func() float64 { return float64(rec.Failed()) })
	reg.NewCounterFunc("linkly_clicks_orphaned_total", "Clicks discarded because their link was deleted before they were written.",
		func() float64 { return float64(rec.Orphaned()) })
	reg.NewGaugeFunc("linkly_click_queue_depth", "Clicks waiting to be written.",
		func() float64 { return float64(rec.Pending()) })
	if lim != nil {
		reg.NewGaugeFunc("linkly_rate_limit_buckets", "Clients currently tracked by the rate limiter.",
			func() float64 { return float64(lim.Len()) })
	}
	reg.NewGaugeFunc("go_goroutines", "Number of goroutines.",
		func() float64 { return float64(runtime.NumGoroutine()) })
}

// Handler returns the HTTP handler, for tests and benchmarks.
func (a *App) Handler() http.Handler { return a.handler }

// Run serves on ln until ctx is cancelled, then shuts down gracefully. It
// always releases the App's resources before returning.
func (a *App) Run(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}

	sweepCtx, stopSweep := context.WithCancel(context.Background())
	defer stopSweep()
	if a.limiter != nil {
		go a.limiter.Run(sweepCtx, time.Minute)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	a.log.Info("listening", "addr", ln.Addr().String(), "base_url", a.cfg.BaseURL)

	var err error
	select {
	case err = <-serveErr: // the listener failed; still clean up below
	case <-ctx.Done():
		a.log.Info("shutting down", "timeout", a.cfg.ShutdownTimeout.String())
	}
	return errors.Join(err, a.shutdown(srv))
}

// maxFlushReserve is how much of the shutdown budget is kept back for
// flushing clicks, so slow HTTP clients cannot use all of it.
const maxFlushReserve = 2 * time.Second

// shutdown stops accepting requests and waits for in-flight ones, then
// flushes queued clicks (which those requests may have added), then closes
// the database.
//
// The whole sequence fits in ShutdownTimeout. The HTTP drain may use all
// of it except a reserve of min(2s, ShutdownTimeout/2), so a client that
// holds its connection open cannot starve the click flush, which gets the
// reserve plus whatever the drain left unused. The recorder's Close
// returns only once its worker has stopped, even when the flush runs out
// of time, so the database never closes under a write.
func (a *App) shutdown(srv *http.Server) error {
	start := time.Now()
	reserve := min(maxFlushReserve, a.cfg.ShutdownTimeout/2)
	httpCtx, cancelHTTP := context.WithDeadline(context.Background(), start.Add(a.cfg.ShutdownTimeout-reserve))
	defer cancelHTTP()
	flushCtx, cancelFlush := context.WithDeadline(context.Background(), start.Add(a.cfg.ShutdownTimeout))
	defer cancelFlush()

	var errs []error
	if err := srv.Shutdown(httpCtx); err != nil {
		errs = append(errs, fmt.Errorf("http shutdown: %w", err))
		// Drop the stragglers' connections; clicks their handlers try to
		// record from now on are refused and counted as dropped.
		_ = srv.Close()
	}
	pending := a.recorder.Pending()
	if err := a.recorder.Close(flushCtx); err != nil {
		errs = append(errs, fmt.Errorf("flushing clicks: %w", err))
	}
	a.log.Info("clicks flushed", "pending_at_shutdown", pending, "recorded_total", a.recorder.Recorded(),
		"dropped_total", a.recorder.Dropped(), "failed_total", a.recorder.Failed())
	if err := a.store.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing database: %w", err))
	}
	if len(errs) == 0 {
		a.log.Info("shutdown complete")
	}
	return errors.Join(errs...)
}

// Close releases resources for an App that was never Run.
func (a *App) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()
	return errors.Join(a.recorder.Close(ctx), a.store.Close())
}
