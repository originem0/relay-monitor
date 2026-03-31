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
	ProviderID         int64
	ProviderName       string
	BaseURL            string
	APIKey             string
	APIFormat          string // "chat" or "responses"
	Vendor             string
	Score              float64
	LatencyMs          int64
	ChatStreaming      *bool
	ChatToolUse        *bool
	ResponsesBasic     *bool
	ResponsesStreaming *bool
	ResponsesToolUse   *bool
}

type RequestRequirements struct {
	NeedsStreaming bool
	NeedsTools     bool
	NeedsToolCall  bool
}

type rankedCandidate struct {
	ScoredProvider
	matchRank int
}

type DebugCandidate struct {
	ScoredProvider
	MatchRank    int
	BreakerState BreakerState
	Requests     int64
	Errors       int64
	ErrorRate    float64
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
func (rt *RoutingTable) Rebuild(
	results []store.CheckResultRow,
	dbProviders []store.ProviderRow,
	memProviders []provider.Provider,
	fingerprints map[[2]string]FingerprintScore,
	capabilities map[int64]map[string]store.CapabilityRow,
) {
	// Build lookup maps
	keyByName := make(map[string]string)       // provider name → API key
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

	// Build URL lookup map (O(1) access, avoids O(N×M) scan in the result loop below)
	urlByID := make(map[int64]string, len(dbProviders))
	for _, dp := range dbProviders {
		urlByID[dp.ID] = dp.BaseURL
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

		baseURL, ok2 := urlByID[r.ProviderID]
		if !ok2 || baseURL == "" {
			// Provider exists in check results but not in DB — stale record, skip
			continue
		}

		sp := ScoredProvider{
			ProviderID:   r.ProviderID,
			ProviderName: r.ProviderName,
			BaseURL:      baseURL,
			APIKey:       apiKey,
			APIFormat:    formatByID[r.ProviderID],
			Vendor:       r.Vendor,
			Score:        score,
			LatencyMs:    r.LatencyMs,
		}
		if caps, ok := capabilities[r.ProviderID]; ok {
			if cap, ok := caps[r.Model]; ok {
				sp.ChatStreaming = cap.Streaming
				sp.ChatToolUse = cap.ToolUse
				sp.ResponsesBasic = cap.ResponsesBasic
				sp.ResponsesStreaming = cap.ResponsesStreaming
				sp.ResponsesToolUse = cap.ResponsesToolUse
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
func (rt *RoutingTable) Select(model, apiFormat string, req RequestRequirements, stats *Stats, breakers *Breakers) []ScoredProvider {
	candidates := rt.rankedCandidates(model, apiFormat, req, stats, breakers)
	if len(candidates) == 0 {
		return nil
	}

	// Prefer better capability matches first, then better quality within the same tier.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].matchRank != candidates[j].matchRank {
			return candidates[i].matchRank < candidates[j].matchRank
		}
		return candidates[i].Score > candidates[j].Score
	})

	bestRank := candidates[0].matchRank
	var top []ScoredProvider
	for _, candidate := range candidates {
		if candidate.matchRank != bestRank {
			break
		}
		top = append(top, candidate.ScoredProvider)
		if len(top) == 3 {
			break
		}
	}

	primary := weightedSelect(top)

	// Build result: primary first, then others in score order
	result := []ScoredProvider{primary}
	for _, candidate := range candidates {
		if candidate.ProviderID != primary.ProviderID {
			result = append(result, candidate.ScoredProvider)
		}
	}
	return result
}

func (rt *RoutingTable) DebugSelection(model, apiFormat string, req RequestRequirements, stats *Stats, breakers *Breakers) []DebugCandidate {
	candidates := rt.rankedCandidates(model, apiFormat, req, stats, breakers)
	out := make([]DebugCandidate, len(candidates))
	for i, candidate := range candidates {
		debug := DebugCandidate{
			ScoredProvider: candidate.ScoredProvider,
			MatchRank:      candidate.matchRank,
			BreakerState:   BreakerHealthy,
		}
		if breakers != nil {
			debug.BreakerState = breakers.GetState(candidate.ProviderID, model)
		}
		if stats != nil {
			if snap, ok := stats.snapshot(candidate.ProviderID, model); ok {
				debug.Requests = snap.Requests
				debug.Errors = snap.Errors
				if snap.Requests > 0 {
					debug.ErrorRate = float64(snap.Errors) / float64(snap.Requests)
				}
			}
		}
		out[i] = debug
	}
	return out
}

func applyRequestRequirements(sp ScoredProvider, requestFormat string, req RequestRequirements) (ScoredProvider, int, bool) {
	toolUse, streaming, basic := capabilitiesForRequest(sp, requestFormat)
	matchRank := 0

	if requestFormat == "responses" {
		switch {
		case basic != nil && !*basic:
			return sp, 0, false
		case basic != nil && *basic && sp.APIFormat == "responses":
		case basic != nil && *basic:
			matchRank += 1
		case sp.APIFormat == "responses":
			matchRank += 1
		default:
			matchRank += 2
		}
	}

	if req.NeedsTools {
		switch {
		case toolUse != nil && *toolUse:
		case toolUse != nil && !*toolUse:
			return sp, 0, false
		default:
			if req.NeedsToolCall {
				matchRank += 3
			} else {
				matchRank += 2
			}
		}
	}

	if req.NeedsStreaming {
		switch {
		case streaming != nil && !*streaming:
			return sp, 0, false
		case streaming == nil:
			matchRank += 1
		}
	}

	return sp, matchRank, true
}

func capabilitiesForRequest(sp ScoredProvider, requestFormat string) (toolUse, streaming, basic *bool) {
	if requestFormat == "responses" {
		return sp.ResponsesToolUse, sp.ResponsesStreaming, sp.ResponsesBasic
	}
	return sp.ChatToolUse, sp.ChatStreaming, nil
}

// applyErrorPenalty adjusts a provider's score based on live proxy error rate.
// Minimum 5 requests before penalty kicks in (avoid penalizing on noise).
func applyErrorPenalty(sp ScoredProvider, model string, stats *Stats) float64 {
	snap, ok := stats.snapshot(sp.ProviderID, model)
	if !ok || snap.Requests < 5 {
		return sp.Score
	}
	errorRate := float64(snap.Errors) / float64(snap.Requests)
	// Linear penalty: 10% error rate → score × 0.9, 50% → score × 0.5
	return sp.Score * (1.0 - errorRate)
}

func (rt *RoutingTable) rankedCandidates(model, apiFormat string, req RequestRequirements, stats *Stats, breakers *Breakers) []rankedCandidate {
	rt.mu.RLock()
	all := rt.models[model]
	rt.mu.RUnlock()

	if len(all) == 0 {
		return nil
	}

	var candidates []rankedCandidate
	for _, sp := range all {
		if !matchFormat(sp.APIFormat, apiFormat) {
			continue
		}
		adjusted, rank, ok := applyRequestRequirements(sp, apiFormat, req)
		if !ok {
			continue
		}
		if breakers != nil {
			switch breakers.GetState(sp.ProviderID, model) {
			case BreakerOpen:
				continue
			case BreakerHalfOpen:
				adjusted.Score *= 0.3
			case BreakerSuspect:
				adjusted.Score *= 0.5
			}
		}
		if stats != nil {
			adjusted.Score = applyErrorPenalty(adjusted, model, stats)
		}
		candidates = append(candidates, rankedCandidate{
			ScoredProvider: adjusted,
			matchRank:      rank,
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].matchRank != candidates[j].matchRank {
			return candidates[i].matchRank < candidates[j].matchRank
		}
		return candidates[i].Score > candidates[j].Score
	})
	return candidates
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
