package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ProviderRow represents a provider record from the DB.
type ProviderRow struct {
	ID          int64
	Name        string
	BaseURL     string
	APIFormat   string
	Platform    string
	Status      string
	Health      float64
	LastBalance *float64 // nil = not supported
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CheckResultRow represents a check result from the DB.
type CheckResultRow struct {
	ID           int64
	RunID        string
	ProviderID   int64
	ProviderName string // joined from providers
	Model        string
	Vendor       string
	Status       string
	Correct      bool
	Answer       string
	LatencyMs    int64
	ErrorMsg     string
	HasReasoning bool
	CheckedAt    time.Time
}

// EventRow represents an event from the DB.
type EventRow struct {
	ID        int64
	Type      string
	Provider  string
	Model     string
	OldValue  string
	NewValue  string
	Message   string
	Read      bool
	CreatedAt time.Time
}

func (s *Store) UpsertProvider(name, baseURL, apiFormat, platform string) (int64, error) {
	if apiFormat == "" {
		apiFormat = "chat"
	}
	if platform == "" {
		platform = "unknown"
	}

	_, err := s.db.Exec(`
		INSERT INTO providers (name, base_url, api_format, platform, updated_at)
		VALUES (?, ?, ?, ?, datetime('now'))
		ON CONFLICT(name) DO UPDATE SET
			base_url = excluded.base_url,
			api_format = excluded.api_format,
			platform = CASE WHEN excluded.platform != 'unknown' THEN excluded.platform ELSE providers.platform END,
			updated_at = datetime('now')
	`, name, baseURL, apiFormat, platform)
	if err != nil {
		return 0, fmt.Errorf("upsert provider: %w", err)
	}

	// Get the ID (either inserted or existing)
	var id int64
	err = s.db.QueryRow(`SELECT id FROM providers WHERE name = ?`, name).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) UpdateProviderStatus(id int64, status string, health float64, lastError string) error {
	_, err := s.db.Exec(`UPDATE providers SET status = ?, health = ?, last_error = ?, updated_at = datetime('now') WHERE id = ?`,
		status, health, lastError, id)
	return err
}

func (s *Store) GetProviders() ([]ProviderRow, error) {
	rows, err := s.db.Query(`SELECT id, name, base_url, api_format, platform, status, health, last_balance, COALESCE(last_error,''), created_at, updated_at FROM providers ORDER BY health DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ProviderRow
	for rows.Next() {
		var r ProviderRow
		var balance sql.NullFloat64
		err := rows.Scan(&r.ID, &r.Name, &r.BaseURL, &r.APIFormat, &r.Platform, &r.Status, &r.Health, &balance, &r.LastError, &r.CreatedAt, &r.UpdatedAt)
		if err != nil {
			return nil, err
		}
		if balance.Valid {
			r.LastBalance = &balance.Float64
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) GetProviderByName(name string) (*ProviderRow, error) {
	var r ProviderRow
	var balance sql.NullFloat64
	err := s.db.QueryRow(`SELECT id, name, base_url, api_format, platform, status, health, last_balance, created_at, updated_at FROM providers WHERE name = ?`, name).
		Scan(&r.ID, &r.Name, &r.BaseURL, &r.APIFormat, &r.Platform, &r.Status, &r.Health, &balance, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if balance.Valid {
		r.LastBalance = &balance.Float64
	}
	return &r, nil
}

func (s *Store) InsertCheckRun(id, mode, triggerType string) error {
	_, err := s.db.Exec(`INSERT INTO check_runs (id, mode, status, trigger_type, started_at) VALUES (?, ?, 'running', ?, datetime('now'))`,
		id, mode, triggerType)
	return err
}

func (s *Store) FinishCheckRun(id string, providersCount, modelsCount, okCount, correctCount int, summary string) error {
	_, err := s.db.Exec(`UPDATE check_runs SET status = 'completed', ended_at = datetime('now'), providers_count = ?, models_count = ?, ok_count = ?, correct_count = ?, summary = ? WHERE id = ?`,
		providersCount, modelsCount, okCount, correctCount, summary, id)
	return err
}

func (s *Store) InsertCheckResult(runID string, providerID int64, model, vendor, status string, correct bool, answer string, latencyMs int64, errorMsg string, hasReasoning bool) error {
	_, err := s.db.Exec(`INSERT INTO check_results (run_id, provider_id, model, vendor, status, correct, answer, latency_ms, error_msg, has_reasoning) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, providerID, model, vendor, status, correct, answer, latencyMs, errorMsg, hasReasoning)
	return err
}

// GetLatestResults returns the most recent check result for each provider+model combination.
func (s *Store) GetLatestResults() ([]CheckResultRow, error) {
	rows, err := s.db.Query(`
		SELECT cr.id, cr.run_id, cr.provider_id, p.name, cr.model, cr.vendor, cr.status, cr.correct,
		       COALESCE(cr.answer, ''), cr.latency_ms, COALESCE(cr.error_msg, ''), cr.has_reasoning, cr.checked_at
		FROM check_results cr
		JOIN providers p ON p.id = cr.provider_id
		WHERE cr.id IN (
			SELECT MAX(id) FROM check_results GROUP BY provider_id, model
		)
		ORDER BY p.name, cr.model
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCheckResults(rows)
}

// GetProviderResults returns the most recent results for a specific provider.
func (s *Store) GetProviderResults(providerID int64) ([]CheckResultRow, error) {
	rows, err := s.db.Query(`
		SELECT cr.id, cr.run_id, cr.provider_id, p.name, cr.model, cr.vendor, cr.status, cr.correct,
		       COALESCE(cr.answer, ''), cr.latency_ms, COALESCE(cr.error_msg, ''), cr.has_reasoning, cr.checked_at
		FROM check_results cr
		JOIN providers p ON p.id = cr.provider_id
		WHERE cr.provider_id = ? AND cr.id IN (
			SELECT MAX(id) FROM check_results WHERE provider_id = ? GROUP BY model
		)
		ORDER BY cr.model
	`, providerID, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCheckResults(rows)
}

func scanCheckResults(rows *sql.Rows) ([]CheckResultRow, error) {
	var result []CheckResultRow
	for rows.Next() {
		var r CheckResultRow
		err := rows.Scan(&r.ID, &r.RunID, &r.ProviderID, &r.ProviderName, &r.Model, &r.Vendor,
			&r.Status, &r.Correct, &r.Answer, &r.LatencyMs, &r.ErrorMsg, &r.HasReasoning, &r.CheckedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) InsertEvent(typ, prov, model, oldVal, newVal, message string) error {
	_, err := s.db.Exec(`INSERT INTO events (type, provider, model, old_value, new_value, message) VALUES (?, ?, ?, ?, ?, ?)`,
		typ, prov, model, oldVal, newVal, message)
	return err
}

func (s *Store) GetRecentEvents(limit int) ([]EventRow, error) {
	rows, err := s.db.Query(`SELECT id, type, provider, COALESCE(model, ''), COALESCE(old_value, ''), COALESCE(new_value, ''), message, read, created_at FROM events ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []EventRow
	for rows.Next() {
		var r EventRow
		err := rows.Scan(&r.ID, &r.Type, &r.Provider, &r.Model, &r.OldValue, &r.NewValue, &r.Message, &r.Read, &r.CreatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) GetUnreadEventCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE read = 0`).Scan(&count)
	return count, err
}

func (s *Store) MarkEventsRead() error {
	_, err := s.db.Exec(`UPDATE events SET read = 1 WHERE read = 0`)
	return err
}

func (s *Store) UpdateProviderBalance(id int64, balance float64) error {
	_, err := s.db.Exec(`UPDATE providers SET last_balance = ?, updated_at = datetime('now') WHERE id = ?`, balance, id)
	return err
}

func (s *Store) RenameProvider(id int64, newName string) error {
	_, err := s.db.Exec(`UPDATE providers SET name = ?, updated_at = datetime('now') WHERE id = ?`, newName, id)
	return err
}

// DeleteProvider removes a provider and all its associated data.
func (s *Store) DeleteProvider(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`DELETE FROM check_results WHERE provider_id = ?`,
		`DELETE FROM fingerprint_results WHERE provider_id = ?`,
		`DELETE FROM capabilities WHERE provider_id = ?`,
		`DELETE FROM events WHERE provider = (SELECT name FROM providers WHERE id = ?)`,
		`DELETE FROM providers WHERE id = ?`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt, id); err != nil {
			return fmt.Errorf("delete provider %d: %w", id, err)
		}
	}
	return tx.Commit()
}

func (s *Store) UpdateProviderPlatform(id int64, platform string) error {
	_, err := s.db.Exec(`UPDATE providers SET platform = ?, updated_at = datetime('now') WHERE id = ?`, platform, id)
	return err
}

func (s *Store) UpsertCapability(providerID int64, model string, streaming, toolUse *bool) error {
	_, err := s.db.Exec(`
		INSERT INTO capabilities (provider_id, model, streaming, tool_use, tested_at)
		VALUES (?, ?, ?, ?, datetime('now'))
		ON CONFLICT(provider_id, model) DO UPDATE SET
			streaming = excluded.streaming,
			tool_use = excluded.tool_use,
			tested_at = datetime('now')
	`, providerID, model, streaming, toolUse)
	return err
}

// CapabilityRow holds capability data for a model on a provider.
type CapabilityRow struct {
	ProviderID int64
	Model      string
	Streaming  *bool
	ToolUse    *bool
}

func (s *Store) GetCapabilities(providerID int64) ([]CapabilityRow, error) {
	rows, err := s.db.Query(`SELECT provider_id, model, streaming, tool_use FROM capabilities WHERE provider_id = ?`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CapabilityRow
	for rows.Next() {
		var r CapabilityRow
		if err := rows.Scan(&r.ProviderID, &r.Model, &r.Streaming, &r.ToolUse); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) GetAllCapabilities() (map[int64]map[string]CapabilityRow, error) {
	rows, err := s.db.Query(`SELECT provider_id, model, streaming, tool_use FROM capabilities`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]map[string]CapabilityRow)
	for rows.Next() {
		var r CapabilityRow
		if err := rows.Scan(&r.ProviderID, &r.Model, &r.Streaming, &r.ToolUse); err != nil {
			return nil, err
		}
		if result[r.ProviderID] == nil {
			result[r.ProviderID] = make(map[string]CapabilityRow)
		}
		result[r.ProviderID][r.Model] = r
	}
	return result, rows.Err()
}

// FingerprintRow represents a fingerprint result joined with provider info.
type FingerprintRow struct {
	ID            int64
	ProviderID    int64
	ProviderName  string
	Model         string
	Vendor        string
	TotalScore    int
	L1, L2, L3, L4 int
	ExpectedTier  string
	ExpectedMin   int
	Verdict       string
	SelfIDVerdict string
	SelfIDDetail  string
	CheckedAt     time.Time
}

func (s *Store) InsertFingerprintResult(providerID int64, model, vendor string, totalScore, l1, l2, l3, l4 int, expectedTier string, expectedMin int, verdict, selfIDVerdict, selfIDDetail, answersJSON string) error {
	_, err := s.db.Exec(`
		INSERT INTO fingerprint_results (provider_id, model, vendor, total_score, l1, l2, l3, l4, expected_tier, expected_min, verdict, self_id_verdict, self_id_detail, answers_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		providerID, model, vendor, totalScore, l1, l2, l3, l4, expectedTier, expectedMin, verdict, selfIDVerdict, selfIDDetail, answersJSON)
	return err
}

// GetLatestFingerprints returns the most recent fingerprint result per provider+model.
func (s *Store) GetLatestFingerprints() ([]FingerprintRow, error) {
	rows, err := s.db.Query(`
		SELECT fr.id, fr.provider_id, p.name, fr.model, fr.vendor,
		       fr.total_score, COALESCE(fr.l1,0), COALESCE(fr.l2,0), COALESCE(fr.l3,0), COALESCE(fr.l4,0),
		       COALESCE(fr.expected_tier,'?'), COALESCE(fr.expected_min,0), fr.verdict,
		       COALESCE(fr.self_id_verdict,''), COALESCE(fr.self_id_detail,''), fr.checked_at
		FROM fingerprint_results fr
		JOIN providers p ON p.id = fr.provider_id
		WHERE fr.id IN (
			SELECT MAX(id) FROM fingerprint_results GROUP BY provider_id, model
		)
		ORDER BY p.name, fr.model
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []FingerprintRow
	for rows.Next() {
		var r FingerprintRow
		err := rows.Scan(&r.ID, &r.ProviderID, &r.ProviderName, &r.Model, &r.Vendor,
			&r.TotalScore, &r.L1, &r.L2, &r.L3, &r.L4,
			&r.ExpectedTier, &r.ExpectedMin, &r.Verdict,
			&r.SelfIDVerdict, &r.SelfIDDetail, &r.CheckedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
