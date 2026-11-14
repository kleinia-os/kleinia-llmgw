// Package store implements llmgw's SQLite persistence layer: upstream
// sources with their alias→model mappings, gateway API keys with quota
// limits, and the per-request usage ledger that quota windows and reporting
// are derived from.
//
// Timestamps are stored as RFC3339Nano strings in UTC so rows stay
// human-readable in sqlite3 CLI sessions; window queries compare them with
// SQLite's datetime() (see KeyWindows), which parses RFC3339Nano's variable
// fractional seconds and compares chronologically at second granularity.
//
// The modernc.org/sqlite driver is pure Go, so the gateway binary carries
// no cgo dependency. File databases open with WAL journaling, a 5s busy
// timeout and foreign keys enforced; ":memory:" databases (tests) are pinned
// to a single connection because an in-memory SQLite database is private to
// the connection that created it.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Store wraps the gateway's SQLite database. All methods are safe for
// concurrent use.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database at path and migrates it to
// the current schema version. path ":memory:" gives a private in-memory
// database for tests.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if path != ":memory:" {
		// WAL gives concurrent readers during writes; it does not apply to
		// in-memory databases.
		dsn = path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// migrations[v-1] holds the statements that bring a database from version
// v-1 to v. Each migration runs in one transaction.
var migrations = [][]string{
	{
		`CREATE TABLE sources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			type TEXT NOT NULL,
			base_url TEXT NOT NULL,
			api_key TEXT NOT NULL DEFAULT '',
			priority INTEGER NOT NULL DEFAULT 0,
			weight INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT,
			updated_at TEXT
		)`,
		`CREATE TABLE models (
			id INTEGER PRIMARY KEY,
			source_id INTEGER REFERENCES sources(id) ON DELETE CASCADE,
			alias TEXT NOT NULL,
			upstream_model TEXT NOT NULL,
			UNIQUE(source_id, alias)
		)`,
		`CREATE TABLE keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			key TEXT UNIQUE NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			models TEXT NOT NULL DEFAULT '*',
			rpm INTEGER,
			tpm INTEGER,
			daily_tokens INTEGER,
			created_at TEXT,
			revoked_at TEXT
		)`,
		`CREATE TABLE usage (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			key_id INTEGER,
			key_name TEXT,
			source_id INTEGER,
			source_name TEXT,
			client_format TEXT,
			model_alias TEXT,
			upstream_model TEXT,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			cost_est REAL,
			latency_ms INTEGER,
			ttft_ms INTEGER,
			stream INTEGER,
			status INTEGER,
			degraded INTEGER DEFAULT 0,
			session TEXT,
			error_kind TEXT
		)`,
		`CREATE INDEX idx_usage_ts ON usage(ts)`,
		`CREATE INDEX idx_usage_key_id_ts ON usage(key_id, ts)`,
	},
	// v2: agent identity (G1) — AID-bound keys, key suspension, usage
	// attribution and request ids.
	{
		`ALTER TABLE keys ADD COLUMN aid TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE keys ADD COLUMN suspended INTEGER NOT NULL DEFAULT 0`,
		`CREATE UNIQUE INDEX idx_keys_aid ON keys(aid) WHERE aid != ''`,
		`ALTER TABLE usage ADD COLUMN aid TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage ADD COLUMN req_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX idx_usage_aid_ts ON usage(aid, ts)`,
	},
}

// migrate brings the database up to len(migrations), one transaction per
// step, and is a no-op on an up-to-date database.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: create schema_version: %w", err)
	}
	var version int
	switch err := s.db.QueryRow(`SELECT version FROM schema_version LIMIT 1`).Scan(&version); {
	case errors.Is(err, sql.ErrNoRows):
		// Fresh database: version 0, no rows yet.
	case err != nil:
		return fmt.Errorf("store: read schema version: %w", err)
	}
	for v := version; v < len(migrations); v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("store: migrate to v%d: %w", v+1, err)
		}
		for _, stmt := range migrations[v] {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("store: migrate to v%d: %w", v+1, err)
			}
		}
		if _, err := tx.Exec(`DELETE FROM schema_version`); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migrate to v%d: %w", v+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, v+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migrate to v%d: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migrate to v%d: %w", v+1, err)
		}
	}
	return nil
}

// nowRFC3339 is the canonical timestamp format for stored TEXT columns.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }
