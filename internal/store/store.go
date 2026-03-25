package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

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

	// Add last_error column if missing (migration)
	db.Exec(`ALTER TABLE providers ADD COLUMN last_error TEXT DEFAULT ''`)

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Cleanup(retentionDays int) error {
	// Only keep the latest check result per provider+model, delete older ones
	_, err := s.db.Exec(`DELETE FROM check_results WHERE id NOT IN (
		SELECT MAX(id) FROM check_results GROUP BY provider_id, model
	)`)
	if err != nil {
		return err
	}
	// Keep only latest fingerprint per provider+model
	_, err = s.db.Exec(`DELETE FROM fingerprint_results WHERE id NOT IN (
		SELECT MAX(id) FROM fingerprint_results GROUP BY provider_id, model
	)`)
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
	// Vacuum to reclaim space
	_, err = s.db.Exec(`VACUUM`)
	return err
}
