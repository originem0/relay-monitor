package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// TraceRow is the flat representation of a proxy trace for SQLite persistence.
type TraceRow struct {
	TraceID       string
	ReceivedAt    time.Time
	Model         string
	Endpoint      string
	Stream        bool
	HasTools      bool
	Attempts      int
	FinalProvider string
	FinalStatus   string
	LatencyMs     int64
	DetailJSON    []byte // full Trace serialized as JSON
}

// TraceSummaryRow is the list-view projection of a stored trace.
type TraceSummaryRow struct {
	TraceID       string    `json:"id"`
	ReceivedAt    time.Time `json:"received_at"`
	Model         string    `json:"model"`
	Endpoint      string    `json:"endpoint"`
	Stream        bool      `json:"stream"`
	Attempts      int       `json:"attempts"`
	FinalProvider string    `json:"final_provider"`
	FinalStatus   string    `json:"final_status"`
	LatencyMs     int64     `json:"latency_ms"`
}

// TraceQuery controls trace list filtering.
type TraceQuery struct {
	Model  string
	Status string // "ok"|"failed"|""
	Limit  int
	Offset int
}

// InsertTraces batch-inserts trace rows.
func (s *Store) InsertTraces(rows []TraceRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO proxy_traces
		(trace_id, received_at, model, endpoint, stream, has_tools, attempts, final_provider, final_status, latency_ms, detail_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		_, err := stmt.Exec(
			r.TraceID, r.ReceivedAt.UTC(), r.Model, r.Endpoint,
			r.Stream, r.HasTools, r.Attempts, r.FinalProvider,
			r.FinalStatus, r.LatencyMs, string(r.DetailJSON),
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TrimTraces keeps only the most recent `keep` traces (ring buffer semantics).
func (s *Store) TrimTraces(keep int) error {
	_, err := s.db.Exec(`DELETE FROM proxy_traces WHERE id <= (SELECT MAX(id) - ? FROM proxy_traces)`, keep)
	return err
}

// QueryTraces returns trace summaries matching the filter.
func (s *Store) QueryTraces(q TraceQuery) ([]TraceSummaryRow, int, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}

	// Build WHERE clause
	where := "1=1"
	var args []any
	if q.Model != "" {
		where += " AND model = ?"
		args = append(args, q.Model)
	}
	if q.Status != "" {
		where += " AND final_status = ?"
		args = append(args, q.Status)
	}

	// Count
	var total int
	countArgs := make([]any, len(args))
	copy(countArgs, args)
	err := s.db.QueryRow("SELECT COUNT(*) FROM proxy_traces WHERE "+where, countArgs...).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	// Fetch
	query := "SELECT trace_id, received_at, model, endpoint, stream, attempts, final_provider, final_status, latency_ms FROM proxy_traces WHERE " +
		where + " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, q.Limit, q.Offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []TraceSummaryRow
	for rows.Next() {
		var r TraceSummaryRow
		if err := rows.Scan(&r.TraceID, &r.ReceivedAt, &r.Model, &r.Endpoint, &r.Stream, &r.Attempts, &r.FinalProvider, &r.FinalStatus, &r.LatencyMs); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// GetTraceDetail returns the full detail JSON for a specific trace.
func (s *Store) GetTraceDetail(traceID string) (json.RawMessage, error) {
	var detail sql.NullString
	err := s.db.QueryRow("SELECT detail_json FROM proxy_traces WHERE trace_id = ?", traceID).Scan(&detail)
	if err != nil {
		return nil, err
	}
	if !detail.Valid {
		return nil, nil
	}
	return json.RawMessage(detail.String), nil
}
