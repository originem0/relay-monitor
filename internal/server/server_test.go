package server

import (
	"database/sql"
	"testing"
	"time"

	"relay-monitor/internal/checker"
	"relay-monitor/internal/provider"
	"relay-monitor/internal/store"
)

func boolPtr(v bool) *bool {
	return &v
}

func TestSelectCapabilityProbeTargetsPrioritizesResponsesHotModels(t *testing.T) {
	prov := provider.Provider{Name: "relay", APIFormat: "chat"}
	results := []checker.TestResult{
		{Model: "gpt-5.2-codex", Correct: true, LatencyMs: 1200},
		{Model: "gpt-5.3-codex", Correct: true, LatencyMs: 1100},
		{Model: "claude-3.7-sonnet", Correct: true, LatencyMs: 800},
		{Model: "gpt-5.4", Correct: true, LatencyMs: 1500},
	}

	targets := selectCapabilityProbeTargets(prov, results, nil, 3)
	if len(targets) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(targets))
	}
	if targets[0].Result.Model != "gpt-5.4" {
		t.Fatalf("expected gpt-5.4 to be first, got %s", targets[0].Result.Model)
	}
	for _, target := range targets {
		if target.Result.Model == "claude-3.7-sonnet" {
			t.Fatalf("chat-only model should not outrank missing responses targets: %+v", targets)
		}
	}
}

func TestSelectCapabilityProbeTargetsSkipsFreshCapabilities(t *testing.T) {
	now := time.Now()
	prov := provider.Provider{Name: "relay", APIFormat: "chat"}
	results := []checker.TestResult{
		{Model: "claude-3.7-sonnet", Correct: true, LatencyMs: 700},
		{Model: "o4-mini", Correct: true, LatencyMs: 900},
		{Model: "gpt-5.4", Correct: true, LatencyMs: 1000},
	}
	existing := []store.CapabilityRow{
		{
			Model:              "gpt-5.4",
			Streaming:          boolPtr(true),
			ToolUse:            boolPtr(true),
			ChatTestedAt:       sql.NullTime{Time: now, Valid: true},
			ResponsesBasic:     boolPtr(true),
			ResponsesStreaming: boolPtr(true),
			ResponsesToolUse:   boolPtr(true),
			ResponsesTestedAt:  sql.NullTime{Time: now, Valid: true},
		},
		{
			Model:        "o4-mini",
			Streaming:    boolPtr(true),
			ToolUse:      boolPtr(true),
			ChatTestedAt: sql.NullTime{Time: now, Valid: true},
		},
	}

	targets := selectCapabilityProbeTargets(prov, results, existing, 1)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].Result.Model != "o4-mini" {
		t.Fatalf("expected o4-mini to be selected, got %s", targets[0].Result.Model)
	}
	if !targets[0].NeedResponses {
		t.Fatalf("expected o4-mini to require responses probe")
	}
}

func TestSummarizeProviderResultsCountsOKSeparately(t *testing.T) {
	summary := summarizeProviderResults([]checker.TestResult{
		{Model: "m1", Status: "ok", Correct: true},
		{Model: "m2", Status: "ok", Correct: false},
		{Model: "m3", Status: "error", Correct: false},
	}, "")

	if summary.Models != 3 {
		t.Fatalf("models = %d, want 3", summary.Models)
	}
	if summary.OK != 2 {
		t.Fatalf("ok = %d, want 2", summary.OK)
	}
	if summary.Correct != 1 {
		t.Fatalf("correct = %d, want 1", summary.Correct)
	}
	if summary.Status != "down" {
		t.Fatalf("status = %s, want down", summary.Status)
	}
}

func TestAuthoritativeProviderStateRejectsPartialFullRun(t *testing.T) {
	replaceCurrent, updateProvider := authoritativeProviderState(checker.ModeFull, &checker.ProviderResult{
		Provider:    "relay",
		ModelsFound: 3,
		Results: []checker.TestResult{
			{Model: "m1", Status: "ok", Correct: true},
			{Model: "m2", Status: "ok", Correct: true},
		},
	})
	if replaceCurrent || updateProvider {
		t.Fatalf("partial full run should not mutate current state: replace=%v update=%v", replaceCurrent, updateProvider)
	}
}

func TestAuthoritativeProviderStateTreatsProviderLevelFailureAsAuthoritative(t *testing.T) {
	replaceCurrent, updateProvider := authoritativeProviderState(checker.ModeFull, &checker.ProviderResult{
		Provider: "relay",
		Error:    "fetch models failed",
	})
	if !replaceCurrent || !updateProvider {
		t.Fatalf("provider-level full failure should clear current snapshot and update provider state: replace=%v update=%v", replaceCurrent, updateProvider)
	}
}
