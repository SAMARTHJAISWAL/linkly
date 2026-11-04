package memstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/analytics"
	"github.com/SAMARTHJAISWAL/linkly/internal/httpapi"
	"github.com/SAMARTHJAISWAL/linkly/internal/service"
	"github.com/SAMARTHJAISWAL/linkly/internal/store"
)

// The demo wires Store into exactly these slots.
var (
	_ service.Store  = (*Store)(nil)
	_ analytics.Sink = (*Store)(nil)
	_ httpapi.Pinger = (*Store)(nil)
)

var day0 = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func mustCreate(t *testing.T, s *Store, code string) store.Link {
	t.Helper()
	l, err := s.CreateLink(context.Background(), store.Link{Code: code, URL: "https://example.com/" + code, CreatedAt: day0})
	if err != nil {
		t.Fatalf("CreateLink(%q): %v", code, err)
	}
	return l
}

func TestCreateGetDelete(t *testing.T) {
	ctx := context.Background()
	s := New()
	a := mustCreate(t, s, "abc")
	b := mustCreate(t, s, "abd")
	if a.ID != 1 || b.ID != 2 {
		t.Fatalf("IDs = %d, %d; want 1, 2", a.ID, b.ID)
	}
	if _, err := s.CreateLink(ctx, store.Link{Code: "abc", URL: "https://x.test"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate code: err = %v, want ErrConflict", err)
	}
	// Codes are case-sensitive, as in SQLite's default BINARY collation.
	mustCreate(t, s, "ABC")

	got, err := s.GetLink(ctx, "abc")
	if err != nil || got != a {
		t.Fatalf("GetLink = %+v, %v; want %+v", got, err, a)
	}
	if _, err := s.GetLink(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetLink(missing) err = %v", err)
	}

	if _, err := s.InsertClicks(ctx, []analytics.Click{{Code: "abc", At: day0}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLink(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLink(ctx, "abc"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	if len(s.clicks) != 0 {
		t.Fatalf("clicks not cascaded: %d left", len(s.clicks))
	}
	// IDs are never reused, like INTEGER PRIMARY KEY after a delete of a
	// non-maximal row.
	if c := mustCreate(t, s, "new"); c.ID != 4 {
		t.Fatalf("ID after delete = %d, want 4", c.ID)
	}
}

func TestInsertClicksSkipsUnknownCodes(t *testing.T) {
	s := New()
	mustCreate(t, s, "abc")
	n, err := s.InsertClicks(context.Background(), []analytics.Click{
		{Code: "abc", At: day0},
		{Code: "gone", At: day0},
		{Code: "abc", At: day0, RefHost: "news.example"},
	})
	if err != nil || n != 2 {
		t.Fatalf("InsertClicks = %d, %v; want 2, nil", n, err)
	}
}

func TestStats(t *testing.T) {
	ctx := context.Background()
	s := New()
	l := mustCreate(t, s, "abc")
	other := mustCreate(t, s, "xyz")

	at := func(days int) time.Time { return day0.AddDate(0, 0, days) }
	clicks := []analytics.Click{
		{Code: "abc", At: at(-40)}, // before the window: counted in total and referrers only
		{Code: "abc", At: at(-1), RefHost: "b.example"},
		{Code: "abc", At: at(0), RefHost: "a.example"},
		{Code: "abc", At: at(0), RefHost: "b.example"},
		{Code: "abc", At: at(0)},
		{Code: "abc", At: at(0), RefHost: "a.example"},
		{Code: "abc", At: at(0), RefHost: "c.example"},
		{Code: "xyz", At: at(0), RefHost: "a.example"},
	}
	if _, err := s.InsertClicks(ctx, clicks); err != nil {
		t.Fatal(err)
	}

	st, err := s.Stats(ctx, l.ID, at(-29).Truncate(24*time.Hour), 3)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 7 {
		t.Errorf("Total = %d, want 7", st.Total)
	}
	wantDaily := []store.DayCount{{Day: "2026-03-09", Clicks: 1}, {Day: "2026-03-10", Clicks: 5}}
	if !slices.Equal(st.Daily, wantDaily) {
		t.Errorf("Daily = %v, want %v", st.Daily, wantDaily)
	}
	// Most clicks first, ties broken by host, limited to 3. Direct traffic
	// ("") has two clicks and sorts first on the tie; c.example is cut.
	wantRefs := []store.ReferrerCount{{Host: "", Clicks: 2}, {Host: "a.example", Clicks: 2}, {Host: "b.example", Clicks: 2}}
	if !slices.Equal(st.Referrers, wantRefs) {
		t.Errorf("Referrers = %v, want %v", st.Referrers, wantRefs)
	}

	empty, err := s.Stats(ctx, other.ID+100, at(-29), 10)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Total != 0 || empty.Daily == nil || empty.Referrers == nil {
		t.Errorf("empty stats = %+v; want zero total and non-nil empty slices", empty)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := New()
	var changes atomic.Int32
	s.OnChange(func() { changes.Add(1) })

	mustCreate(t, s, "abc")
	exp := day0.Add(time.Hour)
	if _, err := s.CreateLink(ctx, store.Link{Code: "tmp", URL: "https://t.example", CreatedAt: day0, ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertClicks(ctx, []analytics.Click{{Code: "abc", At: day0, RefHost: "r.example"}}); err != nil {
		t.Fatal(err)
	}
	if got := changes.Load(); got != 3 {
		t.Fatalf("OnChange called %d times, want 3", got)
	}

	data, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	r := New()
	if err := r.Restore(data); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 2 {
		t.Fatalf("restored %d links, want 2", r.Len())
	}
	got, _ := r.GetLink(ctx, "tmp")
	if !got.ExpiresAt.Equal(exp) || !got.CreatedAt.Equal(day0) || got.ID != 2 {
		t.Fatalf("restored link = %+v", got)
	}
	st, _ := r.Stats(ctx, 1, day0.AddDate(0, 0, -1), 10)
	if st.Total != 1 || st.Referrers[0].Host != "r.example" {
		t.Fatalf("restored stats = %+v", st)
	}
	// The ID sequence continues where it left off.
	if l := mustCreate(t, r, "next"); l.ID != 3 {
		t.Fatalf("ID after restore = %d, want 3", l.ID)
	}
	data2, _ := r.Snapshot()
	if !strings.Contains(string(data2), `"next"`) {
		t.Fatalf("snapshot missing new link: %s", data2)
	}

	for name, bad := range map[string]string{
		"garbage":   "{",
		"version":   `{"v":99}`,
		"dup code":  `{"v":1,"links":[{"id":1,"code":"a"},{"id":2,"code":"a"}]}`,
		"dup id":    `{"v":1,"links":[{"id":1,"code":"a"},{"id":1,"code":"b"}]}`,
		"zero id":   `{"v":1,"links":[{"id":0,"code":"a"}]}`,
		"no code":   `{"v":1,"links":[{"id":1,"code":""}]}`,
		"not json?": "null",
	} {
		before, _ := r.Snapshot()
		if err := r.Restore([]byte(bad)); err == nil {
			t.Errorf("%s: Restore accepted %q", name, bad)
		}
		after, _ := r.Snapshot()
		if string(before) != string(after) {
			t.Errorf("%s: failed Restore changed the store", name)
		}
	}
}

// TestWithRealServer runs the real service, recorder and HTTP handler on
// top of the in-memory store, the way the WebAssembly demo does.
func TestWithRealServer(t *testing.T) {
	st := New()
	rec := analytics.NewRecorder(st, analytics.Options{Buffer: 16, BatchSize: 16, FlushInterval: 5 * time.Millisecond})
	t.Cleanup(func() { _ = rec.Close(context.Background()) })
	base := "https://demo.example/linkly/#"
	h := httpapi.New(httpapi.Options{
		Service: service.New(st, service.Options{SelfHost: "demo.example"}),
		Clicks:  rec,
		Health:  st,
		BaseURL: base,
	})

	do := func(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	w := do("POST", "/api/links", `{"url":"https://example.com/x","alias":"demo1"}`, "Content-Type", "application/json")
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"short_url":"`+base+`/demo1"`) {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/api/links", `{"url":"https://demo.example/linkly/#/demo1"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("self link: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/demo1", "", "Referer", "https://news.example/item"); w.Code != http.StatusFound || w.Header().Get("Location") != "https://example.com/x" {
		t.Fatalf("redirect: %d %v", w.Code, w.Header())
	}
	deadline := time.Now().Add(2 * time.Second)
	for rec.Pending() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	w = do("GET", "/api/links/demo1", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"total_clicks":1`) ||
		!strings.Contains(w.Body.String(), `"host":"news.example"`) {
		t.Fatalf("stats: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/api/links/demo1/qr.png", ""); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("qr: %d %v", w.Code, w.Header())
	}
}
