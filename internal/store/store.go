// Package store is the only package that talks to SQLite.
//
// The driver is imported in this one file. To use a pure-Go driver instead
// (for example modernc.org/sqlite, which needs no C compiler), change the
// import, the driver name in sql.Open and the DSN options in Open.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver (needs CGO)
)

//go:embed schema.sql
var schemaV1 string

// Errors the rest of the application reacts to.
var (
	ErrNotFound      = errors.New("not found")
	ErrInvalidState  = errors.New("operation not allowed in the current state")
	ErrOpenJobExists = errors.New("an unfinished update already exists for this feed")
)

// OpenJobError carries the id of the unfinished job that blocks a new one.
type OpenJobError struct{ JobID int64 }

func (e *OpenJobError) Error() string {
	return fmt.Sprintf("update %d for this feed is not finished yet; resume or cancel it first", e.JobID)
}
func (e *OpenJobError) Unwrap() error { return ErrOpenJobExists }

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	// Options, all of which matter for reliability:
	//   foreign_keys=on      enforce REFERENCES
	//   journal_mode=WAL     readers do not block the importing writer
	//   synchronous=FULL     a committed batch survives power loss, not only a crash
	//   busy_timeout=5000    wait for the write lock instead of failing at once
	//   txlock=immediate     take the write lock at BEGIN, avoiding upgrade deadlocks
	dsn := path + "?_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000&_txlock=immediate"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(8)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }


func (s *Store) migrate(ctx context.Context) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		var version int
		if err := tx.tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		switch version {
		case 0:
			if _, err := tx.tx.ExecContext(ctx, schemaV1); err != nil {
				return fmt.Errorf("create schema: %w", err)
			}
			if _, err := tx.tx.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
				return fmt.Errorf("set schema version: %w", err)
			}
		case 1:
			// up to date
		default:
			return fmt.Errorf("database schema version %d is newer than this application understands", version)
		}
		return nil
	})
}

// Tx is a database transaction. Get one through Store.WithTx.
type Tx struct {
	tx *sql.Tx
}

// WithTx runs fn in a transaction: commit if fn returns nil, roll back otherwise.
// If ctx is cancelled the transaction is rolled back too.
func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) (err error) {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = sqlTx.Rollback()
		}
	}()
	if err = fn(&Tx{tx: sqlTx}); err != nil {
		return err
	}
	if err = sqlTx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// --- time helpers -----------------------------------------------------------

// Times are stored as fixed-width UTC text, so they are readable in the
// sqlite3 shell and sort correctly as strings.
const timeLayout = "2006-01-02T15:04:05.000Z"

func timeText(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

func parseNullTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// --- small SQL helpers ------------------------------------------------------

type scanner interface{ Scan(dest ...any) error }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// clampPage keeps limit/offset within sane bounds.
func clampPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
