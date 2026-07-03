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

// migrate creates the events table and its indexes if they do not exist, then
// adds any columns missing from an older database. New columns must be appended
// both to the CREATE TABLE above (for fresh databases) and to the ensureColumns
// map below (for databases created by an earlier version).
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
			duration_ms INTEGER,
			trace_id TEXT,
			span_id TEXT,
			metadata TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_developer ON events (developer_id, ts_unix_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_events_team ON events (team, ts_unix_ms)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("exec migration: %w", err)
		}
	}
	// Columns added after the initial release. SQLite has no
	// "ADD COLUMN IF NOT EXISTS", so add only what a pre-existing table lacks.
	return ensureColumns(db, "events", []column{
		{name: "trace_id", typ: "TEXT"},
		{name: "span_id", typ: "TEXT"},
		{name: "metadata", typ: "TEXT"},
	})
}

// column is a table column name and its SQLite type.
type column struct {
	name string
	typ  string
}

// ensureColumns adds any of cols missing from table, using PRAGMA table_info to
// discover the existing columns. It is idempotent: already-present columns are
// left untouched.
func ensureColumns(db *sql.DB, table string, cols []column) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	existing := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan %s column: %w", table, err)
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate %s columns: %w", table, err)
	}
	_ = rows.Close()

	for _, c := range cols {
		if existing[c.name] {
			continue
		}
		// #nosec G202 -- table and column identifiers are fixed literals from
		// migrate(), never external input; only the type is interpolated.
		stmt := `ALTER TABLE ` + table + ` ADD COLUMN ` + c.name + ` ` + c.typ
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("add column %s.%s: %w", table, c.name, err)
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
		status, input_tokens, output_tokens, tokens_approximated, cost_usd, duration_ms,
		trace_id, span_id, metadata
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

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
		e.TraceID,
		e.SpanID,
		e.Metadata,
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

// EventRow is a single recorded request as surfaced to the dashboard, including
// its SQLite rowid so callers can tail new rows.
type EventRow struct {
	RowID           int64   `json:"row_id"`
	TimestampUnixMs int64   `json:"ts_unix_ms"`
	Provider        string  `json:"provider"`
	Model           string  `json:"model"`
	DeveloperID     string  `json:"developer_id"`
	Team            string  `json:"team"`
	Status          int     `json:"status"`
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	CostUSD         float64 `json:"cost_usd"`
	DurationMs      int64   `json:"duration_ms"`
}

// eventCols is the fixed SELECT list shared by the event-row queries.
const eventCols = `rowid, ts_unix_ms, provider, model, developer_id, team, status, input_tokens, output_tokens, cost_usd, duration_ms`

// scanEventRows scans a result set produced with eventCols into EventRow values.
func scanEventRows(rows *sql.Rows) ([]EventRow, error) {
	defer func() { _ = rows.Close() }()
	var out []EventRow
	for rows.Next() {
		var r EventRow
		if err := rows.Scan(&r.RowID, &r.TimestampUnixMs, &r.Provider, &r.Model,
			&r.DeveloperID, &r.Team, &r.Status, &r.InputTokens, &r.OutputTokens,
			&r.CostUSD, &r.DurationMs); err != nil {
			return nil, fmt.Errorf("scan event row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event rows: %w", err)
	}
	return out, nil
}

// RecentEvents returns the most recent up-to-limit events in ascending rowid
// order, plus the current maximum rowid in the table (0 when empty). It is used
// for the dashboard's initial load; the returned maxRowID seeds EventsSince for
// live tailing even when the table has more than limit rows.
func (s *Store) RecentEvents(limit int) ([]EventRow, int64, error) {
	// #nosec G202 -- eventCols is a fixed literal; limit is bound as a parameter.
	q := `SELECT ` + eventCols + ` FROM events ORDER BY rowid DESC LIMIT ?`
	rows, err := s.db.Query(q, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("query recent events: %w", err)
	}
	desc, err := scanEventRows(rows)
	if err != nil {
		return nil, 0, err
	}
	// Reverse to ascending rowid order for display; the first DESC row is the max.
	var maxRowID int64
	if len(desc) > 0 {
		maxRowID = desc[0].RowID
	}
	out := make([]EventRow, len(desc))
	for i, r := range desc {
		out[len(desc)-1-i] = r
	}
	return out, maxRowID, nil
}

// EventsSince returns up-to-limit events with rowid greater than sinceRowID in
// ascending order, plus the new maximum rowid to poll from next (sinceRowID when
// no new rows). It powers the dashboard's live tail.
func (s *Store) EventsSince(sinceRowID int64, limit int) ([]EventRow, int64, error) {
	// #nosec G202 -- eventCols is a fixed literal; args are bound as parameters.
	q := `SELECT ` + eventCols + ` FROM events WHERE rowid > ? ORDER BY rowid ASC LIMIT ?`
	rows, err := s.db.Query(q, sinceRowID, limit)
	if err != nil {
		return nil, sinceRowID, fmt.Errorf("query events since %d: %w", sinceRowID, err)
	}
	out, err := scanEventRows(rows)
	if err != nil {
		return nil, sinceRowID, err
	}
	maxRowID := sinceRowID
	if n := len(out); n > 0 {
		maxRowID = out[n-1].RowID // ascending, so last is max
	}
	return out, maxRowID, nil
}

// EventMetadata returns the stored metadata JSON blob for a single event by
// rowid. The string is empty when the row has no metadata; sql.ErrNoRows is
// returned when the rowid does not exist.
func (s *Store) EventMetadata(rowID int64) (string, error) {
	var meta sql.NullString
	err := s.db.QueryRow(`SELECT metadata FROM events WHERE rowid = ?`, rowID).Scan(&meta)
	if err != nil {
		return "", fmt.Errorf("query event %d metadata: %w", rowID, err)
	}
	return meta.String, nil
}

// ReportRow is one grouped spend total keyed by a dimension value (a developer
// id, team, or model), with its aggregate cost and request count.
type ReportRow struct {
	Key      string
	CostUSD  float64
	Requests int
}

// Report is a spend summary grouped by developer, team, and model, plus the
// overall totals across all matching events.
type Report struct {
	ByDeveloper   []ReportRow
	ByTeam        []ReportRow
	ByModel       []ReportRow
	TotalUSD      float64
	TotalRequests int
}

// SpendReport summarizes recorded spend since the given time. It groups events
// by developer_id, team, and model (summing cost_usd and counting rows) and
// computes the overall totals. The query is read-only and fully parameterized.
func (s *Store) SpendReport(since time.Time) (Report, error) {
	sinceMs := since.UnixMilli()

	byDeveloper, err := s.groupBy("developer_id", sinceMs)
	if err != nil {
		return Report{}, err
	}
	byTeam, err := s.groupBy("team", sinceMs)
	if err != nil {
		return Report{}, err
	}
	byModel, err := s.groupBy("model", sinceMs)
	if err != nil {
		return Report{}, err
	}

	const totalsQ = `SELECT COALESCE(SUM(cost_usd), 0), COUNT(*) FROM events WHERE ts_unix_ms >= ?`
	var (
		totalUSD      float64
		totalRequests int
	)
	if err := s.db.QueryRow(totalsQ, sinceMs).Scan(&totalUSD, &totalRequests); err != nil {
		return Report{}, fmt.Errorf("query spend totals: %w", err)
	}

	return Report{
		ByDeveloper:   byDeveloper,
		ByTeam:        byTeam,
		ByModel:       byModel,
		TotalUSD:      totalUSD,
		TotalRequests: totalRequests,
	}, nil
}

// groupBy returns the per-value cost and request totals for a single grouping
// column since sinceMs. The column is chosen by callers from a fixed set (never
// interpolated from external input) and all values are bound as parameters.
func (s *Store) groupBy(col string, sinceMs int64) ([]ReportRow, error) {
	// #nosec G202 -- col is a fixed literal chosen by SpendReport, not input.
	q := `SELECT ` + col + `, COALESCE(SUM(cost_usd), 0), COUNT(*) FROM events WHERE ts_unix_ms >= ? GROUP BY ` + col + ` ORDER BY SUM(cost_usd) DESC`
	rows, err := s.db.Query(q, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("query spend by %s: %w", col, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ReportRow
	for rows.Next() {
		var r ReportRow
		if err := rows.Scan(&r.Key, &r.CostUSD, &r.Requests); err != nil {
			return nil, fmt.Errorf("scan spend by %s: %w", col, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spend by %s: %w", col, err)
	}
	return out, nil
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
