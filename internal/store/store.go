package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type Store struct {
	db *sql.DB
}

func New(dbPath string) (*Store, error) {
	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if dir != "." && dir != "" {
		os.MkdirAll(dir, 0755)
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Run schema migration
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	// Idempotent column-add migrations. addColumn ignores the expected
	// "duplicate column" error (the column already exists) but surfaces anything
	// else — a real migration failure used to be swallowed here, silently leaving
	// the schema wrong until an INSERT failed with "no such column".
	migrations := []string{
		`ALTER TABLE providers ADD COLUMN last_error TEXT DEFAULT ''`,
		`ALTER TABLE capabilities ADD COLUMN chat_tested_at DATETIME`,
		`ALTER TABLE capabilities ADD COLUMN responses_basic BOOLEAN`,
		`ALTER TABLE capabilities ADD COLUMN responses_streaming BOOLEAN`,
		`ALTER TABLE capabilities ADD COLUMN responses_tool_use BOOLEAN`,
		`ALTER TABLE capabilities ADD COLUMN responses_tested_at DATETIME`,
		`ALTER TABLE fingerprint_results ADD COLUMN l1 INTEGER`,
		`ALTER TABLE fingerprint_results ADD COLUMN l2 INTEGER`,
		`ALTER TABLE fingerprint_results ADD COLUMN l3 INTEGER`,
		`ALTER TABLE fingerprint_results ADD COLUMN l4 INTEGER`,
		`ALTER TABLE fingerprint_results ADD COLUMN expected_tier TEXT`,
		`ALTER TABLE fingerprint_results ADD COLUMN expected_min INTEGER`,
		`ALTER TABLE fingerprint_results ADD COLUMN self_id_verdict TEXT`,
		`ALTER TABLE fingerprint_results ADD COLUMN self_id_detail TEXT`,
		`ALTER TABLE fingerprint_results ADD COLUMN answers_json TEXT`,
	}
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate schema (%s): %w", stmt, err)
		}
	}

	// Backfill migrations (idempotent UPDATEs); a failure here is worth surfacing.
	backfills := []string{
		`UPDATE capabilities SET responses_basic = 1 WHERE responses_basic IS NULL AND (responses_streaming = 1 OR responses_tool_use = 1)`,
		`UPDATE capabilities SET chat_tested_at = tested_at WHERE chat_tested_at IS NULL AND (streaming IS NOT NULL OR tool_use IS NOT NULL)`,
		`UPDATE capabilities SET responses_tested_at = tested_at WHERE responses_tested_at IS NULL AND (responses_basic IS NOT NULL OR responses_streaming IS NOT NULL OR responses_tool_use IS NOT NULL)`,
	}
	for _, stmt := range backfills {
		if _, err := db.Exec(stmt); err != nil {
			log.Printf("[store] backfill migration failed: %v", err)
		}
	}

	s := &Store{db: db}
	if err := s.AbortRunningCheckRuns("aborted during restart"); err != nil {
		db.Close()
		return nil, fmt.Errorf("abort stale runs: %w", err)
	}
	if err := s.seedCurrentResultsFromHistoryIfEmpty(); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed current results: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Cleanup(retentionDays int) error {
	// Keep full history within retention instead of collapsing history into one row.
	_, err := s.db.Exec(`DELETE FROM check_results WHERE checked_at < datetime('now', ? || ' days')`, -retentionDays)
	if err != nil {
		return err
	}
	// Expire current_results rows not refreshed within maxStaleAge. A snapshot is
	// "current" only if re-verified recently; without this, failed/disabled
	// providers' rows linger for months and pollute the dashboard, model page and
	// routing. New data from each check refreshes live providers' checked_at, so
	// only genuinely-stale rows are removed here.
	if _, err := s.db.Exec(
		`DELETE FROM current_results WHERE checked_at < datetime('now', ?)`,
		fmt.Sprintf("-%d hours", int(maxStaleAge.Hours())),
	); err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM fingerprint_results WHERE checked_at < datetime('now', ? || ' days')`, -retentionDays)
	if err != nil {
		return err
	}
	// Keep only recent events
	_, err = s.db.Exec(`DELETE FROM events WHERE created_at < datetime('now', ? || ' days')`, -retentionDays)
	if err != nil {
		return err
	}
	// Clean old check runs
	_, err = s.db.Exec(`DELETE FROM check_runs WHERE started_at < datetime('now', ? || ' days')`, -retentionDays)
	if err != nil {
		return err
	}
	// VACUUM rewrites the entire DB file and holds a write lock throughout.
	// Run it synchronously: Cleanup already executes on a background cadence,
	// and an unsupervised `go db.Exec` discarded errors while concurrent
	// writers silently ate busy_timeout failures with nothing in the logs.
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}

func (s *Store) seedCurrentResultsFromHistoryIfEmpty() error {
	count, err := s.CurrentResultsCount()
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	runRows, err := s.db.Query(`
		SELECT cr.provider_id, cr.run_id, MAX(cr.id) AS max_result_id
		FROM check_results cr
		JOIN check_runs runs ON runs.id = cr.run_id
		WHERE runs.status = 'completed'
		  AND runs.trigger_type != 'warmup'
		  AND (runs.mode = 'full' OR runs.trigger_type = 'scheduled')
		GROUP BY cr.provider_id, cr.run_id
		ORDER BY cr.provider_id, max_result_id DESC
	`)
	if err != nil {
		return err
	}
	defer runRows.Close()

	chosenRunByProvider := make(map[int64]string)
	for runRows.Next() {
		var providerID int64
		var runID string
		var maxResultID int64
		if err := runRows.Scan(&providerID, &runID, &maxResultID); err != nil {
			return err
		}
		if _, ok := chosenRunByProvider[providerID]; !ok {
			chosenRunByProvider[providerID] = runID
		}
	}
	if err := runRows.Err(); err != nil {
		return err
	}
	if len(chosenRunByProvider) == 0 {
		return nil
	}

	rows, err := s.db.Query(`
		SELECT cr.run_id, cr.provider_id, cr.model, cr.vendor, cr.status, cr.correct,
		       COALESCE(cr.answer, ''), cr.latency_ms, COALESCE(cr.error_msg, ''), cr.has_reasoning
		FROM check_results cr
		JOIN check_runs runs ON runs.id = cr.run_id
		WHERE runs.status = 'completed'
		  AND runs.trigger_type != 'warmup'
		  AND (runs.mode = 'full' OR runs.trigger_type = 'scheduled')
		ORDER BY cr.provider_id, cr.id ASC
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type seedRow struct {
		runID      string
		providerID int64
		result     CheckResultInput
	}

	var snapshotRows []seedRow
	for rows.Next() {
		var row seedRow
		if err := rows.Scan(
			&row.runID,
			&row.providerID,
			&row.result.Model,
			&row.result.Vendor,
			&row.result.Status,
			&row.result.Correct,
			&row.result.Answer,
			&row.result.LatencyMs,
			&row.result.ErrorMsg,
			&row.result.HasReasoning,
		); err != nil {
			return err
		}
		if chosenRunByProvider[row.providerID] != row.runID {
			continue
		}
		snapshotRows = append(snapshotRows, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(snapshotRows) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM current_results`); err != nil {
		return err
	}
	for _, row := range snapshotRows {
		if _, err := tx.Exec(
			`INSERT INTO current_results (provider_id, run_id, model, vendor, status, correct, answer, latency_ms, error_msg, has_reasoning, checked_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
			row.providerID, row.runID, row.result.Model, row.result.Vendor, row.result.Status, row.result.Correct,
			row.result.Answer, row.result.LatencyMs, row.result.ErrorMsg, row.result.HasReasoning,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}
