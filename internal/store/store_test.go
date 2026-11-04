package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/analytics"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "linkly.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func mustCreate(t *testing.T, s *Store, code string) Link {
	t.Helper()
	l, err := s.CreateLink(context.Background(), Link{
		Code: code, URL: "https://example.com/" + code, CreatedAt: time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatalf("CreateLink(%q): %v", code, err)
	}
	return l
}

func TestCreateAndGetLink(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	expires := created.Add(24 * time.Hour)

	tests := []struct {
		name string
		in   Link
	}{
		{"no expiry", Link{Code: "abc", URL: "https://example.com/a", CreatedAt: created}},
		{"with expiry", Link{Code: "xyz", URL: "https://example.com/x", CreatedAt: created, ExpiresAt: expires}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.CreateLink(ctx, tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID == 0 {
				t.Fatal("ID not set")
			}
			back, err := s.GetLink(ctx, tt.in.Code)
			if err != nil {
				t.Fatal(err)
			}
			want := tt.in
			want.ID = got.ID
			if !back.CreatedAt.Equal(want.CreatedAt) || !back.ExpiresAt.Equal(want.ExpiresAt) ||
				back.ID != want.ID || back.Code != want.Code || back.URL != want.URL {
				t.Fatalf("GetLink = %+v, want %+v", back, want)
			}
		})
	}
}

func TestCreateConflict(t *testing.T) {
	s, _ := openTemp(t)
	mustCreate(t, s, "dup")
	_, err := s.CreateLink(context.Background(), Link{Code: "dup", URL: "https://x", CreatedAt: time.Now()})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestCodesAreCaseSensitive(t *testing.T) {
	s, _ := openTemp(t)
	mustCreate(t, s, "AbC")
	mustCreate(t, s, "abc")
	if _, err := s.GetLink(context.Background(), "ABC"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetAndDeleteMissing(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if _, err := s.GetLink(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetLink err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteLink(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteLink err = %v, want ErrNotFound", err)
	}
}

func TestClicksAndStats(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	l := mustCreate(t, s, "stats")
	other := mustCreate(t, s, "other")

	day := func(d int, h int) time.Time { return time.Date(2026, 3, d, h, 0, 0, 0, time.UTC) }
	clicks := []analytics.Click{
		{Code: "stats", At: day(1, 9), RefHost: "news.ycombinator.com"},
		{Code: "stats", At: day(1, 23), RefHost: "news.ycombinator.com"},
		{Code: "stats", At: day(3, 0), RefHost: ""},
		{Code: "stats", At: day(3, 1), RefHost: "t.co"},
		{Code: "stats", At: day(3, 2), RefHost: "news.ycombinator.com"},
		{Code: "stats", At: day(5, 0), RefHost: "t.co"},
		{Code: "other", At: day(3, 0), RefHost: "t.co"},
		{Code: "gone", At: day(3, 0)}, // unknown code: skipped, not an error
	}
	n, err := s.InsertClicks(ctx, clicks)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("stored %d clicks, want 7", n)
	}

	st, err := s.Stats(ctx, l.ID, day(2, 0), 2)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 6 {
		t.Fatalf("Total = %d, want 6", st.Total)
	}
	wantDaily := []DayCount{{"2026-03-03", 3}, {"2026-03-05", 1}}
	if !slices.Equal(st.Daily, wantDaily) {
		t.Fatalf("Daily = %v, want %v", st.Daily, wantDaily)
	}
	wantRefs := []ReferrerCount{{"news.ycombinator.com", 3}, {"t.co", 2}}
	if !slices.Equal(st.Referrers, wantRefs) {
		t.Fatalf("Referrers = %v, want %v", st.Referrers, wantRefs)
	}

	ost, err := s.Stats(ctx, other.ID, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if ost.Total != 1 {
		t.Fatalf("other.Total = %d, want 1", ost.Total)
	}
}

func TestStatsEmptyLink(t *testing.T) {
	s, _ := openTemp(t)
	l := mustCreate(t, s, "quiet")
	st, err := s.Stats(context.Background(), l.ID, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 0 || st.Daily == nil || len(st.Daily) != 0 || st.Referrers == nil || len(st.Referrers) != 0 {
		t.Fatalf("Stats = %+v, want zero total and empty (non-nil) slices", st)
	}
}

func TestDeleteCascadesToClicks(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	l := mustCreate(t, s, "bye")
	if _, err := s.InsertClicks(ctx, []analytics.Click{{Code: "bye", At: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLink(ctx, "bye"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stats(ctx, l.ID, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 0 {
		t.Fatalf("clicks survived deletion: %d", st.Total)
	}
}

func TestReopenKeepsDataAndSkipsAppliedMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "linkly.db")
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, "persist")
	s.Close()

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if _, err := s.GetLink(ctx, "persist"); err != nil {
		t.Fatalf("link lost across reopen: %v", err)
	}
	var mode string
	if err := s.r.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q (%v), want wal", mode, err)
	}
}

func TestReadPoolIsReadOnly(t *testing.T) {
	s, _ := openTemp(t)
	if _, err := s.r.ExecContext(context.Background(), `DELETE FROM links`); err == nil {
		t.Fatal("write through the read pool succeeded")
	}
}

func TestConcurrentWritesAndReads(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	l := mustCreate(t, s, "busy")

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 8 {
		wg.Go(func() {
			for range 20 {
				if _, err := s.InsertClicks(ctx, []analytics.Click{{Code: "busy", At: time.Now()}}); err != nil {
					errs <- err
					return
				}
			}
		})
		wg.Go(func() {
			for range 20 {
				if _, err := s.Stats(ctx, l.ID, time.Time{}, 5); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	st, _ := s.Stats(ctx, l.ID, time.Time{}, 5)
	if st.Total != 160 {
		t.Fatalf("Total = %d, want 160", st.Total)
	}
}

func TestContextIsHonoured(t *testing.T) {
	s, _ := openTemp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetLink(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestLoadMigrationsValidation(t *testing.T) {
	tests := []struct {
		name    string
		files   fstest.MapFS
		wantErr bool
		want    []int
	}{
		{"ordered by version", fstest.MapFS{
			"migrations/0010_b.sql": {Data: []byte("b")},
			"migrations/0002_a.sql": {Data: []byte("a")},
			"migrations/README":     {Data: []byte("ignored")},
		}, false, []int{2, 10}},
		{"bad name", fstest.MapFS{"migrations/init.sql": {}}, true, nil},
		{"duplicate version", fstest.MapFS{
			"migrations/0001_a.sql": {}, "migrations/1_b.sql": {},
		}, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms, err := loadMigrations(tt.files)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			var got []int
			for _, m := range ms {
				got = append(got, m.version)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("versions = %v, want %v", got, tt.want)
			}
		})
	}
}
