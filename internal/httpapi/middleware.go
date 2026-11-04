package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	routeKey
)

// RequestID returns the request ID stored in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// withRequestID tags each request with an ID, reusing a well-formed
// X-Request-ID from the client or proxy so logs can be correlated across
// hops, and echoes it in the response.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// routeHolder lets the per-route wrapper report the matched pattern back
// to the observing middleware, which runs before routing.
type routeHolder struct{ pattern string }

// route wraps a handler so the observing middleware can label metrics and
// logs with its pattern rather than the raw path (which would make metric
// cardinality unbounded).
func route(pattern string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rh, ok := r.Context().Value(routeKey).(*routeHolder); ok {
			rh.pattern = pattern
		}
		h.ServeHTTP(w, r)
	})
}

// statusWriter records the status code and body size of a response.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) headerWritten() bool { return w.status != 0 }

// observe logs every request and counts it by route and status.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rh := &routeHolder{}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), routeKey, rh)))

		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		pattern := rh.pattern
		if pattern == "" {
			pattern = "unmatched"
		}
		s.m.requests.Inc(pattern, strconv.Itoa(status))

		level := slog.LevelInfo
		if pattern == patHealth || pattern == patMetrics {
			level = slog.LevelDebug // scraped constantly; keep them out of the default log
		}
		// Client IPs are deliberately not logged, matching the analytics policy.
		s.log.LogAttrs(r.Context(), level, "request",
			slog.String("request_id", RequestID(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("route", pattern),
			slog.Int("status", status),
			slog.Int("bytes", sw.bytes),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}

// recoverPanics turns a handler panic into a logged 500 instead of a
// dropped connection. http.ErrAbortHandler is re-raised, as net/http uses
// it to abort a response deliberately.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			s.m.panics.Inc()
			s.log.ErrorContext(r.Context(), "panic serving request",
				"request_id", RequestID(r.Context()),
				"panic", v,
				"stack", string(debug.Stack()),
			)
			if sw, ok := w.(interface{ headerWritten() bool }); ok && sw.headerWritten() {
				return // too late to change the response
			}
			writeError(w, r, http.StatusInternalServerError, "internal error", "")
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets headers that are safe for every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
