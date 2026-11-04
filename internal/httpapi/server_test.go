package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/analytics"
	"github.com/SAMARTHJAISWAL/linkly/internal/metrics"
	"github.com/SAMARTHJAISWAL/linkly/internal/ratelimit"
	"github.com/SAMARTHJAISWAL/linkly/internal/service"
	"github.com/SAMARTHJAISWAL/linkly/internal/store"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	t        *testing.T
	srv      *httptest.Server
	clock    *clock
	reg      *metrics.Registry
	recorder *analytics.Recorder
	store    *store.Store
}

type envOpts struct {
	adminToken string
	rate       float64
	burst      int
	trustProxy bool
}

const baseURL = "https://sho.rt"

func newEnv(t *testing.T, o envOpts) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Now().UTC()}
	rec := analytics.NewRecorder(st, analytics.Options{Buffer: 1000, BatchSize: 100, FlushInterval: 5 * time.Millisecond})
	var lim *ratelimit.Limiter
	if o.rate > 0 {
		lim = ratelimit.New(o.rate, o.burst, time.Minute, ratelimit.WithClock(clk.Now))
	}
	reg := metrics.NewRegistry()
	svc := service.New(st, service.Options{SelfHost: "sho.rt", Now: clk.Now})
	h := New(Options{
		Service: svc, Clicks: rec, Health: st, Limiter: lim, Registry: reg,
		BaseURL: baseURL, AdminToken: o.adminToken, TrustProxy: o.trustProxy, Now: clk.Now,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		rec.Close(context.Background())
		st.Close()
	})
	return &env{t: t, srv: srv, clock: clk, reg: reg, recorder: rec, store: st}
}

// do sends a request without following redirects.
func (e *env) do(method, path string, body any, hdr ...string) *http.Response {
	e.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		buf, _ := json.Marshal(b)
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decoding %s response: %v", resp.Status, err)
	}
	return v
}

func wantStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s: status %d, want %d; body: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, b)
	}
}

func (e *env) create(body map[string]any) linkResponse {
	e.t.Helper()
	resp := e.do("POST", "/api/links", body)
	wantStatus(e.t, resp, http.StatusCreated)
	return decode[linkResponse](e.t, resp)
}

func (e *env) stats(code string) statsResponse {
	e.t.Helper()
	resp := e.do("GET", "/api/links/"+code, nil)
	wantStatus(e.t, resp, http.StatusOK)
	return decode[statsResponse](e.t, resp)
}

// waitForClicks polls stats until the async recorder has flushed n clicks.
func (e *env) waitForClicks(code string, n int64) statsResponse {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s := e.stats(code)
		if s.TotalClicks == n {
			return s
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("total_clicks = %d, want %d", s.TotalClicks, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCreateRedirectStats(t *testing.T) {
	e := newEnv(t, envOpts{})
	link := e.create(map[string]any{"url": "https://example.com/some/long/path?q=1&r=2"})

	if len(link.Code) != service.CodeLength {
		t.Fatalf("code %q has length %d", link.Code, len(link.Code))
	}
	if link.ShortURL != baseURL+"/"+link.Code || link.QRURL != baseURL+"/api/links/"+link.Code+"/qr.png" {
		t.Fatalf("unexpected URLs: %+v", link)
	}
	if link.ExpiresAt != nil || link.Expired {
		t.Fatalf("link should not expire: %+v", link)
	}

	referers := []string{
		"https://news.ycombinator.com/item?id=1",
		"https://NEWS.ycombinator.com/newest",
		"",
		"https://t.co/abc",
	}
	for _, ref := range referers {
		var hdr []string
		if ref != "" {
			hdr = []string{"Referer", ref}
		}
		resp := e.do("GET", "/"+link.Code, nil, hdr...)
		wantStatus(t, resp, http.StatusFound)
		if loc := resp.Header.Get("Location"); loc != "https://example.com/some/long/path?q=1&r=2" {
			t.Fatalf("Location = %q", loc)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", cc)
		}
	}

	s := e.waitForClicks(link.Code, 4)
	if len(s.Daily) != service.StatsDays {
		t.Fatalf("clicks_by_day has %d entries", len(s.Daily))
	}
	today := s.Daily[len(s.Daily)-1]
	if today.Date != e.clock.Now().Format(time.DateOnly) || today.Clicks != 4 {
		t.Fatalf("today = %+v", today)
	}
	want := []referrerResponse{{"news.ycombinator.com", 2}, {"(direct)", 1}, {"t.co", 1}}
	if fmt.Sprint(s.TopReferrers) != fmt.Sprint(want) {
		t.Fatalf("top_referrers = %v, want %v", s.TopReferrers, want)
	}
}

func TestHeadIsNotAClick(t *testing.T) {
	e := newEnv(t, envOpts{})
	link := e.create(map[string]any{"url": "https://example.com"})
	wantStatus(t, e.do("HEAD", "/"+link.Code, nil), http.StatusFound)
	wantStatus(t, e.do("GET", "/"+link.Code, nil), http.StatusFound)
	e.waitForClicks(link.Code, 1)
	time.Sleep(30 * time.Millisecond) // several flush intervals: nothing else arrives
	if s := e.stats(link.Code); s.TotalClicks != 1 {
		t.Fatalf("total_clicks = %d, want 1", s.TotalClicks)
	}
}

func TestCustomAlias(t *testing.T) {
	e := newEnv(t, envOpts{})
	link := e.create(map[string]any{"url": "https://example.com/launch", "alias": "Launch-2026"})
	if link.Code != "Launch-2026" {
		t.Fatalf("code = %q", link.Code)
	}
	resp := e.do("GET", "/Launch-2026", nil)
	wantStatus(t, resp, http.StatusFound)

	dup := e.do("POST", "/api/links", map[string]any{"url": "https://example.org", "alias": "Launch-2026"})
	wantStatus(t, dup, http.StatusConflict)
	if er := decode[errorResponse](t, dup); er.Field != "alias" || er.RequestID == "" {
		t.Fatalf("error body = %+v", er)
	}
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t, envOpts{})
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	tests := []struct {
		name      string
		body      any
		ctype     string
		wantCode  int
		wantField string
	}{
		{"ok", map[string]any{"url": "https://example.com", "expires_at": future}, "", 201, ""},
		{"missing url", map[string]any{}, "", 400, "url"},
		{"not http", map[string]any{"url": "javascript:alert(1)"}, "", 400, "url"},
		{"loop to self", map[string]any{"url": "https://sho.rt/abc"}, "", 400, "url"},
		{"credentials", map[string]any{"url": "https://a@b.com"}, "", 400, "url"},
		{"too long", map[string]any{"url": "https://example.com/" + strings.Repeat("a", 2100)}, "", 400, "url"},
		{"reserved alias", map[string]any{"url": "https://example.com", "alias": "healthz"}, "", 400, "alias"},
		{"bad alias", map[string]any{"url": "https://example.com", "alias": "a b"}, "", 400, "alias"},
		{"past expiry", map[string]any{"url": "https://example.com", "expires_at": past}, "", 400, "expires_at"},
		{"bad expiry format", `{"url":"https://example.com","expires_at":"tomorrow"}`, "", 400, ""},
		{"unknown field", `{"url":"https://example.com","code":"x"}`, "", 400, ""},
		{"malformed json", `{"url":`, "", 400, ""},
		{"trailing data", `{"url":"https://example.com"} {}`, "", 400, ""},
		{"wrong content type", `url=https://example.com`, "application/x-www-form-urlencoded", 415, ""},
		{"body too large", `{"url":"https://example.com/` + strings.Repeat("a", 9000) + `"}`, "", 413, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hdr []string
			if tt.ctype != "" {
				hdr = []string{"Content-Type", tt.ctype}
			}
			resp := e.do("POST", "/api/links", tt.body, hdr...)
			wantStatus(t, resp, tt.wantCode)
			if tt.wantCode >= 400 {
				er := decode[errorResponse](t, resp)
				if er.Error == "" || er.Field != tt.wantField {
					t.Fatalf("error body = %+v, want field %q", er, tt.wantField)
				}
			}
		})
	}
}

func TestExpiry(t *testing.T) {
	e := newEnv(t, envOpts{})
	exp := e.clock.Now().Add(time.Hour).Truncate(time.Second)
	link := e.create(map[string]any{"url": "https://example.com", "expires_at": exp})
	if link.ExpiresAt == nil || !link.ExpiresAt.Equal(exp) {
		t.Fatalf("expires_at = %v, want %v", link.ExpiresAt, exp)
	}
	wantStatus(t, e.do("GET", "/"+link.Code, nil), http.StatusFound)

	e.clock.Advance(time.Hour)
	wantStatus(t, e.do("GET", "/"+link.Code, nil), http.StatusGone)
	wantStatus(t, e.do("GET", "/api/links/"+link.Code+"/qr.png", nil), http.StatusGone)

	// Stats stay available, and the expired redirect was not counted.
	s := e.waitForClicks(link.Code, 1)
	if !s.Expired {
		t.Fatal("stats should report expired: true")
	}
}

func TestNotFound(t *testing.T) {
	e := newEnv(t, envOpts{})
	for _, path := range []string{"/nope123", "/bad.code", "/favicon.ico", "/" + strings.Repeat("x", 40)} {
		wantStatus(t, e.do("GET", path, nil), http.StatusNotFound)
	}
	wantStatus(t, e.do("GET", "/api/links/nope123", nil), http.StatusNotFound)
	wantStatus(t, e.do("GET", "/api/links/nope123/qr.png", nil), http.StatusNotFound)
	wantStatus(t, e.do("PUT", "/api/links", nil), http.StatusMethodNotAllowed)
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, envOpts{rate: 1, burst: 3})
	body := map[string]any{"url": "https://example.com"}
	for i := range 3 {
		if resp := e.do("POST", "/api/links", body); resp.StatusCode != http.StatusCreated {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
	}
	resp := e.do("POST", "/api/links", body)
	wantStatus(t, resp, http.StatusTooManyRequests)
	if ra := resp.Header.Get("Retry-After"); ra != "1" {
		t.Fatalf("Retry-After = %q, want 1", ra)
	}

	// Reads and redirects are not limited.
	wantStatus(t, e.do("GET", "/healthz", nil), http.StatusOK)

	e.clock.Advance(time.Second)
	wantStatus(t, e.do("POST", "/api/links", body), http.StatusCreated)
	wantStatus(t, e.do("POST", "/api/links", body), http.StatusTooManyRequests)

	if got := scrape(t, e); !strings.Contains(got, "linkly_rate_limited_total 2") {
		t.Fatalf("metrics missing rate-limited count:\n%s", got)
	}
}

func TestRateLimitIgnoresForwardedForUnlessTrusted(t *testing.T) {
	tests := []struct {
		trust     bool
		wantSplit bool // different XFF values get separate buckets
	}{
		{trust: false, wantSplit: false},
		{trust: true, wantSplit: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("trust=%v", tt.trust), func(t *testing.T) {
			e := newEnv(t, envOpts{rate: 1, burst: 1, trustProxy: tt.trust})
			body := map[string]any{"url": "https://example.com"}
			wantStatus(t, e.do("POST", "/api/links", body, "X-Forwarded-For", "203.0.113.1"), http.StatusCreated)
			resp := e.do("POST", "/api/links", body, "X-Forwarded-For", "203.0.113.2")
			want := http.StatusTooManyRequests
			if tt.wantSplit {
				want = http.StatusCreated
			}
			wantStatus(t, resp, want)
		})
	}
}

func TestDelete(t *testing.T) {
	t.Run("disabled without token", func(t *testing.T) {
		e := newEnv(t, envOpts{})
		link := e.create(map[string]any{"url": "https://example.com"})
		wantStatus(t, e.do("DELETE", "/api/links/"+link.Code, nil, "Authorization", "Bearer anything"), http.StatusForbidden)
	})

	e := newEnv(t, envOpts{adminToken: "correct-horse"})
	link := e.create(map[string]any{"url": "https://example.com"})
	path := "/api/links/" + link.Code

	tests := []struct {
		name string
		auth string
		want int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer battery-staple", http.StatusUnauthorized},
		{"token prefix", "Bearer correct", http.StatusUnauthorized},
		{"wrong scheme", "Basic correct-horse", http.StatusUnauthorized},
		{"correct token", "Bearer correct-horse", http.StatusNoContent},
		{"already deleted", "bearer correct-horse", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hdr []string
			if tt.auth != "" {
				hdr = []string{"Authorization", tt.auth}
			}
			resp := e.do("DELETE", path, nil, hdr...)
			wantStatus(t, resp, tt.want)
			if tt.want == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
				t.Fatal("401 without WWW-Authenticate")
			}
		})
	}
	wantStatus(t, e.do("GET", "/"+link.Code, nil), http.StatusNotFound)
}

func TestQRCode(t *testing.T) {
	e := newEnv(t, envOpts{})
	link := e.create(map[string]any{"url": "https://example.com"})

	for _, size := range []int{0, 128, 512} {
		path := "/api/links/" + link.Code + "/qr.png"
		want := qrDefaultSize
		if size != 0 {
			path += fmt.Sprintf("?size=%d", size)
			want = size
		}
		resp := e.do("GET", path, nil)
		wantStatus(t, resp, http.StatusOK)
		if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
			t.Fatalf("Content-Type = %q", ct)
		}
		img, err := png.Decode(resp.Body)
		if err != nil {
			t.Fatalf("not a PNG: %v", err)
		}
		if b := img.Bounds(); b.Dx() != want || b.Dy() != want {
			t.Fatalf("image is %dx%d, want %dx%d", b.Dx(), b.Dy(), want, want)
		}
	}
	for _, bad := range []string{"10", "5000", "big"} {
		wantStatus(t, e.do("GET", "/api/links/"+link.Code+"/qr.png?size="+bad, nil), http.StatusBadRequest)
	}
}

func TestHealthMetricsAndUI(t *testing.T) {
	e := newEnv(t, envOpts{})
	resp := e.do("GET", "/healthz", nil)
	wantStatus(t, resp, http.StatusOK)
	if h := decode[map[string]string](t, resp); h["status"] != "ok" {
		t.Fatalf("healthz = %v", h)
	}

	link := e.create(map[string]any{"url": "https://example.com"})
	e.do("GET", "/"+link.Code, nil)
	e.do("GET", "/missing1", nil)

	m := scrape(t, e)
	for _, want := range []string{
		`linkly_http_requests_total{route="POST /api/links",code="201"} 1`,
		`linkly_http_requests_total{route="GET /{code}",code="302"} 1`,
		`linkly_http_requests_total{route="GET /{code}",code="404"} 1`,
		`linkly_redirect_duration_seconds_count 2`,
		`# TYPE linkly_redirect_duration_seconds histogram`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q", want)
		}
	}

	ui := e.do("GET", "/", nil)
	wantStatus(t, ui, http.StatusOK)
	if ct := ui.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("UI Content-Type = %q", ct)
	}
	if csp := ui.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("UI CSP = %q", csp)
	}
	body, _ := io.ReadAll(ui.Body)
	if !bytes.Contains(body, []byte(`<script src="/static/app.js"`)) {
		t.Fatal("index.html does not load app.js")
	}
	for _, asset := range []string{"/static/app.js", "/static/style.css", "/static/favicon.svg"} {
		wantStatus(t, e.do("GET", asset, nil), http.StatusOK)
	}
	wantStatus(t, e.do("GET", "/static/nope.js", nil), http.StatusNotFound)
}

func TestHealthUnavailable(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.store.Close()
	wantStatus(t, e.do("GET", "/healthz", nil), http.StatusServiceUnavailable)
}

func TestRequestID(t *testing.T) {
	e := newEnv(t, envOpts{})
	if id := e.do("GET", "/healthz", nil, "X-Request-ID", "trace-abc.123").Header.Get("X-Request-ID"); id != "trace-abc.123" {
		t.Fatalf("valid inbound ID not echoed: %q", id)
	}
	id := e.do("GET", "/healthz", nil, "X-Request-ID", "bad id\"with<chars>").Header.Get("X-Request-ID")
	if len(id) != 16 || strings.ContainsAny(id, " \"<>") {
		t.Fatalf("unsafe inbound ID not replaced: %q", id)
	}
}

func TestPanicRecovery(t *testing.T) {
	s := New(Options{})
	h := withRequestID(s.observe(s.recoverPanics(route("GET /boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if s.m.panics.Value() != 1 || s.m.requests.Value("GET /boom", "500") != 1 {
		t.Fatal("panic not counted")
	}
}

func scrape(t *testing.T, e *env) string {
	t.Helper()
	resp := e.do("GET", "/metrics", nil)
	wantStatus(t, resp, http.StatusOK)
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// BenchmarkRedirect measures the full handler stack for GET /{code}
// against a real SQLite file: middleware, lookup, click hand-off.
func BenchmarkRedirect(b *testing.B) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	rec := analytics.NewRecorder(st, analytics.Options{Buffer: 1 << 16, BatchSize: 1000, FlushInterval: 100 * time.Millisecond})
	defer rec.Close(ctx)
	svc := service.New(st, service.Options{SelfHost: "sho.rt"})
	h := New(Options{Service: svc, Clicks: rec, BaseURL: baseURL})

	link, err := svc.Create(ctx, service.CreateRequest{URL: "https://example.com/target"})
	if err != nil {
		b.Fatal(err)
	}
	path := "/" + link.Code

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != http.StatusFound {
				b.Fatalf("status %d", w.Code)
			}
		}
	})
}

// TestCreateRejectsTrailingJSON: anything but whitespace after the JSON
// object is an error, including a stray closing brace or bracket, which
// Decoder.More does not report.
func TestCreateRejectsTrailingJSON(t *testing.T) {
	e := newEnv(t, envOpts{})
	tests := []struct {
		name string
		body string
		want int
	}{
		{"extra brace", `{"url":"https://example.com"}}`, 400},
		{"extra bracket", `{"url":"https://example.com"}]`, 400},
		{"second object", `{"url":"https://example.com"} {}`, 400},
		{"second value", `{"url":"https://example.com"} 1`, 400},
		{"garbage", `{"url":"https://example.com"}x`, 400},
		{"trailing whitespace", "{\"url\":\"https://example.com\"}\n\t ", 201},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantStatus(t, e.do("POST", "/api/links", tt.body), tt.want)
		})
	}
}

// TestCreateContentType: the media type is compared exactly and
// case-insensitively, with parameters allowed.
func TestCreateContentType(t *testing.T) {
	e := newEnv(t, envOpts{})
	body := `{"url":"https://example.com"}`
	tests := []struct {
		ctype string
		want  int
	}{
		{"application/json", 201},
		{"application/json; charset=utf-8", 201},
		{"Application/JSON", 201},
		{"APPLICATION/JSON ; Charset=UTF-8", 201},
		{"application/jsonl", 415},
		{"application/json-seq", 415},
		{"application/jsonx; charset=utf-8", 415},
		{"text/plain", 415},
		{"application/json; charset", 415}, // malformed parameter
	}
	for _, tt := range tests {
		t.Run(tt.ctype, func(t *testing.T) {
			wantStatus(t, e.do("POST", "/api/links", body, "Content-Type", tt.ctype), tt.want)
		})
	}
}

// TestCreateRejectsZeroExpiry: the zero time is not "never"; only an absent
// or null expires_at means a link never expires.
func TestCreateRejectsZeroExpiry(t *testing.T) {
	e := newEnv(t, envOpts{})
	resp := e.do("POST", "/api/links", `{"url":"https://example.com","expires_at":"0001-01-01T00:00:00Z"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if er := decode[errorResponse](t, resp); er.Field != "expires_at" {
		t.Fatalf("error body = %+v, want field expires_at", er)
	}

	resp = e.do("POST", "/api/links", `{"url":"https://example.com","expires_at":null}`)
	wantStatus(t, resp, http.StatusCreated)
	if l := decode[linkResponse](t, resp); l.ExpiresAt != nil {
		t.Fatalf("null expires_at should mean never, got %v", l.ExpiresAt)
	}
}

// TestRedirectRefusesLoops: a stored link whose target is this server, by
// the name the client used or by the configured base URL, is answered with
// 508 and not counted. Such links predate validation or were created under
// another name; either way a client following redirects must not be able
// to inflate the count.
func TestRedirectRefusesLoops(t *testing.T) {
	e := newEnv(t, envOpts{})
	ctx := context.Background()
	now := e.clock.Now()
	for code, target := range map[string]string{
		"loopy": e.srv.URL + "/loopy",          // http://127.0.0.1:PORT/loopy
		"based": "https://SHO.RT./based",       // the base URL's host
		"other": "http://127.0.0.1:1/anything", // same host as the request, other port
	} {
		if _, err := e.store.CreateLink(ctx, store.Link{Code: code, URL: target, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	for _, code := range []string{"loopy", "based", "other"} {
		resp := e.do("GET", "/"+code, nil)
		wantStatus(t, resp, http.StatusLoopDetected)
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Fatalf("%s: Location = %q on a refused loop", code, loc)
		}
	}

	// A client that follows redirects (curl -L, a browser) stops at once.
	resp, err := http.Get(e.srv.URL + "/loopy")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusLoopDetected {
		t.Fatalf("following redirects: status %d, want 508", resp.StatusCode)
	}
	if n := e.recorder.Pending() + int(e.recorder.Recorded()); n != 0 {
		t.Fatalf("%d clicks recorded for refused loops", n)
	}
	if got := scrape(t, e); !strings.Contains(got, "linkly_redirect_loops_total 4") {
		t.Fatalf("metrics missing loop count:\n%s", got)
	}
}

// TestCreateRefusesRequestHost: a link to the name the client used to reach
// the server is refused even when it differs from LINKLY_BASE_URL.
func TestCreateRefusesRequestHost(t *testing.T) {
	e := newEnv(t, envOpts{})
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/links", strings.NewReader(`{"url":"https://Short.Example./abc"}`))
	req.Host = "short.example:8443"
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	wantStatus(t, resp, http.StatusBadRequest)
	if er := decode[errorResponse](t, resp); er.Field != "url" || !strings.Contains(er.Error, "this shortener") {
		t.Fatalf("error body = %+v", er)
	}
}

// TestCreateRefusesNonASCIIHost: the review's reproduction, a no-break
// space inside the host, must not be stored or echoed in Location.
func TestCreateRefusesNonASCIIHost(t *testing.T) {
	e := newEnv(t, envOpts{})
	for _, target := range []string{"https://exa mple.com/", "http://127.0.0.1:8080/loopy", "http://2130706433/"} {
		resp := e.do("POST", "/api/links", map[string]any{"url": target})
		wantStatus(t, resp, http.StatusBadRequest)
	}
}

// TestDeleteThrottlesFailedAuth: after authFailBurst wrong tokens a client
// is refused with 429, whatever token it sends, until a failure's worth of
// time has passed. Other clients are unaffected.
func TestDeleteThrottlesFailedAuth(t *testing.T) {
	e := newEnv(t, envOpts{adminToken: "correct-horse-battery", trustProxy: true})
	link := e.create(map[string]any{"url": "https://example.com"})
	path := "/api/links/" + link.Code
	attacker := []string{"X-Forwarded-For", "203.0.113.9"}
	bad := append([]string{"Authorization", "Bearer guess"}, attacker...)

	for i := range authFailBurst {
		if resp := e.do("DELETE", path, nil, bad...); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, resp.StatusCode)
		}
	}
	resp := e.do("DELETE", path, nil, bad...)
	wantStatus(t, resp, http.StatusTooManyRequests)
	if ra := resp.Header.Get("Retry-After"); ra != "60" {
		t.Fatalf("Retry-After = %q, want 60", ra)
	}
	// Even the right token is refused while blocked: a guesser learns nothing.
	good := append([]string{"Authorization", "Bearer correct-horse-battery"}, attacker...)
	wantStatus(t, e.do("DELETE", path, nil, good...), http.StatusTooManyRequests)

	// Another client is not affected.
	other := e.create(map[string]any{"url": "https://example.org"})
	wantStatus(t, e.do("DELETE", "/api/links/"+other.Code, nil,
		"Authorization", "Bearer correct-horse-battery", "X-Forwarded-For", "198.51.100.7"), http.StatusNoContent)

	// One failure refills per minute.
	e.clock.Advance(time.Minute)
	wantStatus(t, e.do("DELETE", path, nil, good...), http.StatusNoContent)

	if got := scrape(t, e); !strings.Contains(got, "linkly_auth_throttled_total 2") {
		t.Fatalf("metrics missing throttled count:\n%s", got)
	}
}
