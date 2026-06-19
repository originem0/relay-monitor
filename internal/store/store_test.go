package store

import (
	"path/filepath"
	"testing"
	"time"
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

func TestApplyProviderCheckReplacesResultsVerbatimNoPreserve(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	providerID, err := st.UpsertProvider("flaky", "https://example.com/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// First check: both models correct.
	good := []CheckResultInput{
		{Model: "gpt-5.4", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 1000},
		{Model: "gpt-4o", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 800},
	}
	if err := st.ApplyProviderCheck("run-1", providerID, good, true, true, "ok", 100, ""); err != nil {
		t.Fatalf("apply good check: %v", err)
	}

	// Second check: gpt-5.4 returns 503 (transient), gpt-4o returns 401.
	// Strict-latest-snapshot policy: there is no cross-run preservation — both are
	// recorded as failed verbatim, even the transient one.
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
	if r, ok := resultMap["gpt-5.4"]; !ok {
		t.Fatal("gpt-5.4 should be in current_results")
	} else if r.Correct {
		t.Fatalf("gpt-5.4 transient failure must NOT be preserved as correct, got correct=%v", r.Correct)
	}
	if r, ok := resultMap["gpt-4o"]; !ok {
		t.Fatal("gpt-4o should be in current_results")
	} else if r.Correct {
		t.Fatal("gpt-4o 401 must replace old correct result")
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
		{"models list empty", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsTransientError(tt.err); got != tt.want {
			t.Errorf("IsTransientError(%q) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// A persistent provider failure (clearCurrent=true) wipes the snapshot so a dead
// station stops appearing as usable; a transient one (clearCurrent=false) keeps it.
func TestApplyProviderCheckExClearsCurrentOnPersistentError(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	pid, err := st.UpsertProvider("dead", "https://example.com/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	seed := func(run string) {
		if err := st.ApplyProviderCheck(run, pid, []CheckResultInput{
			{Model: "gpt-5.4", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 100},
		}, true, true, "ok", 100, ""); err != nil {
			t.Fatalf("seed %s: %v", run, err)
		}
	}

	// Persistent failure ("models list empty"): clearCurrent=true, no results.
	seed("run-1")
	if err := st.ApplyProviderCheckEx("run-2", pid, nil, time.Now(), false, false, true, true, "error", 0, "models list empty"); err != nil {
		t.Fatalf("apply clear: %v", err)
	}
	if current, _ := st.GetProviderResults(pid); len(current) != 0 {
		t.Fatalf("snapshot should be cleared on persistent failure, got %d rows", len(current))
	}

	// Transient failure: clearCurrent=false, snapshot preserved.
	seed("run-3")
	if err := st.ApplyProviderCheckEx("run-4", pid, nil, time.Now(), false, false, false, true, "error", 0, "Connection failed: timeout"); err != nil {
		t.Fatalf("apply transient: %v", err)
	}
	if current, _ := st.GetProviderResults(pid); len(current) != 1 {
		t.Fatalf("transient failure must preserve snapshot, got %d rows", len(current))
	}
}

// All of a provider's models written in one call share the provided checked_at,
// not per-row now() — the fix for spread-out within-provider timestamps.
func TestApplyProviderCheckExStampsRunTimestamp(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	pid, err := st.UpsertProvider("ts", "https://example.com/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	want := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := st.ApplyProviderCheckEx("run-1", pid, []CheckResultInput{
		{Model: "m1", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 100},
		{Model: "m2", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 200},
		{Model: "m3", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 300},
	}, want, true, true, false, true, "ok", 100, ""); err != nil {
		t.Fatalf("apply: %v", err)
	}

	current, err := st.GetProviderResults(pid)
	if err != nil {
		t.Fatalf("get results: %v", err)
	}
	if len(current) != 3 {
		t.Fatalf("want 3 rows, got %d", len(current))
	}
	const wantStr = "2026-06-01 12:00:00"
	for _, cr := range current {
		if got := cr.CheckedAt.UTC().Format("2006-01-02 15:04:05"); got != wantStr {
			t.Errorf("model %s checked_at = %s, want uniform %s", cr.Model, got, wantStr)
		}
	}
}

// Cleanup expires current_results rows older than maxStaleAge while keeping fresh ones.
func TestCleanupExpiresStaleCurrentResults(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	pid, err := st.UpsertProvider("expire", "https://example.com/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.ApplyProviderCheck("run-1", pid, []CheckResultInput{
		{Model: "fresh", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 100},
		{Model: "old", Vendor: "GPT", Status: "ok", Correct: true, Answer: "42", LatencyMs: 100},
	}, true, true, "ok", 100, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE current_results SET checked_at = datetime('now','-3 days') WHERE provider_id = ? AND model = 'old'`, pid); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if err := st.Cleanup(7); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	current, err := st.GetProviderResults(pid)
	if err != nil {
		t.Fatalf("get results: %v", err)
	}
	if len(current) != 1 || current[0].Model != "fresh" {
		t.Fatalf("stale row should be expired, kept = %+v", current)
	}
}
