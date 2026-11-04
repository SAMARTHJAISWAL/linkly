// Package httpapi exposes linkly over HTTP: the JSON API, the redirect
// endpoint, health and metrics, and the embedded web UI.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/analytics"
	"github.com/SAMARTHJAISWAL/linkly/internal/metrics"
	"github.com/SAMARTHJAISWAL/linkly/internal/ratelimit"
	"github.com/SAMARTHJAISWAL/linkly/internal/service"
)

// ClickRecorder accepts clicks without blocking; *analytics.Recorder
// implements it.
type ClickRecorder interface {
	Record(analytics.Click) bool
}

// Pinger reports whether a dependency is healthy.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Options wires a Server's dependencies.
type Options struct {
	Service  *service.Service
	Clicks   ClickRecorder
	Health   Pinger
	Limiter  *ratelimit.Limiter // rate limits link creation; nil disables limiting
	Registry *metrics.Registry  // where HTTP metrics are registered; nil creates a private one
	Logger   *slog.Logger

	BaseURL    string // public origin for short URLs, e.g. "https://sho.rt"
	AdminToken string // bearer token for DELETE; empty disables deletion
	TrustProxy bool   // take the client IP from X-Forwarded-For
	Now        func() time.Time
}

// Server is the HTTP handler for linkly.
type Server struct {
	svc        *service.Service
	clicks     ClickRecorder
	health     Pinger
	limiter    *ratelimit.Limiter
	authFails  *ratelimit.Limiter // failed admin-token attempts per client
	reg        *metrics.Registry
	log        *slog.Logger
	baseURL    string
	adminToken string
	trustProxy bool
	now        func() time.Time

	m       serverMetrics
	handler http.Handler
}

type serverMetrics struct {
	requests        *metrics.CounterVec
	redirectLatency *metrics.Histogram
	redirectLoops   *metrics.Counter
	rateLimited     *metrics.Counter
	authThrottled   *metrics.Counter
	panics          *metrics.Counter
}

// Failed admin authentication: authFailBurst attempts at once, then one per
// minute, per client.
const (
	authFailBurst = 5
	authFailRate  = 1.0 / 60
)

// Route patterns, also used as metric labels.
const (
	patIndex    = "GET /{$}"
	patStatic   = "GET /static/"
	patRedirect = "GET /{code}"
	patCreate   = "POST /api/links"
	patStats    = "GET /api/links/{code}"
	patQR       = "GET /api/links/{code}/qr.png"
	patDelete   = "DELETE /api/links/{code}"
	patHealth   = "GET /healthz"
	patMetrics  = "GET /metrics"
)

// New builds the handler.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Registry == nil {
		opts.Registry = metrics.NewRegistry()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{
		svc:        opts.Service,
		clicks:     opts.Clicks,
		health:     opts.Health,
		limiter:    opts.Limiter,
		reg:        opts.Registry,
		log:        opts.Logger,
		baseURL:    opts.BaseURL,
		adminToken: opts.AdminToken,
		trustProxy: opts.TrustProxy,
		now:        opts.Now,
	}
	// Always on, even when LINKLY_RATE_LIMIT=0: it is a brute-force
	// defence, not traffic shaping. Buckets are swept inline because the
	// server has no background goroutines of its own.
	s.authFails = ratelimit.New(authFailRate, authFailBurst, 10*time.Minute,
		ratelimit.WithClock(opts.Now), ratelimit.WithAutoSweep(time.Minute))
	s.m = serverMetrics{
		requests: s.reg.NewCounterVec("linkly_http_requests_total",
			"HTTP requests by route pattern and status code.", "route", "code"),
		redirectLatency: s.reg.NewHistogram("linkly_redirect_duration_seconds",
			"Time to resolve and answer a redirect, excluding network I/O.", metrics.DefaultLatencyBuckets),
		redirectLoops: s.reg.NewCounter("linkly_redirect_loops_total",
			"Redirects refused with 508 because the target is this server."),
		rateLimited: s.reg.NewCounter("linkly_rate_limited_total",
			"Requests refused with 429 by the rate limiter."),
		authThrottled: s.reg.NewCounter("linkly_auth_throttled_total",
			"DELETE requests refused with 429 after repeated wrong admin tokens."),
		panics: s.reg.NewCounter("linkly_http_panics_total",
			"Handler panics recovered by middleware."),
	}

	mux := http.NewServeMux()
	handle := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, route(pattern, h)) }

	handle(patIndex, s.handleIndex)
	mux.Handle(patStatic, route(patStatic, staticHandler()))
	handle(patRedirect, s.handleRedirect)
	mux.Handle(patCreate, route(patCreate, s.rateLimit(http.HandlerFunc(s.handleCreate))))
	handle(patStats, s.handleStats)
	handle(patQR, s.handleQR)
	handle(patDelete, s.handleDelete)
	handle(patHealth, s.handleHealth)
	mux.Handle(patMetrics, route(patMetrics, s.reg.Handler()))

	// Outermost first: every response gets an ID and security headers, is
	// observed, and panics are recovered inside observation so they are
	// logged and counted as 500s.
	s.handler = withRequestID(securityHeaders(s.observe(s.recoverPanics(mux))))
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// rateLimit refuses requests from clients that have exhausted their bucket.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	if s.limiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := s.limiter.Allow(clientKey(r, s.trustProxy))
		if !ok {
			s.m.rateLimited.Inc()
			w.Header().Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(wait)))
			writeError(w, r, http.StatusTooManyRequests, "rate limit exceeded; retry later", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}
