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

func TestApplyProviderCheckAuthoritativeErrorClearsCurrentSnapshot(t *testing.T) {
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

	if err := st.ApplyProviderCheck("run-error", providerID, nil, true, true, "error", 0, "fetch models failed"); err != nil {
		t.Fatalf("apply authoritative error check: %v", err)
	}

	current, err := st.GetProviderResults(providerID)
	if err != nil {
		t.Fatalf("get provider results: %v", err)
	}
	if len(current) != 0 {
		t.Fatalf("current snapshot = %+v, want empty after authoritative provider failure", current)
	}

	provider, err := st.GetProviderByName("broken-provider")
	if err != nil {
		t.Fatalf("get provider by name: %v", err)
	}
	if provider.Status != "error" || provider.LastError != "fetch models failed" {
		t.Fatalf("provider state = status:%s error:%q, want error/fetch models failed", provider.Status, provider.LastError)
	}
}
