package proxy

import (
	"math"
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

// Rebuild reconstructs the routing table from the latest check results and provider list.
// providers supplies API keys (not stored in DB). breakers supplies circuit breaker state.
func (rt *RoutingTable) Rebuild(results []store.CheckResultRow, dbProviders []store.ProviderRow, memProviders []provider.Provider, breakers *Breakers) {
	// Build lookup maps
	keyByName := make(map[string]string) // provider name → API key
	for _, p := range memProviders {
		keyByName[p.Name] = p.APIKey
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

		// Score: 0.6 × latency_score + 0.4 × health_score
		latencyScore := 1.0 - clamp(float64(r.LatencyMs)/30000.0, 0, 1)
		healthScore := healthByID[r.ProviderID] / 100.0
		score := 0.6*latencyScore + 0.4*healthScore

		// Apply circuit breaker modifier
		if breakers != nil {
			switch breakers.GetState(r.ProviderID, r.Model) {
			case BreakerSuspect:
				score *= 0.5
			case BreakerOpen:
				continue // skip entirely
			case BreakerHalfOpen:
				score *= 0.3 // allow but with very low priority
			}
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
// The first element is the weighted-random primary choice; the rest are failover candidates in score order.
func (rt *RoutingTable) Select(model, apiFormat string) []ScoredProvider {
	rt.mu.RLock()
	all := rt.models[model]
	rt.mu.RUnlock()

	if len(all) == 0 {
		return nil
	}

	// Filter by API format
	var candidates []ScoredProvider
	for _, sp := range all {
		if matchFormat(sp.APIFormat, apiFormat) {
			candidates = append(candidates, sp)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// Take top 3 for weighted random selection
	top := candidates
	if len(top) > 3 {
		top = top[:3]
	}

	// Weighted random: use softmax-style normalization
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
	if requestFormat == "responses" {
		return providerFormat == "responses"
	}
	// chat format: accept anything that's not exclusively responses
	return providerFormat != "responses"
}

func weightedSelect(candidates []ScoredProvider) ScoredProvider {
	if len(candidates) == 1 {
		return candidates[0]
	}

	// Compute weights using exponential scaling
	weights := make([]float64, len(candidates))
	total := 0.0
	for i, c := range candidates {
		w := math.Exp(c.Score * 3) // scale factor 3 gives good spread
		weights[i] = w
		total += w
	}

	r := rand.Float64() * total
	cumulative := 0.0
	for i, w := range weights {
		cumulative += w
		if r <= cumulative {
			return candidates[i]
		}
	}
	return candidates[len(candidates)-1]
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
