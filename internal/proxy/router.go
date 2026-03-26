package proxy

import (
	"math/rand"
	"sort"
	"sync"

	"relay-monitor/internal/provider"
	"relay-monitor/internal/store"
)

// ScoredProvider is a routing candidate with a computed score.
type ScoredProvider struct {
	ProviderID   int64
	ProviderName string
	BaseURL      string
	APIKey       string
	APIFormat    string // "chat" or "responses"
	Vendor       string
	Score        float64
	LatencyMs    int64
}

// RoutingTable maps model names to scored provider lists, rebuilt after each check.
type RoutingTable struct {
	mu     sync.RWMutex
	models map[string][]ScoredProvider // model → providers sorted by score desc
}

func NewRoutingTable() *RoutingTable {
	return &RoutingTable{
		models: make(map[string][]ScoredProvider),
	}
}

// FingerprintScore holds a fingerprint score for a (provider, model) pair.
type FingerprintScore struct {
	TotalScore  int
	ExpectedMin int
	Verdict     string
}

// Rebuild reconstructs the routing table from the latest check results and provider list.
// fingerprints maps (providerName, model) to fingerprint scores for quality-based routing.
func (rt *RoutingTable) Rebuild(results []store.CheckResultRow, dbProviders []store.ProviderRow, memProviders []provider.Provider, fingerprints map[[2]string]FingerprintScore) {
	// Build lookup maps
	keyByName := make(map[string]string)     // provider name → API key
	priorityByName := make(map[string]float64) // provider name → priority multiplier
	for _, p := range memProviders {
		keyByName[p.Name] = p.APIKey
		priorityByName[p.Name] = p.Priority
	}
	healthByID := make(map[int64]float64)
	statusByID := make(map[int64]string)
	formatByID := make(map[int64]string)
	for _, p := range dbProviders {
		healthByID[p.ID] = p.Health
		statusByID[p.ID] = p.Status
		formatByID[p.ID] = p.APIFormat
	}

	newModels := make(map[string][]ScoredProvider)

	for _, r := range results {
		// Hard filter: must be correct, provider must be ok or degraded
		if !r.Correct || r.Status != "ok" {
			continue
		}
		pStatus := statusByID[r.ProviderID]
		if pStatus != "ok" && pStatus != "degraded" {
			continue
		}

		apiKey, ok := keyByName[r.ProviderName]
		if !ok || apiKey == "" {
			continue
		}

		// Score: 0.4 × latency + 0.3 × health + 0.3 × fingerprint
		latencyScore := 1.0 - clamp(float64(r.LatencyMs)/30000.0, 0, 1)
		healthScore := healthByID[r.ProviderID] / 100.0
		fpScore := 0.5 // default when no fingerprint data
		fpKey := [2]string{r.ProviderName, r.Model}
		if fp, ok := fingerprints[fpKey]; ok {
			fpScore = clamp(float64(fp.TotalScore)/10.0, 0, 1)
			// Penalize suspected/fake heavily
			if fp.Verdict == "LIKELY FAKE" || fp.Verdict == "FAIL" {
				fpScore = 0
			} else if fp.Verdict == "SUSPECTED" || fp.Verdict == "MISMATCH" {
				fpScore *= 0.3
			}
		}
		score := 0.4*latencyScore + 0.3*healthScore + 0.3*fpScore

		// Manual priority boost (default 1.0, set >1 to prioritize)
		if pri := priorityByName[r.ProviderName]; pri > 0 {
			score *= pri
		}

		sp := ScoredProvider{
			ProviderID:   r.ProviderID,
			ProviderName: r.ProviderName,
			BaseURL:      r.ProviderName, // placeholder, set below
			APIKey:       apiKey,
			APIFormat:    formatByID[r.ProviderID],
			Vendor:       r.Vendor,
			Score:        score,
			LatencyMs:    r.LatencyMs,
		}

		// Get actual BaseURL from DB providers
		for _, dp := range dbProviders {
			if dp.ID == r.ProviderID {
				sp.BaseURL = dp.BaseURL
				break
			}
		}

		newModels[r.Model] = append(newModels[r.Model], sp)
	}

	// Sort each model's providers by score descending
	for model := range newModels {
		sort.Slice(newModels[model], func(i, j int) bool {
			return newModels[model][i].Score > newModels[model][j].Score
		})
	}

	rt.mu.Lock()
	rt.models = newModels
	rt.mu.Unlock()
}

// Select returns an ordered list of providers for the given model and API format.
// Breaker state and live error rates are applied dynamically at selection time.
func (rt *RoutingTable) Select(model, apiFormat string, stats *Stats, breakers *Breakers) []ScoredProvider {
	rt.mu.RLock()
	all := rt.models[model]
	rt.mu.RUnlock()

	if len(all) == 0 {
		return nil
	}

	// Filter by API format, apply breaker + error-rate penalties
	var candidates []ScoredProvider
	for _, sp := range all {
		if !matchFormat(sp.APIFormat, apiFormat) {
			continue
		}
		adjusted := sp
		// Dynamic circuit breaker check
		if breakers != nil {
			switch breakers.GetState(sp.ProviderID, model) {
			case BreakerOpen:
				continue // skip entirely
			case BreakerHalfOpen:
				adjusted.Score *= 0.3
			case BreakerSuspect:
				adjusted.Score *= 0.5
			}
		}
		if stats != nil {
			adjusted.Score = applyErrorPenalty(adjusted, model, stats)
		}
		candidates = append(candidates, adjusted)
	}
	if len(candidates) == 0 {
		return nil
	}

	// Re-sort by adjusted score
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	// Take top 3 for weighted random selection
	top := candidates
	if len(top) > 3 {
		top = top[:3]
	}

	primary := weightedSelect(top)

	// Build result: primary first, then others in score order
	result := []ScoredProvider{primary}
	for _, sp := range candidates {
		if sp.ProviderID != primary.ProviderID {
			result = append(result, sp)
		}
	}
	return result
}

// applyErrorPenalty adjusts a provider's score based on live proxy error rate.
// Minimum 5 requests before penalty kicks in (avoid penalizing on noise).
func applyErrorPenalty(sp ScoredProvider, model string, stats *Stats) float64 {
	v := stats.get(sp.ProviderID, model)
	reqs := v.Requests.Load()
	if reqs < 5 {
		return sp.Score
	}
	errs := v.Errors.Load()
	errorRate := float64(errs) / float64(reqs)
	// Linear penalty: 10% error rate → score × 0.9, 50% → score × 0.5
	return sp.Score * (1.0 - errorRate)
}

// Models returns a deduplicated list of all model names in the routing table.
func (rt *RoutingTable) Models() []string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	models := make([]string, 0, len(rt.models))
	for m := range rt.models {
		models = append(models, m)
	}
	sort.Strings(models)
	return models
}

// Ready returns true if the routing table has at least one model.
func (rt *RoutingTable) Ready() bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return len(rt.models) > 0
}

// ModelProviderCount returns the number of providers serving each model.
func (rt *RoutingTable) ModelProviderCount() map[string]int {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	counts := make(map[string]int, len(rt.models))
	for m, providers := range rt.models {
		counts[m] = len(providers)
	}
	return counts
}

// ProviderModelCount returns how many models each provider is routing.
func (rt *RoutingTable) ProviderModelCount() map[int64]int {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	counts := make(map[int64]int)
	for _, providers := range rt.models {
		seen := make(map[int64]bool)
		for _, sp := range providers {
			if !seen[sp.ProviderID] {
				counts[sp.ProviderID]++
				seen[sp.ProviderID] = true
			}
		}
	}
	return counts
}

// ModelInfo describes a model available in the routing table.
type ModelInfo struct {
	Model        string
	Vendor       string
	Providers    int
	BestProvider string
	BestLatency  int64
}

// ModelCatalog returns detailed info for every model in the routing table.
func (rt *RoutingTable) ModelCatalog() []ModelInfo {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	catalog := make([]ModelInfo, 0, len(rt.models))
	for model, providers := range rt.models {
		if len(providers) == 0 {
			continue
		}
		best := providers[0] // already sorted by score desc
		catalog = append(catalog, ModelInfo{
			Model:        model,
			Vendor:       best.Vendor,
			Providers:    len(providers),
			BestProvider: best.ProviderName,
			BestLatency:  best.LatencyMs,
		})
	}
	sort.Slice(catalog, func(i, j int) bool {
		if catalog[i].Vendor != catalog[j].Vendor {
			return catalog[i].Vendor < catalog[j].Vendor
		}
		return catalog[i].Model < catalog[j].Model
	})
	return catalog
}

func matchFormat(providerFormat, requestFormat string) bool {
	if providerFormat == "" {
		providerFormat = "chat"
	}
	// Only reject: chat request to a responses-only provider (no /chat/completions endpoint).
	// All other combos are allowed — most providers support both formats,
	// and failover handles the rare 404 from providers that don't.
	if requestFormat == "chat" && providerFormat == "responses" {
		return false
	}
	return true
}

func weightedSelect(candidates []ScoredProvider) ScoredProvider {
	if len(candidates) == 1 {
		return candidates[0]
	}

	// 90% pick the best, 10% probe a random alternative (health awareness)
	if rand.Float64() < 0.9 {
		return candidates[0] // already sorted by score desc
	}
	return candidates[1+rand.Intn(len(candidates)-1)]
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// DebugScores returns the raw scored providers for a given model, sorted by score desc.
func (rt *RoutingTable) DebugScores(model string) []ScoredProvider {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	src := rt.models[model]
	out := make([]ScoredProvider, len(src))
	copy(out, src)
	return out
}
