// Package dbx wraps SQLite access: connection setup, embedded versioned
// migrations, and a data access layer for the app's five tables.
//
// SQLite is the durability layer, not the hot path for playback sync — the
// party package keeps authoritative in-memory state per active party and
// writes through to here asynchronously. See ARCHITECTURE.md.
package dbx

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens (creating if necessary) the SQLite database at path, applies
// any pending migrations, and returns a ready-to-use connection pool.
//
// modernc.org/sqlite is a pure-Go driver (no CGO), which is required to keep
// the final binary statically linked for the distroless/scratch container.
func Open(path string) (*sql.DB, error) {
	// _pragma params configure SQLite for a single-process, low-write-volume
	// server: WAL for concurrent readers alongside the writer, a busy
	// timeout so concurrent writers block-and-retry instead of erroring, and
	// foreign_keys on since we rely on ON DELETE CASCADE.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("dbx: open: %w", err)
	}
	// SQLite has no real server-side connection concurrency; a single writer
	// connection avoids SQLITE_BUSY churn under WAL at this app's scale.
	db.SetMaxOpenConns(1)

	if err := migrateUp(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("dbx: migrate: %w", err)
	}
	return db, nil
}

func migrateUp(db *sql.DB) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	target, err := sqlitemigrate.WithInstance(db, &sqlitemigrate.Config{})
	if err != nil {
		return err
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", target)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}
