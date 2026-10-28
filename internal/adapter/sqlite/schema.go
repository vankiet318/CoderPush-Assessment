package sqlite

import (
	"database/sql"
	"fmt"
)

var schema = []string{
	`CREATE TABLE IF NOT EXISTS products (
		sku               TEXT PRIMARY KEY,
		name              TEXT NOT NULL,
		category          TEXT NOT NULL,
		price_cents       INTEGER NOT NULL,
		status            TEXT NOT NULL,
		source_updated_at TEXT NOT NULL,
		last_run_id       INTEGER NOT NULL,
		local_updated_at  TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS import_runs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		feed        TEXT NOT NULL,
		status      TEXT NOT NULL,
		cursor      TEXT NOT NULL,
		counters    TEXT NOT NULL,
		last_error  TEXT NOT NULL,
		started_at  TEXT NOT NULL,
		updated_at  TEXT NOT NULL,
		finished_at TEXT NOT NULL
	)`,
	// At most one update may be running at any time.
	`CREATE UNIQUE INDEX IF NOT EXISTS import_runs_one_running ON import_runs(status) WHERE status = 'RUNNING'`,
	`CREATE TABLE IF NOT EXISTS changes (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id   INTEGER NOT NULL REFERENCES import_runs(id),
		batch_id TEXT NOT NULL,
		sku      TEXT NOT NULL,
		kind     TEXT NOT NULL,
		fields   TEXT NOT NULL,
		reason   TEXT NOT NULL,
		at       TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS changes_run ON changes(run_id)`,
	`CREATE TABLE IF NOT EXISTS issues (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id      INTEGER NOT NULL REFERENCES import_runs(id),
		sku         TEXT NOT NULL,
		kind        TEXT NOT NULL,
		message     TEXT NOT NULL,
		fingerprint TEXT NOT NULL UNIQUE,
		status      TEXT NOT NULL,
		created_at  TEXT NOT NULL,
		resolved_at TEXT NOT NULL
	)`,
}

func migrate(db *sql.DB) error {
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}
