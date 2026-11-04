//go:build !js

package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
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
		{LinkID: l.ID, Code: "stats", At: day(1, 9), RefHost: "news.ycombinator.com"},
		{LinkID: l.ID, Code: "stats", At: day(1, 23), RefHost: "news.ycombinator.com"},
		{LinkID: l.ID, Code: "stats", At: day(3, 0), RefHost: ""},
		{LinkID: l.ID, Code: "stats", At: day(3, 1), RefHost: "t.co"},
		{LinkID: l.ID, Code: "stats", At: day(3, 2), RefHost: "news.ycombinator.com"},
		{LinkID: l.ID, Code: "stats", At: day(5, 0), RefHost: "t.co"},
		{LinkID: other.ID, Code: "other", At: day(3, 0), RefHost: "t.co"},
		{LinkID: other.ID + 100, Code: "gone", At: day(3, 0)}, // no such link: skipped, not an error
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
	if _, err := s.InsertClicks(ctx, []analytics.Click{{LinkID: l.ID, Code: "bye", At: time.Now()}}); err != nil {
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
				if _, err := s.InsertClicks(ctx, []analytics.Click{{LinkID: l.ID, Code: "busy", At: time.Now()}}); err != nil {
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

// BenchmarkInsertClicks measures click write throughput in batches of the
// default size (500), spread over a few links. ns/op is per click.
func BenchmarkInsertClicks(b *testing.B) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	codes := []string{"aaa", "bbb", "ccc", "ddd"}
	ids := make([]int64, len(codes))
	for i, c := range codes {
		l, err := s.CreateLink(ctx, Link{Code: c, URL: "https://example.com", CreatedAt: time.Now()})
		if err != nil {
			b.Fatal(err)
		}
		ids[i] = l.ID
	}
	const batch = 500
	clicks := make([]analytics.Click, batch)
	for i := range clicks {
		clicks[i] = analytics.Click{LinkID: ids[i%len(ids)], Code: codes[i%len(codes)], At: time.Now(), RefHost: "news.ycombinator.com"}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i += batch {
		n := min(batch, b.N-i)
		if _, err := s.InsertClicks(ctx, clicks[:n]); err != nil {
			b.Fatal(err)
		}
	}
}

// TestConcurrentOpenOnFreshFile: two processes (here, two independent
// connection pools) starting against the same new database must both
// succeed, with the schema migrated exactly once.
func TestConcurrentOpenOnFreshFile(t *testing.T) {
	ctx := context.Background()
	for round := range 10 {
		path := filepath.Join(t.TempDir(), "linkly.db")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		stores := make([]*Store, 2)
		for i := range 2 {
			wg.Go(func() { stores[i], errs[i] = Open(ctx, path) })
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, opener %d: %v", round, i, err)
			}
		}
		var applied int
		if err := stores[0].r.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
			t.Fatal(err)
		}
		if want := len(mustLoadMigrations(t)); applied != want {
			t.Fatalf("round %d: %d migrations recorded, want %d", round, applied, want)
		}
		// Both handles work.
		mustCreate(t, stores[0], "a")
		if _, err := stores[1].GetLink(ctx, "a"); err != nil {
			t.Fatalf("round %d: second handle: %v", round, err)
		}
		for _, s := range stores {
			s.Close()
		}
	}
}

func mustLoadMigrations(t *testing.T) []migration {
	t.Helper()
	ms, err := loadMigrations(migrationFS)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestLinkIDsAreNeverReused(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	a := mustCreate(t, s, "first") // the highest id, which plain INTEGER PRIMARY KEY would hand out again
	if err := s.DeleteLink(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if b := mustCreate(t, s, "first"); b.ID <= a.ID {
		t.Fatalf("id %d reused after delete (new id %d)", a.ID, b.ID)
	}
}

// TestClicksFollowLinkIDNotCode: a click still queued when its link is
// deleted and the code re-created belongs to the old link, so it is
// skipped rather than credited to the new one.
func TestClicksFollowLinkIDNotCode(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	old := mustCreate(t, s, "same")
	if err := s.DeleteLink(ctx, "same"); err != nil {
		t.Fatal(err)
	}
	cur := mustCreate(t, s, "same")
	n, err := s.InsertClicks(ctx, []analytics.Click{
		{LinkID: old.ID, Code: "same", At: time.Now()},
		{LinkID: old.ID, Code: "same", At: time.Now()},
		{LinkID: cur.ID, Code: "same", At: time.Now()},
	})
	if err != nil || n != 1 {
		t.Fatalf("InsertClicks = %d, %v; want 1, nil", n, err)
	}
	if st, _ := s.Stats(ctx, cur.ID, time.Time{}, 5); st.Total != 1 {
		t.Fatalf("re-created link has %d clicks, want 1", st.Total)
	}
}

// TestMigrationToAutoincrementKeepsData opens a database created by the
// first schema version, with links and clicks in it, and checks that the
// table rebuild keeps every row (the old links table is dropped with
// foreign keys off, so ON DELETE CASCADE must not fire) and that ids stop
// being reused afterwards.
func TestMigrationToAutoincrementKeepsData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", "file:"+path+"?"+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := fs.ReadFile(migrationFS, "migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL) STRICT`,
		string(v1),
		`INSERT INTO schema_migrations VALUES (1, 0)`,
		`INSERT INTO links (id, code, url, created_at) VALUES (1, 'one', 'https://a.example', 0), (2, 'two', 'https://b.example', 0), (3, 'three', 'https://c.example', 0)`,
		`INSERT INTO clicks (link_id, clicked_at) VALUES (1, 10), (1, 11), (2, 12), (3, 13)`,
		`DELETE FROM links WHERE id = 2`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	var ddl string
	if err := s.r.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name = 'links'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "AUTOINCREMENT") {
		t.Fatalf("links was not rebuilt with AUTOINCREMENT:\n%s", ddl)
	}
	for code, want := range map[string]struct{ id, clicks int64 }{"one": {1, 2}, "three": {3, 1}} {
		l, err := s.GetLink(ctx, code)
		if err != nil || l.ID != want.id {
			t.Fatalf("GetLink(%s) = %+v, %v; want id %d", code, l, err, want.id)
		}
		if st, _ := s.Stats(ctx, l.ID, time.Time{}, 5); st.Total != want.clicks {
			t.Fatalf("%s has %d clicks after migrating, want %d", code, st.Total, want.clicks)
		}
	}
	rows, err := s.w.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("foreign key violations after migrating")
	}
	rows.Close()
	var fk int
	if err := s.w.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d (%v) on the write connection after migrating, want 1", fk, err)
	}

	// Deleting the highest id no longer frees it.
	a := mustCreate(t, s, "four")
	if a.ID != 4 {
		t.Fatalf("first id after migrating = %d, want 4", a.ID)
	}
	if err := s.DeleteLink(ctx, "four"); err != nil {
		t.Fatal(err)
	}
	if b := mustCreate(t, s, "five"); b.ID != 5 {
		t.Fatalf("id after deleting the newest link = %d, want 5", b.ID)
	}
	// Deleting a link still cascades to its clicks.
	if err := s.DeleteLink(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	var left int
	s.r.QueryRowContext(ctx, `SELECT count(*) FROM clicks WHERE link_id = 1`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d clicks survived deleting their link", left)
	}
}
