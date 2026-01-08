package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS repos (
  id INTEGER PRIMARY KEY,
  root_path TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS commits (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  sha TEXT UNIQUE NOT NULL,
  author TEXT NOT NULL,
  subject TEXT NOT NULL,
  timestamp TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS review_jobs (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  commit_id INTEGER REFERENCES commits(id),
  git_ref TEXT NOT NULL,
  agent TEXT NOT NULL DEFAULT 'codex',
  status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed','canceled')) DEFAULT 'queued',
  enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
  started_at TEXT,
  finished_at TEXT,
  worker_id TEXT,
  error TEXT,
  prompt TEXT,
  retry_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS reviews (
  id INTEGER PRIMARY KEY,
  job_id INTEGER UNIQUE NOT NULL REFERENCES review_jobs(id),
  agent TEXT NOT NULL,
  prompt TEXT NOT NULL,
  output TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  addressed INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS responses (
  id INTEGER PRIMARY KEY,
  commit_id INTEGER NOT NULL REFERENCES commits(id),
  responder TEXT NOT NULL,
  response TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_review_jobs_status ON review_jobs(status);
CREATE INDEX IF NOT EXISTS idx_review_jobs_repo ON review_jobs(repo_id);
CREATE INDEX IF NOT EXISTS idx_review_jobs_git_ref ON review_jobs(git_ref);
CREATE INDEX IF NOT EXISTS idx_commits_sha ON commits(sha);
`

type DB struct {
	*sql.DB
}

// DefaultDBPath returns the default database path
func DefaultDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".roborev", "reviews.db")
}

// Open opens or creates the database at the given path
func Open(dbPath string) (*DB, error) {
	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	// Open with WAL mode and busy timeout
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	wrapped := &DB{db}

	// Initialize schema (CREATE IF NOT EXISTS is idempotent)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize schema: %w", err)
	}

	// Run migrations for existing databases
	if err := wrapped.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return wrapped, nil
}

// migrate runs any needed migrations for existing databases
func (db *DB) migrate() error {
	// Migration: add prompt column to review_jobs if missing
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'prompt'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check prompt column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN prompt TEXT`)
		if err != nil {
			return fmt.Errorf("add prompt column: %w", err)
		}
	}

	// Migration: add addressed column to reviews if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('reviews') WHERE name = 'addressed'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check addressed column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE reviews ADD COLUMN addressed INTEGER NOT NULL DEFAULT 0`)
		if err != nil {
			return fmt.Errorf("add addressed column: %w", err)
		}
	}

	// Migration: add retry_count column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'retry_count'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check retry_count column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`)
		if err != nil {
			return fmt.Errorf("add retry_count column: %w", err)
		}
	}

	return nil
}

// ResetStaleJobs marks all running jobs as queued (for daemon restart)
func (db *DB) ResetStaleJobs() error {
	_, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'queued', worker_id = NULL, started_at = NULL
		WHERE status = 'running'
	`)
	return err
}
