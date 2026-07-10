package proxy

import (
	"testing"
	"time"

	"relay-monitor/internal/provider"
	"relay-monitor/internal/store"
)

func makeTestData() ([]store.CheckResultRow, []store.ProviderRow, []provider.Provider) {
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "fast", Model: "gpt-5", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 1000},
		{ProviderID: 2, ProviderName: "slow", Model: "gpt-5", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 10000},
		{ProviderID: 3, ProviderName: "mid", Model: "gpt-5", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 5000},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "fast", BaseURL: "https://fast.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 2, Name: "slow", BaseURL: "https://slow.example.com/v1", Status: "ok", Health: 80, APIFormat: "chat"},
		{ID: 3, Name: "mid", BaseURL: "https://mid.example.com/v1", Status: "ok", Health: 90, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "fast", BaseURL: "https://fast.example.com/v1", APIKey: "key1"},
		{Name: "slow", BaseURL: "https://slow.example.com/v1", APIKey: "key2"},
		{Name: "mid", BaseURL: "https://mid.example.com/v1", APIKey: "key3"},
	}
	return results, dbProviders, memProviders
}

func TestRebuildAndSelectBasic(t *testing.T) {
	rt := NewRoutingTable()
	results, dbProviders, memProviders := makeTestData()

	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	candidates := rt.Select("gpt-5", "chat", RequestRequirements{}, nil, nil)
	if len(candidates) == 0 {
		t.Fatal("Select returned no candidates")
	}
	if len(candidates) != 3 {
		t.Errorf("got %d candidates, want 3", len(candidates))
	}
}

func TestRebuildFiltersIncorrect(t *testing.T) {
	rt := NewRoutingTable()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "good", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
		{ProviderID: 2, ProviderName: "bad", Model: "m1", Vendor: "v", Status: "ok", Correct: false, LatencyMs: 500},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "good", BaseURL: "https://good.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 2, Name: "bad", BaseURL: "https://bad.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "good", APIKey: "k1"},
		{Name: "bad", APIKey: "k2"},
	}

	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	candidates := rt.Select("m1", "chat", RequestRequirements{}, nil, nil)
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1 (incorrect filtered)", len(candidates))
	}
	if candidates[0].ProviderName != "good" {
		t.Errorf("candidate = %s, want good", candidates[0].ProviderName)
	}
}

func TestRebuildDownProviderIncludedButPenalized(t *testing.T) {
	rt := NewRoutingTable()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "up", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
		{ProviderID: 2, ProviderName: "down", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 500},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "up", BaseURL: "https://up.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 2, Name: "down", BaseURL: "https://down.example.com/v1", Status: "down", Health: 0, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "up", APIKey: "k1"},
		{Name: "down", APIKey: "k2"},
	}

	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	// Down provider's correct model should still be routable (not hard-filtered)
	candidates := rt.Select("m1", "chat", RequestRequirements{}, nil, nil)
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2 (down provider penalized but included)", len(candidates))
	}

	// "up" should have a higher raw score (checked via DebugScores, which doesn't do weighted random)
	scores := rt.DebugScores("m1")
	if len(scores) != 2 {
		t.Fatalf("debug scores: got %d, want 2", len(scores))
	}
	// DebugScores returns sorted by score desc
	if scores[0].ProviderName != "up" {
		t.Errorf("highest scoring provider = %s (%.3f), want up", scores[0].ProviderName, scores[0].Score)
	}
	if scores[0].Score <= scores[1].Score {
		t.Errorf("up score (%.3f) should be > down score (%.3f)", scores[0].Score, scores[1].Score)
	}
}

func TestRebuildFingerprintPenalty(t *testing.T) {
	rt := NewRoutingTable()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "genuine", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 5000},
		{ProviderID: 2, ProviderName: "fake", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 5000},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "genuine", BaseURL: "https://g.example.com/v1", Status: "ok", Health: 80, APIFormat: "chat"},
		{ID: 2, Name: "fake", BaseURL: "https://f.example.com/v1", Status: "ok", Health: 80, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "genuine", APIKey: "k1"},
		{Name: "fake", APIKey: "k2"},
	}
	fps := map[[2]string]FingerprintScore{
		{"genuine", "m1"}: {TotalScore: 9, ExpectedMin: 9, Verdict: "GENUINE"},
		{"fake", "m1"}:    {TotalScore: 2, ExpectedMin: 9, Verdict: "LIKELY FAKE"},
	}

	rt.Rebuild(results, dbProviders, memProviders, fps, nil)

	// Check raw scores: genuine should have higher score than fake
	rt.mu.RLock()
	providers := rt.models["m1"]
	rt.mu.RUnlock()

	if len(providers) < 2 {
		t.Fatal("expected 2 providers")
	}
	// Sorted by score desc, so first should be genuine
	if providers[0].ProviderName != "genuine" {
		t.Errorf("highest score provider = %s, want genuine", providers[0].ProviderName)
	}
	if providers[0].Score <= providers[1].Score {
		t.Errorf("genuine score (%.3f) should be > fake score (%.3f)", providers[0].Score, providers[1].Score)
	}
}

func TestSelectUnknownModel(t *testing.T) {
	rt := NewRoutingTable()
	if candidates := rt.Select("nonexistent", "chat", RequestRequirements{}, nil, nil); candidates != nil {
		t.Errorf("Select for unknown model should return nil, got %d candidates", len(candidates))
	}
}

func TestSelectFormatFiltering(t *testing.T) {
	rt := NewRoutingTable()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "chat-only", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "chat-only", BaseURL: "https://c.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "chat-only", APIKey: "k1"},
	}

	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	// Chat format should work
	if c := rt.Select("m1", "chat", RequestRequirements{}, nil, nil); len(c) != 1 {
		t.Errorf("chat format: got %d, want 1", len(c))
	}
	// Chat providers should also serve responses format (most support both; failover handles 404)
	if c := rt.Select("m1", "responses", RequestRequirements{}, nil, nil); len(c) == 0 {
		t.Errorf("responses format: got 0, want >0 (chat providers should be candidates for responses)")
	}
}

func TestSelectTop1Priority(t *testing.T) {
	rt := NewRoutingTable()
	results, dbProviders, memProviders := makeTestData()
	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	// Run 100 selections and verify the best provider is chosen ~90% of the time
	bestCount := 0
	n := 1000
	for i := 0; i < n; i++ {
		candidates := rt.Select("gpt-5", "chat", RequestRequirements{}, nil, nil)
		if candidates[0].ProviderName == "fast" {
			bestCount++
		}
	}

	ratio := float64(bestCount) / float64(n)
	// Should be ~90% ± some variance. Allow 80%-97% range.
	if ratio < 0.80 || ratio > 0.97 {
		t.Errorf("best provider selected %.1f%% of time, expected ~90%%", ratio*100)
	}
}

func TestRebuildBreakerOpen(t *testing.T) {
	rt := NewRoutingTable()
	breakers := NewBreakers()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "a", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
		{ProviderID: 2, ProviderName: "b", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "a", BaseURL: "https://a.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 2, Name: "b", BaseURL: "https://b.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "a", APIKey: "k1"},
		{Name: "b", APIKey: "k2"},
	}

	// Open breaker for provider 1
	breakers.ForceState(1, "m1", BreakerOpen)

	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	// Breaker applied at Select time, not Rebuild time
	candidates := rt.Select("m1", "chat", RequestRequirements{}, nil, breakers)
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1 (open breaker filtered)", len(candidates))
	}
	if candidates[0].ProviderName != "b" {
		t.Errorf("candidate = %s, want b", candidates[0].ProviderName)
	}
}

func TestSelectErrorRatePenalty(t *testing.T) {
	rt := NewRoutingTable()
	// Two providers with identical static scores (same latency, health)
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "reliable", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 2000},
		{ProviderID: 2, ProviderName: "flaky", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 2000},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "reliable", BaseURL: "https://r.example.com/v1", Status: "ok", Health: 90, APIFormat: "chat"},
		{ID: 2, Name: "flaky", BaseURL: "https://f.example.com/v1", Status: "ok", Health: 90, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "reliable", APIKey: "k1"},
		{Name: "flaky", APIKey: "k2"},
	}
	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	// Simulate flaky provider having 40% error rate (4 errors / 10 requests)
	stats := NewStats()
	for i := 0; i < 10; i++ {
		stats.Record(2, "m1", 100, i < 4) // first 4 are errors
	}
	// reliable has 0 errors
	for i := 0; i < 10; i++ {
		stats.Record(1, "m1", 100, false)
	}

	candidates := rt.Select("m1", "chat", RequestRequirements{}, stats, nil)
	if len(candidates) < 2 {
		t.Fatal("expected 2 candidates")
	}
	// Check that reliable is chosen most of the time (90% of 90% = ~81%)
	reliableFirst := 0
	for i := 0; i < 200; i++ {
		c := rt.Select("m1", "chat", RequestRequirements{}, stats, nil)
		if c[0].ProviderName == "reliable" {
			reliableFirst++
		}
	}
	if reliableFirst < 140 { // should be ~162/200, allow margin
		t.Errorf("reliable chosen %d/200 times, expected >140 (flaky has 40%% error rate)", reliableFirst)
	}
}

func TestSelectFiltersResponsesToolCapability(t *testing.T) {
	rt := NewRoutingTable()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "bad-responses", Model: "gpt-5.4", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 500},
		{ProviderID: 2, ProviderName: "good-responses", Model: "gpt-5.4", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 900},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "bad-responses", BaseURL: "https://bad.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 2, Name: "good-responses", BaseURL: "https://good.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "bad-responses", APIKey: "k1"},
		{Name: "good-responses", APIKey: "k2"},
	}
	trueVal := true
	falseVal := false
	caps := map[int64]map[string]store.CapabilityRow{
		1: {
			"gpt-5.4": {
				ProviderID:         1,
				Model:              "gpt-5.4",
				ResponsesBasic:     &trueVal,
				ResponsesToolUse:   &falseVal,
				ResponsesStreaming: &trueVal,
			},
		},
		2: {
			"gpt-5.4": {
				ProviderID:         2,
				Model:              "gpt-5.4",
				ResponsesBasic:     &trueVal,
				ResponsesToolUse:   &trueVal,
				ResponsesStreaming: &trueVal,
			},
		},
	}

	rt.Rebuild(results, dbProviders, memProviders, nil, caps)

	candidates := rt.Select("gpt-5.4", "responses", RequestRequirements{NeedsTools: true}, nil, nil)
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	if candidates[0].ProviderName != "good-responses" {
		t.Fatalf("selected %s, want good-responses", candidates[0].ProviderName)
	}
}

func TestApplyRequestRequirementsAllowsBasicResponsesWithoutTools(t *testing.T) {
	base := true
	noTools := false
	sp := ScoredProvider{
		APIFormat:        "responses",
		Score:            1,
		ResponsesBasic:   &base,
		ResponsesToolUse: &noTools,
	}

	_, rank, ok := applyRequestRequirements(sp, "responses", RequestRequirements{})
	if !ok {
		t.Fatal("provider should remain eligible for plain responses requests")
	}
	if rank != 0 {
		t.Fatalf("rank = %d, want 0 for a native known-good responses provider", rank)
	}
}

func TestApplyRequestRequirementsDoesNotPenalizeProvenChatResponsesSupport(t *testing.T) {
	basic := true
	sp := ScoredProvider{
		APIFormat:      "chat",
		Score:          1,
		ResponsesBasic: &basic,
	}

	_, rank, ok := applyRequestRequirements(sp, "responses", RequestRequirements{})
	if !ok {
		t.Fatal("provider with proven /responses support should remain eligible")
	}
	if rank != 0 {
		t.Fatalf("rank = %d, want 0 for a provider with proven /responses support", rank)
	}
}

func TestApplyRequestRequirementsRejectsKnownBadResponsesSupport(t *testing.T) {
	basic := false
	sp := ScoredProvider{
		APIFormat:      "chat",
		Score:          1,
		ResponsesBasic: &basic,
	}

	if _, _, ok := applyRequestRequirements(sp, "responses", RequestRequirements{}); ok {
		t.Fatal("known non-responses provider should be filtered for /responses")
	}
}

func TestApplyRequestRequirementsRejectsRequiredToolCallWithoutCapability(t *testing.T) {
	basic := true
	noTools := false
	sp := ScoredProvider{
		APIFormat:        "responses",
		Score:            1,
		ResponsesBasic:   &basic,
		ResponsesToolUse: &noTools,
	}

	if _, _, ok := applyRequestRequirements(sp, "responses", RequestRequirements{
		NeedsTools:    true,
		NeedsToolCall: true,
	}); ok {
		t.Fatal("provider lacking tool capability should be filtered when tool call is required")
	}
}

func TestApplyRequestRequirementsPenalizesUnknownRequiredToolCall(t *testing.T) {
	basic := true
	sp := ScoredProvider{
		APIFormat:      "responses",
		Score:          1,
		ResponsesBasic: &basic,
	}

	_, rank, ok := applyRequestRequirements(sp, "responses", RequestRequirements{
		NeedsTools:    true,
		NeedsToolCall: true,
	})
	if !ok {
		t.Fatal("unknown tool capability should remain as degraded fallback, not hard-filter")
	}
	if rank == 0 {
		t.Fatal("unknown required tool capability should be ranked below known-good providers")
	}
}

func TestSelectAllowsResponsesBasicWithoutToolUseWhenNoToolsRequested(t *testing.T) {
	rt := NewRoutingTable()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "basic-only", Model: "gpt-5.4", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 500},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "basic-only", BaseURL: "https://basic.example.com/v1", Status: "ok", Health: 100, APIFormat: "responses"},
	}
	memProviders := []provider.Provider{
		{Name: "basic-only", APIKey: "k1"},
	}
	trueVal := true
	falseVal := false
	caps := map[int64]map[string]store.CapabilityRow{
		1: {
			"gpt-5.4": {
				ProviderID:       1,
				Model:            "gpt-5.4",
				ResponsesBasic:   &trueVal,
				ResponsesToolUse: &falseVal,
			},
		},
	}

	rt.Rebuild(results, dbProviders, memProviders, nil, caps)

	candidates := rt.Select("gpt-5.4", "responses", RequestRequirements{}, nil, nil)
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	if candidates[0].ProviderName != "basic-only" {
		t.Fatalf("selected %s, want basic-only", candidates[0].ProviderName)
	}
}

func TestSelectPrefersKnownResponsesSupportOverUnknownFallback(t *testing.T) {
	rt := NewRoutingTable()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "known", Model: "gpt-5.4", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 900},
		{ProviderID: 2, ProviderName: "unknown", Model: "gpt-5.4", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 100},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "known", BaseURL: "https://known.example.com/v1", Status: "ok", Health: 100, APIFormat: "responses"},
		{ID: 2, Name: "unknown", BaseURL: "https://unknown.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "known", APIKey: "k1"},
		{Name: "unknown", APIKey: "k2"},
	}
	trueVal := true
	caps := map[int64]map[string]store.CapabilityRow{
		1: {
			"gpt-5.4": {
				ProviderID:     1,
				Model:          "gpt-5.4",
				ResponsesBasic: &trueVal,
			},
		},
	}

	rt.Rebuild(results, dbProviders, memProviders, nil, caps)

	candidates := rt.Select("gpt-5.4", "responses", RequestRequirements{}, nil, nil)
	if len(candidates) < 2 {
		t.Fatalf("got %d candidates, want at least 2", len(candidates))
	}
	if candidates[0].ProviderName != "known" {
		t.Fatalf("selected %s, want known", candidates[0].ProviderName)
	}
}

func TestDebugSelectionUsesRequestAwareOrdering(t *testing.T) {
	rt := NewRoutingTable()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "fast-but-unknown", Model: "gpt-5.4", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 100},
		{ProviderID: 2, ProviderName: "known-good", Model: "gpt-5.4", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 800},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "fast-but-unknown", BaseURL: "https://unknown.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 2, Name: "known-good", BaseURL: "https://good.example.com/v1", Status: "ok", Health: 100, APIFormat: "responses"},
	}
	memProviders := []provider.Provider{
		{Name: "fast-but-unknown", APIKey: "k1"},
		{Name: "known-good", APIKey: "k2"},
	}
	trueVal := true
	caps := map[int64]map[string]store.CapabilityRow{
		2: {
			"gpt-5.4": {
				ProviderID:         2,
				Model:              "gpt-5.4",
				ResponsesBasic:     &trueVal,
				ResponsesToolUse:   &trueVal,
				ResponsesStreaming: &trueVal,
			},
		},
	}

	rt.Rebuild(results, dbProviders, memProviders, nil, caps)

	debug := rt.DebugSelection("gpt-5.4", "responses", RequestRequirements{
		NeedsStreaming: true,
		NeedsTools:     true,
		NeedsToolCall:  true,
	}, nil, nil)
	if len(debug) != 2 {
		t.Fatalf("got %d candidates, want 2", len(debug))
	}
	if debug[0].ProviderName != "known-good" {
		t.Fatalf("first debug candidate = %s, want known-good", debug[0].ProviderName)
	}
	if debug[0].MatchRank != 0 {
		t.Fatalf("known-good match rank = %d, want 0", debug[0].MatchRank)
	}
	if debug[1].MatchRank <= debug[0].MatchRank {
		t.Fatalf("unknown fallback should rank worse: %+v", debug)
	}
}

func TestSelectWithExplanation_BreakerFilteredShown(t *testing.T) {
	rt := NewRoutingTable()
	breakers := NewBreakers()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "healthy", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
		{ProviderID: 2, ProviderName: "broken", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "healthy", BaseURL: "https://h.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 2, Name: "broken", BaseURL: "https://b.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "healthy", APIKey: "k1"},
		{Name: "broken", APIKey: "k2"},
	}

	breakers.ForceState(2, "m1", BreakerOpen)
	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	candidates, candidateEntries, filtered, _ := rt.SelectWithExplanation("m1", "chat", RequestRequirements{}, nil, breakers)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if len(candidateEntries) != 1 {
		t.Fatalf("expected 1 candidate entry, got %d", len(candidateEntries))
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered entry, got %d", len(filtered))
	}
	if filtered[0].ProviderName != "broken" {
		t.Errorf("expected filtered provider = broken, got %s", filtered[0].ProviderName)
	}
	if filtered[0].ReasonCode != "breaker_open" {
		t.Errorf("expected reason = breaker_open, got %s", filtered[0].ReasonCode)
	}
}

func TestSelectWithExplanation_CapabilityFilteredShown(t *testing.T) {
	rt := NewRoutingTable()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "has-tools", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
		{ProviderID: 2, ProviderName: "no-tools", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "has-tools", BaseURL: "https://h.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 2, Name: "no-tools", BaseURL: "https://n.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "has-tools", APIKey: "k1"},
		{Name: "no-tools", APIKey: "k2"},
	}
	trueVal := true
	falseVal := false
	caps := map[int64]map[string]store.CapabilityRow{
		1: {"m1": {Streaming: &trueVal, ToolUse: &trueVal}},
		2: {"m1": {Streaming: &trueVal, ToolUse: &falseVal}},
	}

	rt.Rebuild(results, dbProviders, memProviders, nil, caps)

	_, _, filtered, _ := rt.SelectWithExplanation("m1", "chat", RequestRequirements{NeedsTools: true}, nil, nil)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered entry, got %d", len(filtered))
	}
	if filtered[0].ReasonCode != "capability_unsupported" {
		t.Errorf("expected reason = capability_unsupported, got %s", filtered[0].ReasonCode)
	}
}

func TestRebuildExcludesStaleResults(t *testing.T) {
	rt := NewRoutingTable()
	now := time.Now()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "fresh", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000, CheckedAt: now.Add(-1 * time.Hour)},
		{ProviderID: 2, ProviderName: "zombie", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000, CheckedAt: now.Add(-49 * time.Hour)},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "fresh", BaseURL: "https://f.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 2, Name: "zombie", BaseURL: "https://z.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "fresh", APIKey: "k1"},
		{Name: "zombie", APIKey: "k2"},
	}
	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	candidates := rt.Select("m1", "chat", RequestRequirements{}, nil, nil)
	if len(candidates) != 1 {
		t.Fatalf("expected only fresh (zombie >48h excluded), got %d", len(candidates))
	}
	if candidates[0].ProviderName != "fresh" {
		t.Errorf("routable = %s, want fresh (stale zombie excluded)", candidates[0].ProviderName)
	}
}

func TestSelectWithExplanation_ForcedProbeWhenAllOpen(t *testing.T) {
	rt := NewRoutingTable()
	breakers := NewBreakers()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "fast", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 500},
		{ProviderID: 2, ProviderName: "slow", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 8000},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "fast", BaseURL: "https://f.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 2, Name: "slow", BaseURL: "https://s.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "fast", APIKey: "k1"},
		{Name: "slow", APIKey: "k2"},
	}
	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	// Every candidate's breaker is open → no normal candidate survives.
	breakers.ForceState(1, "m1", BreakerOpen)
	breakers.ForceState(2, "m1", BreakerOpen)

	candidates, _, filtered, forcedProbe := rt.SelectWithExplanation("m1", "chat", RequestRequirements{}, nil, breakers)
	if !forcedProbe {
		t.Fatal("expected forcedProbe=true when every candidate is open")
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 forced-probe candidate, got %d", len(candidates))
	}
	if candidates[0].ProviderName != "fast" {
		t.Errorf("forced-probe target = %s, want fast (highest score)", candidates[0].ProviderName)
	}
	if len(filtered) != 2 {
		t.Errorf("expected both providers reported in filtered, got %d", len(filtered))
	}
}

func TestRoutePausedExcludedFromRouting(t *testing.T) {
	rt := NewRoutingTable()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "active", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
		{ProviderID: 2, ProviderName: "paused", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
		{ProviderID: 3, ProviderName: "disabled", Model: "m1", Vendor: "v", Status: "ok", Correct: true, LatencyMs: 1000},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "active", BaseURL: "https://a.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 2, Name: "paused", BaseURL: "https://p.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
		{ID: 3, Name: "disabled", BaseURL: "https://d.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"},
	}
	memProviders := []provider.Provider{
		{Name: "active", APIKey: "k1"},
		{Name: "paused", APIKey: "k2", RoutePaused: true},
		{Name: "disabled", APIKey: "k3", Disabled: true},
	}
	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	candidates := rt.Select("m1", "chat", RequestRequirements{}, nil, nil)
	if len(candidates) != 1 {
		t.Fatalf("expected only 'active' routable, got %d", len(candidates))
	}
	if candidates[0].ProviderName != "active" {
		t.Errorf("routable = %s, want active (paused + disabled both excluded)", candidates[0].ProviderName)
	}
}

func TestCLIAutoRoutingKeepsCodexAndExcludesClaude(t *testing.T) {
	rt := NewRoutingTable()
	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "cli", Model: "gpt-5.6-sol", Vendor: "GPT", Status: "ok", Correct: true, LatencyMs: 100},
		{ProviderID: 1, ProviderName: "cli", Model: "sonnet5", Vendor: "Claude", Status: "ok", Correct: true, LatencyMs: 100},
	}
	dbProviders := []store.ProviderRow{{ID: 1, Name: "cli", BaseURL: "https://cli.example.com/v1", Status: "ok", Health: 100, APIFormat: "chat"}}
	memProviders := []provider.Provider{{Name: "cli", APIKey: "k", ClientMode: provider.ClientModeAuto}}

	rt.Rebuild(results, dbProviders, memProviders, nil, nil)
	codex := rt.Select("gpt-5.6-sol", "responses", RequestRequirements{}, nil, nil)
	if len(codex) != 1 || codex[0].APIFormat != "responses" || codex[0].ClientProfile.Mode != provider.ClientModeCodex {
		t.Fatalf("Codex candidates = %#v", codex)
	}
	if claude := rt.Select("sonnet5", "chat", RequestRequirements{}, nil, nil); len(claude) != 0 {
		t.Fatalf("Claude Messages provider leaked into OpenAI proxy: %#v", claude)
	}
}

func TestScoreBreakdownPopulated(t *testing.T) {
	rt := NewRoutingTable()
	results, dbProviders, memProviders := makeTestData()
	rt.Rebuild(results, dbProviders, memProviders, nil, nil)

	rt.mu.RLock()
	providers := rt.models["gpt-5"]
	rt.mu.RUnlock()

	if len(providers) == 0 {
		t.Fatal("no providers in routing table")
	}

	sp := providers[0]
	bd := sp.Breakdown
	if bd.LatencyScore == 0 && bd.HealthScore == 0 {
		t.Error("breakdown should have non-zero values")
	}
	if bd.BaseScore == 0 {
		t.Error("base score should be non-zero")
	}
	if bd.PriorityMul == 0 {
		t.Error("priority multiplier should be non-zero (default 1.0)")
	}

	// Verify base score is correctly computed: 0.4*latency + 0.3*health + 0.3*fp
	expected := 0.4*bd.LatencyScore + 0.3*bd.HealthScore + 0.3*bd.FingerprintScore
	if abs(bd.BaseScore-expected) > 0.001 {
		t.Errorf("base score %.3f doesn't match formula (expected %.3f)", bd.BaseScore, expected)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
