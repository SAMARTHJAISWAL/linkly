//go:build !js

package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations returns the embedded migrations ordered by version. Files
// are named NNNN_description.sql.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %q: name must start with a positive version, e.g. 0001_", name)
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", other, name, v)
		}
		seen[v] = name
		body, err := fs.ReadFile(fsys, "migrations/"+name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: name, sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

// migrate brings the database up to the newest embedded migration.
//
// Several processes may start against the same file at once (a rolling
// deploy, two replicas sharing a volume), so the version check and every
// pending migration run inside one BEGIN IMMEDIATE transaction: whoever
// takes the write lock first migrates, and the others then find nothing to
// do. Waiting for that lock is normally absorbed by busy_timeout; if
// SQLite still reports SQLITE_BUSY (it does not invoke the busy handler in
// every situation), the whole attempt is retried with backoff.
func migrate(ctx context.Context, db *sql.DB) error {
	ms, err := loadMigrations(migrationFS)
	if err != nil {
		return err
	}
	backoff := 10 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := migrateOnce(ctx, db, ms)
		if err == nil || !isBusy(err) || attempt == migrateAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, time.Second)
	}
}

// migrateAttempts bounds the retries; with the backoff above (and a 5s
// busy_timeout inside each attempt) that is well over a minute of waiting.
const migrateAttempts = 12

func migrateOnce(ctx context.Context, db *sql.DB, ms []migration) (err error) {
	// Foreign keys are switched off for the migration, as SQLite's
	// procedure for rebuilding a table requires (dropping a referenced table
	// would otherwise cascade), and checked before committing. The pragma
	// is a no-op inside a transaction and is per connection, so a
	// connection is pinned for the whole sequence.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() {
		// Background: this must run even if ctx is done, or the pooled
		// connection would keep foreign keys off.
		if _, ferr := conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`); ferr != nil {
			err = errors.Join(err, ferr)
		}
	}()

	// The write pool opens transactions with BEGIN IMMEDIATE (_txlock), so
	// the write lock is taken before the version is read.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	) STRICT`); err != nil {
		return err
	}
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	for _, m := range ms {
		if m.version <= current {
			continue
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("applying %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, time.Now().Unix()); err != nil {
			return fmt.Errorf("recording %s: %w", m.name, err)
		}
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	violation := rows.Next()
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if violation {
		return errors.New("migrations left foreign key violations")
	}
	return tx.Commit()
}

func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}
