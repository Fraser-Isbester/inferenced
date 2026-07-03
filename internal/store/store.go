// Package store persists metered telemetry events to a local SQLite ledger and
// answers spend queries used by the enforcement engine.
//
// It uses the pure-Go modernc.org/sqlite driver so the binary builds with CGO
// disabled. The Store implements telemetry.Sink, so it can be composed into the
// gateway's sink fan-out alongside the stdout sink.
package store

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	// Register the pure-Go SQLite driver under the name "sqlite".
	_ "modernc.org/sqlite"

	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

// Store is a SQLite-backed spend ledger. It is safe for concurrent use; the
// underlying *sql.DB manages its own connection pool.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite ledger at path, creating any
// missing parent directories, enabling WAL mode and a busy timeout, and running
// the idempotent schema migration.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir %s: %w", dir, err)
		}
	}

	// Set pragmas in the DSN so EVERY pooled connection inherits them. Running
	// "PRAGMA busy_timeout" via Exec only configures one connection; busy_timeout
	// is per-connection (unlike WAL, which persists to the file), so additional
	// pooled connections would get busy_timeout=0 and fail writes with
	// SQLITE_BUSY under concurrency instead of waiting — silently dropping ledger
	// inserts and failing Spend queries, which would undercount spend.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// DefaultPath returns the default ledger location:
// $XDG_DATA_HOME/inferenced/inferenced.db, falling back to
// ~/.local/share/inferenced/inferenced.db.
func DefaultPath() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "inferenced", "inferenced.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", "inferenced", "inferenced.db"), nil
}

// migrate creates the events table and its indexes if they do not exist.
func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS events (
			request_id TEXT,
			ts_unix_ms INTEGER,
			developer_id TEXT,
			team TEXT,
			provider TEXT,
			model TEXT,
			stream INTEGER,
			status INTEGER,
			input_tokens INTEGER,
			output_tokens INTEGER,
			tokens_approximated INTEGER,
			cost_usd REAL,
			duration_ms INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_developer ON events (developer_id, ts_unix_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_events_team ON events (team, ts_unix_ms)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("exec migration: %w", err)
		}
	}
	return nil
}

// Emit records the event as a row in the ledger. It implements telemetry.Sink.
// Insertion errors are logged and never panic, so a ledger failure cannot take
// down the proxy.
func (s *Store) Emit(e telemetry.Event) {
	const q = `INSERT INTO events (
		request_id, ts_unix_ms, developer_id, team, provider, model, stream,
		status, input_tokens, output_tokens, tokens_approximated, cost_usd, duration_ms
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	if _, err := s.db.Exec(q,
		e.RequestID,
		e.TimestampUnixMs,
		e.DeveloperID,
		e.Team,
		e.Provider,
		e.Model,
		boolToInt(e.Stream),
		e.Status,
		e.InputTokens,
		e.OutputTokens,
		boolToInt(e.TokensApproximated),
		e.CostUSD,
		e.DurationMs,
	); err != nil {
		log.Printf("[STORE] insert event %s: %v", e.RequestID, err)
	}
}

// Spend returns the total cost in US dollars recorded for the given scope and
// id since the given time. scope must be "developer" or "team"; any other value
// is an error. An id of "*" sums all rows for that scope's column; otherwise
// the sum is filtered to the matching id. The column is chosen from a fixed set
// (never interpolated from input) and all values are bound as parameters.
func (s *Store) Spend(scope, id string, since time.Time) (float64, error) {
	var col string
	switch scope {
	case "developer":
		col = "developer_id"
	case "team":
		col = "team"
	default:
		return 0, fmt.Errorf("unknown scope %q", scope)
	}

	sinceMs := since.UnixMilli()

	var (
		total float64
		err   error
	)
	if id == "*" {
		const q = `SELECT COALESCE(SUM(cost_usd), 0) FROM events WHERE ts_unix_ms >= ?`
		err = s.db.QueryRow(q, sinceMs).Scan(&total)
	} else {
		// #nosec G202 -- col is chosen from a fixed allow-list above, not input.
		q := `SELECT COALESCE(SUM(cost_usd), 0) FROM events WHERE ` + col + ` = ? AND ts_unix_ms >= ?`
		err = s.db.QueryRow(q, id, sinceMs).Scan(&total)
	}
	if err != nil {
		return 0, fmt.Errorf("query spend for %s %q: %w", scope, id, err)
	}
	return total, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close store: %w", err)
	}
	return nil
}

// boolToInt maps a bool to the 0/1 integer stored in SQLite.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
