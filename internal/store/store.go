// Package store persists links and clicks in SQLite using the pure-Go
// modernc.org/sqlite driver, so the binary needs no cgo.
//
// Two connection pools are kept. SQLite allows one writer at a time, so the
// write pool has a single connection and transactions start with BEGIN
// IMMEDIATE; this turns lock contention into queueing in database/sql
// instead of SQLITE_BUSY errors. The read pool has several read-only
// connections that, thanks to WAL mode, never wait on the writer.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/analytics"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	// ErrNotFound means no link has the requested code.
	ErrNotFound = errors.New("store: link not found")
	// ErrConflict means the code is already taken.
	ErrConflict = errors.New("store: code already exists")
)

// Link is a stored short link. A zero ExpiresAt means it never expires.
type Link struct {
	ID        int64
	Code      string
	URL       string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// DayCount is the number of clicks on one UTC day ("2006-01-02").
type DayCount struct {
	Day    string
	Clicks int64
}

// ReferrerCount is the number of clicks from one referring host ("" is direct).
type ReferrerCount struct {
	Host   string
	Clicks int64
}

// Stats summarises a link's clicks. Daily is sparse: days without clicks
// are omitted.
type Stats struct {
	Total     int64
	Daily     []DayCount
	Referrers []ReferrerCount
}

// Store is safe for concurrent use.
type Store struct {
	w, r *sql.DB

	insertLink, deleteLink, insertClick         *sql.Stmt
	getLink, countClicks, dailyClicks, referrer *sql.Stmt
}

const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

// Open opens (creating if needed) the database at path and applies any
// pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	w, err := sql.Open("sqlite", "file:"+path+"?"+pragmas+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxIdleTime(0)

	s := &Store{w: w}
	if err := migrate(ctx, w); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}

	// The read pool is opened after migrating so its connections see the
	// final schema.
	r, err := sql.Open("sqlite", "file:"+path+"?"+pragmas+"&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, err
	}
	n := max(4, runtime.GOMAXPROCS(0))
	r.SetMaxOpenConns(n)
	r.SetMaxIdleConns(n)
	s.r = r

	if err := s.prepare(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) prepare(ctx context.Context) error {
	stmts := []struct {
		db  *sql.DB
		dst **sql.Stmt
		sql string
	}{
		{s.w, &s.insertLink, `INSERT INTO links (code, url, created_at, expires_at) VALUES (?, ?, ?, ?)`},
		{s.w, &s.deleteLink, `DELETE FROM links WHERE code = ?`},
		// Resolving the code inside the INSERT means a click for a link
		// deleted while the click sat in the queue is silently skipped
		// instead of failing the whole batch on the foreign key.
		{s.w, &s.insertClick, `INSERT INTO clicks (link_id, clicked_at, referrer_host)
			SELECT id, ?, ? FROM links WHERE code = ?`},
		{s.r, &s.getLink, `SELECT id, code, url, created_at, expires_at FROM links WHERE code = ?`},
		{s.r, &s.countClicks, `SELECT count(*) FROM clicks WHERE link_id = ?`},
		{s.r, &s.dailyClicks, `SELECT date(clicked_at, 'unixepoch') AS day, count(*)
			FROM clicks WHERE link_id = ? AND clicked_at >= ?
			GROUP BY day ORDER BY day`},
		{s.r, &s.referrer, `SELECT referrer_host, count(*) AS n
			FROM clicks WHERE link_id = ?
			GROUP BY referrer_host ORDER BY n DESC, referrer_host LIMIT ?`},
	}
	for _, st := range stmts {
		p, err := st.db.PrepareContext(ctx, st.sql)
		if err != nil {
			return fmt.Errorf("preparing %q: %w", st.sql, err)
		}
		*st.dst = p
	}
	return nil
}

// Close releases the statements and both pools.
func (s *Store) Close() error {
	for _, st := range []*sql.Stmt{
		s.insertLink, s.deleteLink, s.insertClick,
		s.getLink, s.countClicks, s.dailyClicks, s.referrer,
	} {
		if st != nil {
			st.Close()
		}
	}
	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	errs = append(errs, s.w.Close())
	return errors.Join(errs...)
}

// Ping checks that both pools can reach the database.
func (s *Store) Ping(ctx context.Context) error {
	return errors.Join(s.w.PingContext(ctx), s.r.PingContext(ctx))
}

// CreateLink inserts l and returns it with its ID set. It returns
// ErrConflict if the code is taken.
func (s *Store) CreateLink(ctx context.Context, l Link) (Link, error) {
	res, err := s.insertLink.ExecContext(ctx, l.Code, l.URL, l.CreatedAt.Unix(), nullableUnix(l.ExpiresAt))
	if err != nil {
		if isUniqueViolation(err) {
			return Link{}, ErrConflict
		}
		return Link{}, err
	}
	l.ID, err = res.LastInsertId()
	return l, err
}

// GetLink returns the link with the given code or ErrNotFound.
func (s *Store) GetLink(ctx context.Context, code string) (Link, error) {
	var (
		l       Link
		created int64
		expires sql.NullInt64
	)
	err := s.getLink.QueryRowContext(ctx, code).Scan(&l.ID, &l.Code, &l.URL, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, err
	}
	l.CreatedAt = time.Unix(created, 0).UTC()
	if expires.Valid {
		l.ExpiresAt = time.Unix(expires.Int64, 0).UTC()
	}
	return l, nil
}

// DeleteLink removes a link and, via ON DELETE CASCADE, its clicks.
func (s *Store) DeleteLink(ctx context.Context, code string) error {
	res, err := s.deleteLink.ExecContext(ctx, code)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// InsertClicks writes a batch of clicks in one transaction and reports how
// many were stored. Clicks for codes that no longer exist are skipped.
// It implements analytics.Sink.
func (s *Store) InsertClicks(ctx context.Context, clicks []analytics.Click) (int, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt := tx.StmtContext(ctx, s.insertClick)
	var stored int64
	for _, c := range clicks {
		res, err := stmt.ExecContext(ctx, c.At.Unix(), c.RefHost, c.Code)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		stored += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(stored), nil
}

// Stats returns the total click count for a link, its clicks per UTC day
// since the given time and its top referrers. The three queries run in one
// read transaction, so they see the same snapshot.
func (s *Store) Stats(ctx context.Context, linkID int64, since time.Time, topReferrers int) (Stats, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Stats{}, err
	}
	defer tx.Rollback()

	var st Stats
	if err := tx.StmtContext(ctx, s.countClicks).QueryRowContext(ctx, linkID).Scan(&st.Total); err != nil {
		return Stats{}, err
	}

	rows, err := tx.StmtContext(ctx, s.dailyClicks).QueryContext(ctx, linkID, since.Unix())
	if err != nil {
		return Stats{}, err
	}
	st.Daily, err = scanAll(rows, func(d *DayCount) []any { return []any{&d.Day, &d.Clicks} })
	if err != nil {
		return Stats{}, err
	}

	rows, err = tx.StmtContext(ctx, s.referrer).QueryContext(ctx, linkID, topReferrers)
	if err != nil {
		return Stats{}, err
	}
	st.Referrers, err = scanAll(rows, func(r *ReferrerCount) []any { return []any{&r.Host, &r.Clicks} })
	if err != nil {
		return Stats{}, err
	}
	return st, tx.Commit()
}

func scanAll[T any](rows *sql.Rows, fields func(*T) []any) ([]T, error) {
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var v T
		if err := rows.Scan(fields(&v)...); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func nullableUnix(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) &&
		(se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}
