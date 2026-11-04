package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/config"
	"github.com/SAMARTHJAISWAL/linkly/internal/store"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// TestShutdownFlushesClicks runs the real server with a flush interval far
// longer than the test, clicks a link, and checks that the clicks still
// queued in memory reach the database during graceful shutdown.
func TestShutdownFlushesClicks(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "linkly.db")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.ClickFlushInterval = time.Hour
	cfg.ClickBatch = 10_000
	cfg.ShutdownTimeout = 5 * time.Second

	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := New(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx, ln) }()
	base := "http://" + ln.Addr().String()

	resp, err := http.Post(base+"/api/links", "application/json", strings.NewReader(`{"url":"https://example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	var link struct{ Code string }
	json.NewDecoder(resp.Body).Decode(&link)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %s", resp.Status)
	}

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	const clicks = 25
	for range clicks {
		r, err := noFollow.Get(base + "/" + link.Code)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusFound {
			t.Fatalf("redirect: %s", r.Status)
		}
	}

	if got := statsTotal(t, base, link.Code); got != 0 {
		t.Fatalf("before shutdown total_clicks = %d, want 0 (still queued)", got)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if !strings.Contains(logs.String(), `"pending_at_shutdown":25`) {
		t.Errorf("shutdown log does not report 25 pending clicks:\n%s", logs.String())
	}

	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l, err := st.GetLink(context.Background(), link.Code)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Stats(context.Background(), l.ID, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != clicks {
		t.Fatalf("after shutdown total = %d, want %d", s.Total, clicks)
	}
}

func statsTotal(t *testing.T, base, code string) int64 {
	t.Helper()
	resp, err := http.Get(base + "/api/links/" + code)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s struct {
		TotalClicks int64 `json:"total_clicks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s.TotalClicks
}

func TestMetricsExposeClickPipeline(t *testing.T) {
	cfg := config.Default()
	cfg.DBPath = filepath.Join(t.TempDir(), "linkly.db")
	a, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	srv := &http.Server{Handler: a.Handler()}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	defer srv.Close()

	resp, err := http.Get("http://" + ln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	for _, name := range []string{
		"linkly_clicks_recorded_total", "linkly_clicks_dropped_total", "linkly_clicks_failed_total",
		"linkly_click_queue_depth", "linkly_rate_limit_buckets", "linkly_rate_limited_total",
		"linkly_http_requests_total", "linkly_redirect_duration_seconds", "go_goroutines",
	} {
		if !strings.Contains(b.String(), "# TYPE "+name+" ") {
			t.Errorf("metric %s missing", name)
		}
	}
}

func TestNewFailsOnUnopenableDB(t *testing.T) {
	cfg := config.Default()
	cfg.DBPath = filepath.Join(t.TempDir(), "missing-dir", "x.db")
	if _, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("expected an error for a database in a missing directory")
	}
}
