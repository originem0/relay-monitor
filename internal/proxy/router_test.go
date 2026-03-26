package proxy

import (
	"testing"

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

	rt.Rebuild(results, dbProviders, memProviders, nil)

	candidates := rt.Select("gpt-5", "chat", nil, nil)
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

	rt.Rebuild(results, dbProviders, memProviders, nil)

	candidates := rt.Select("m1", "chat", nil, nil)
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1 (incorrect filtered)", len(candidates))
	}
	if candidates[0].ProviderName != "good" {
		t.Errorf("candidate = %s, want good", candidates[0].ProviderName)
	}
}

func TestRebuildFiltersDownProvider(t *testing.T) {
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

	rt.Rebuild(results, dbProviders, memProviders, nil)

	candidates := rt.Select("m1", "chat", nil, nil)
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1 (down provider filtered)", len(candidates))
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

	rt.Rebuild(results, dbProviders, memProviders, fps)

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
	if candidates := rt.Select("nonexistent", "chat", nil, nil); candidates != nil {
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

	rt.Rebuild(results, dbProviders, memProviders, nil)

	// Chat format should work
	if c := rt.Select("m1", "chat", nil, nil); len(c) != 1 {
		t.Errorf("chat format: got %d, want 1", len(c))
	}
	// Chat providers should also serve responses format (most support both; failover handles 404)
	if c := rt.Select("m1", "responses", nil, nil); len(c) == 0 {
		t.Errorf("responses format: got 0, want >0 (chat providers should be candidates for responses)")
	}
}

func TestSelectTop1Priority(t *testing.T) {
	rt := NewRoutingTable()
	results, dbProviders, memProviders := makeTestData()
	rt.Rebuild(results, dbProviders, memProviders, nil)

	// Run 100 selections and verify the best provider is chosen ~90% of the time
	bestCount := 0
	n := 1000
	for i := 0; i < n; i++ {
		candidates := rt.Select("gpt-5", "chat", nil, nil)
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

	rt.Rebuild(results, dbProviders, memProviders, nil)

	// Breaker applied at Select time, not Rebuild time
	candidates := rt.Select("m1", "chat", nil, breakers)
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
	rt.Rebuild(results, dbProviders, memProviders, nil)

	// Simulate flaky provider having 40% error rate (4 errors / 10 requests)
	stats := NewStats()
	for i := 0; i < 10; i++ {
		stats.Record(2, "m1", 100, i < 4) // first 4 are errors
	}
	// reliable has 0 errors
	for i := 0; i < 10; i++ {
		stats.Record(1, "m1", 100, false)
	}

	candidates := rt.Select("m1", "chat", stats, nil)
	if len(candidates) < 2 {
		t.Fatal("expected 2 candidates")
	}
	// Check that reliable is chosen most of the time (90% of 90% = ~81%)
	reliableFirst := 0
	for i := 0; i < 200; i++ {
		c := rt.Select("m1", "chat", stats, nil)
		if c[0].ProviderName == "reliable" {
			reliableFirst++
		}
	}
	if reliableFirst < 140 { // should be ~162/200, allow margin
		t.Errorf("reliable chosen %d/200 times, expected >140 (flaky has 40%% error rate)", reliableFirst)
	}
}
