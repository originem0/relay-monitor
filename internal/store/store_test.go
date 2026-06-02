package store

import (
	"path/filepath"
	"testing"
)

func TestApplyProviderCheckReplacesCurrentSnapshotButKeepsHistory(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	providerID, err := st.UpsertProvider("snapshot-test", "https://example.com/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert provider: %v", err)
	}

	first := []CheckResultInput{
		{Model: "keep-model", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 100},
		{Model: "drop-model", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 120},
	}
	if err := st.ApplyProviderCheck("run-1", providerID, first, true, true, "ok", 100, ""); err != nil {
		t.Fatalf("apply first provider check: %v", err)
	}

	second := []CheckResultInput{
		{Model: "keep-model", Vendor: "GPT", Status: "error", Correct: false, ErrorMsg: "boom", LatencyMs: 200},
	}
	if err := st.ApplyProviderCheck("run-2", providerID, second, true, true, "down", 0, "boom"); err != nil {
		t.Fatalf("apply second provider check: %v", err)
	}

	current, err := st.GetProviderResults(providerID)
	if err != nil {
		t.Fatalf("get provider results: %v", err)
	}
	if len(current) != 1 || current[0].Model != "keep-model" || current[0].RunID != "run-2" {
		t.Fatalf("current snapshot = %+v, want only keep-model from run-2", current)
	}

	var historyCount int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM check_results WHERE provider_id = ?`, providerID).Scan(&historyCount); err != nil {
		t.Fatalf("count history rows: %v", err)
	}
	if historyCount != 3 {
		t.Fatalf("history rows = %d, want 3", historyCount)
	}

	provider, err := st.GetProviderByName("snapshot-test")
	if err != nil {
		t.Fatalf("get provider by name: %v", err)
	}
	if provider.Status != "down" || provider.LastError != "boom" {
		t.Fatalf("provider state = status:%s error:%q, want down/boom", provider.Status, provider.LastError)
	}
}

func TestSeedCurrentResultsFromHistoryIfEmptyUsesLatestAuthoritativeRun(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	providerID, err := st.UpsertProvider("seed-test", "https://example.com/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert provider: %v", err)
	}

	if _, err := st.db.Exec(`DELETE FROM current_results`); err != nil {
		t.Fatalf("clear current_results: %v", err)
	}
	if err := st.InsertCheckRun("scheduled-old", "basic", "scheduled"); err != nil {
		t.Fatalf("insert scheduled-old run: %v", err)
	}
	if err := st.InsertCheckResult("scheduled-old", providerID, "old-model", "GPT", "ok", true, "42", 100, "", false); err != nil {
		t.Fatalf("insert scheduled-old result: %v", err)
	}
	if err := st.FinishCheckRun("scheduled-old", 1, 1, 1, 1, "done"); err != nil {
		t.Fatalf("finish scheduled-old: %v", err)
	}

	if err := st.InsertCheckRun("manual-quick", "basic", "manual"); err != nil {
		t.Fatalf("insert manual-quick run: %v", err)
	}
	if err := st.InsertCheckResult("manual-quick", providerID, "quick-model", "GPT", "ok", true, "42", 100, "", false); err != nil {
		t.Fatalf("insert manual-quick result: %v", err)
	}
	if err := st.FinishCheckRun("manual-quick", 1, 1, 1, 1, "done"); err != nil {
		t.Fatalf("finish manual-quick: %v", err)
	}

	if err := st.InsertCheckRun("running-full", "full", "manual"); err != nil {
		t.Fatalf("insert running-full run: %v", err)
	}
	if err := st.InsertCheckResult("running-full", providerID, "running-model", "GPT", "ok", true, "42", 100, "", false); err != nil {
		t.Fatalf("insert running-full result: %v", err)
	}

	if err := st.InsertCheckRun("manual-full", "full", "manual"); err != nil {
		t.Fatalf("insert manual-full run: %v", err)
	}
	if err := st.InsertCheckResult("manual-full", providerID, "fresh-model", "GPT", "ok", true, "42", 100, "", false); err != nil {
		t.Fatalf("insert manual-full result: %v", err)
	}
	if err := st.FinishCheckRun("manual-full", 1, 1, 1, 1, "done"); err != nil {
		t.Fatalf("finish manual-full: %v", err)
	}

	if err := st.seedCurrentResultsFromHistoryIfEmpty(); err != nil {
		t.Fatalf("seed current results: %v", err)
	}

	current, err := st.GetProviderResults(providerID)
	if err != nil {
		t.Fatalf("get provider results: %v", err)
	}
	if len(current) != 1 || current[0].RunID != "manual-full" || current[0].Model != "fresh-model" {
		t.Fatalf("seeded current snapshot = %+v, want manual-full/fresh-model", current)
	}
}

func TestApplyProviderCheckNonAuthoritativeDoesNotMutateCurrentState(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	providerID, err := st.UpsertProvider("sample-only", "https://example.com/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert provider: %v", err)
	}

	initial := []CheckResultInput{
		{Model: "stable-model", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 80},
	}
	if err := st.ApplyProviderCheck("run-authoritative", providerID, initial, true, true, "ok", 100, ""); err != nil {
		t.Fatalf("apply authoritative provider check: %v", err)
	}

	sampled := []CheckResultInput{
		{Model: "stable-model", Vendor: "GPT", Status: "error", Correct: false, ErrorMsg: "sample failed", LatencyMs: 300},
	}
	if err := st.ApplyProviderCheck("run-sampled", providerID, sampled, false, false, "down", 0, "sample failed"); err != nil {
		t.Fatalf("apply sampled provider check: %v", err)
	}

	current, err := st.GetProviderResults(providerID)
	if err != nil {
		t.Fatalf("get current provider results: %v", err)
	}
	if len(current) != 1 || current[0].RunID != "run-authoritative" || current[0].Status != "ok" {
		t.Fatalf("current snapshot = %+v, want authoritative snapshot to remain intact", current)
	}

	provider, err := st.GetProviderByName("sample-only")
	if err != nil {
		t.Fatalf("get provider by name: %v", err)
	}
	if provider.Status != "ok" || provider.LastError != "" {
		t.Fatalf("provider state = status:%s error:%q, want ok/empty", provider.Status, provider.LastError)
	}
}

func TestApplyProviderCheckPreservesCurrentOnProviderLevelError(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	providerID, err := st.UpsertProvider("broken-provider", "https://example.com/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert provider: %v", err)
	}

	initial := []CheckResultInput{
		{Model: "old-model", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 90},
	}
	if err := st.ApplyProviderCheck("run-good", providerID, initial, true, true, "ok", 100, ""); err != nil {
		t.Fatalf("apply initial provider check: %v", err)
	}

	// Provider-level error: replaceCurrent=false (authoritativeProviderState changed),
	// but updateProvider=true to update status/error fields.
	if err := st.ApplyProviderCheck("run-error", providerID, nil, false, true, "error", 0, "fetch models failed"); err != nil {
		t.Fatalf("apply provider error: %v", err)
	}

	// Current results should be preserved (not cleared by transient provider failure)
	current, err := st.GetProviderResults(providerID)
	if err != nil {
		t.Fatalf("get provider results: %v", err)
	}
	if len(current) != 1 || current[0].Model != "old-model" || !current[0].Correct {
		t.Fatalf("current snapshot = %+v, want old-model still correct (preserved on provider error)", current)
	}

	// But provider status should be updated
	provider, err := st.GetProviderByName("broken-provider")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if provider.Status != "error" || provider.LastError != "fetch models failed" {
		t.Fatalf("provider state = %s/%q, want error/fetch models failed", provider.Status, provider.LastError)
	}
}

func TestApplyProviderCheckPreservesCorrectOnTransientError(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	providerID, err := st.UpsertProvider("flaky", "https://example.com/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// First check: model is correct
	good := []CheckResultInput{
		{Model: "gpt-5.4", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 1000},
		{Model: "gpt-4o", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 800},
	}
	if err := st.ApplyProviderCheck("run-1", providerID, good, true, true, "ok", 100, ""); err != nil {
		t.Fatalf("apply good check: %v", err)
	}

	// Second check: gpt-5.4 returns 503 (transient), gpt-4o returns 401 (definitive)
	bad := []CheckResultInput{
		{Model: "gpt-5.4", Vendor: "GPT", Status: "error", Correct: false, ErrorMsg: "Server error (503): Service temporarily unavailable", LatencyMs: 200},
		{Model: "gpt-4o", Vendor: "GPT", Status: "error", Correct: false, ErrorMsg: "Auth failed (401): Invalid API key", LatencyMs: 100},
	}
	if err := st.ApplyProviderCheck("run-2", providerID, bad, true, true, "down", 0, ""); err != nil {
		t.Fatalf("apply bad check: %v", err)
	}

	current, err := st.GetProviderResults(providerID)
	if err != nil {
		t.Fatalf("get results: %v", err)
	}

	resultMap := make(map[string]CheckResultRow)
	for _, cr := range current {
		resultMap[cr.Model] = cr
	}

	// gpt-5.4: transient 503 → old correct result preserved
	if r, ok := resultMap["gpt-5.4"]; !ok {
		t.Fatal("gpt-5.4 should still be in current_results")
	} else if !r.Correct {
		t.Fatalf("gpt-5.4 should still be correct (transient error preserved old), got status=%s correct=%v", r.Status, r.Correct)
	}

	// gpt-4o: definitive 401 → replaced with new error
	if r, ok := resultMap["gpt-4o"]; !ok {
		t.Fatal("gpt-4o should still be in current_results")
	} else if r.Correct {
		t.Fatal("gpt-4o should NOT be correct (401 is definitive, old was replaced)")
	}
}

func TestApplyProviderCheckReplacesCorrectOnWrongAnswer(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	providerID, err := st.UpsertProvider("bad-answer", "https://example.com/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := st.ApplyProviderCheck("run-1", providerID, []CheckResultInput{
		{Model: "m1", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 500},
	}, true, true, "ok", 100, ""); err != nil {
		t.Fatal(err)
	}

	// Wrong answer (correct=false, status=ok) → definitive, must replace
	if err := st.ApplyProviderCheck("run-2", providerID, []CheckResultInput{
		{Model: "m1", Vendor: "GPT", Status: "ok", Correct: false, Answer: "wrong", LatencyMs: 500},
	}, true, true, "down", 0, ""); err != nil {
		t.Fatal(err)
	}

	current, err := st.GetProviderResults(providerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].Correct {
		t.Fatalf("wrong answer should replace old correct result: %+v", current)
	}
}

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		err  string
		want bool
	}{
		{"Server error (503): Service temporarily unavailable", true},
		{"Server error (502): Bad Gateway", true},
		{"Rate limited (429): reduce request frequency", true},
		{"Connection failed: dial tcp: lookup foo on 127.0.0.53:53", true},
		{"HTTP 522: error code: 522", true},
		{"got context deadline exceeded somewhere", true},
		{"Auth failed (401): Invalid API key", false},
		{"Permission denied (403): no access", false},
		{"HTTP 400: Stream must be set to true", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isTransientError(tt.err); got != tt.want {
			t.Errorf("isTransientError(%q) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestApplyProviderCheckExpiresStalePreservedResults(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	providerID, err := st.UpsertProvider("stale-prov", "https://example.com/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Insert a correct result with an old checked_at (3 days ago)
	if err := st.ApplyProviderCheck("run-old", providerID, []CheckResultInput{
		{Model: "m1", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 500},
	}, true, true, "ok", 100, ""); err != nil {
		t.Fatal(err)
	}
	// Manually backdate the checked_at to exceed maxStaleAge
	st.db.Exec(`UPDATE current_results SET checked_at = datetime('now', '-3 days') WHERE provider_id = ? AND model = 'm1'`, providerID)

	// Now apply a transient error — should NOT be preserved because the old result is too stale
	if err := st.ApplyProviderCheck("run-new", providerID, []CheckResultInput{
		{Model: "m1", Vendor: "GPT", Status: "error", Correct: false, ErrorMsg: "Server error (503): timeout", LatencyMs: 100},
	}, true, true, "down", 0, ""); err != nil {
		t.Fatal(err)
	}

	current, err := st.GetProviderResults(providerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 {
		t.Fatalf("expected 1 result, got %d", len(current))
	}
	if current[0].Correct {
		t.Fatal("stale correct result (>48h) should be replaced by transient error, not preserved")
	}
}
