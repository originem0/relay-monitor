package proxy

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"relay-monitor/internal/provider"
	"relay-monitor/internal/store"
)

// maxRoutableAge bounds how stale a check result may be and still feed the
// routing table. Beyond it the snapshot is a zombie — the station likely went
// dark or dropped the model — so it's excluded and the proxy never routes on
// weeks-old "available" data. The front-end flags such rows as stale, and the
// run-time breaker is a second line of defense. Mirrors store.maxStaleAge.
const maxRoutableAge = 48 * time.Hour

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
	Breakdown          ScoreBreakdown // populated during Rebuild
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
		// Disabled or route-paused providers are not routable — keep them out of
		// the table entirely. Doing it here (rather than in callers) means every
		// rebuild path, including main.go's startup load, excludes them.
		if p.Disabled || p.RoutePaused {
			continue
		}
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
		// Hard filter: the individual model check must have passed
		if !r.Correct || r.Status != "ok" {
			continue
		}

		// Zombie filter: a result not successfully re-checked within maxRoutableAge
		// is stale (station went dark / dropped the model); don't route on it.
		if !r.CheckedAt.IsZero() && time.Since(r.CheckedAt) > maxRoutableAge {
			continue
		}

		apiKey, ok := keyByName[r.ProviderName]
		if !ok || apiKey == "" {
			continue
		}

		// Score: 0.4 × latency + 0.3 × health + 0.3 × fingerprint
		// Provider-level health feeds into the score as a continuous factor,
		// NOT as a hard gate. A provider with health=40% gets a lower score
		// but its individually-correct models still enter the routing table.
		latencyScore := 1.0 - clamp(float64(r.LatencyMs)/30000.0, 0, 1)
		healthScore := healthByID[r.ProviderID] / 100.0

		// Providers in error/down state get an additional penalty but are not excluded.
		// This keeps their correct models routable at reduced priority.
		pStatus := statusByID[r.ProviderID]
		providerPenalty := 1.0
		if pStatus == "error" {
			providerPenalty = 0.3
		} else if pStatus == "down" {
			providerPenalty = 0.5
		} else if pStatus == "degraded" {
			providerPenalty = 0.8
		}
		fpScore := 0.5 // default when no fingerprint data
		fpVerdict := ""
		fpKey := [2]string{r.ProviderName, r.Model}
		if fp, ok := fingerprints[fpKey]; ok {
			fpScore = clamp(float64(fp.TotalScore)/10.0, 0, 1)
			fpVerdict = fp.Verdict
			// Penalize suspected/fake heavily
			if fp.Verdict == "LIKELY FAKE" || fp.Verdict == "FAIL" {
				fpScore = 0
			} else if fp.Verdict == "SUSPECTED" || fp.Verdict == "MISMATCH" {
				fpScore *= 0.3
			}
		}
		baseScore := 0.4*latencyScore + 0.3*healthScore + 0.3*fpScore
		score := baseScore

		// Manual priority boost (default 1.0, set >1 to prioritize)
		priMul := priorityByName[r.ProviderName]
		if priMul <= 0 {
			priMul = 1.0
		}
		score *= priMul
		score *= providerPenalty

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
			Breakdown: ScoreBreakdown{
				LatencyScore:       latencyScore,
				HealthScore:        healthScore,
				FingerprintScore:   fpScore,
				FingerprintVerdict: fpVerdict,
				BaseScore:          baseScore,
				PriorityMul:        priMul,
			},
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
	candidates, _ := rt.rankedCandidates(model, apiFormat, req, stats, breakers, nil)
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
	candidates, _ := rt.rankedCandidates(model, apiFormat, req, stats, breakers, nil)
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
		case basic != nil && *basic:
			// Capability probes trump the provider's declared wire format.
			// If we already proved /responses works, don't force traffic onto
			// a lower-quality "native responses" provider just because its
			// metadata says APIFormat=responses.
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

// selectionExplanation collects filtering reasons when non-nil.
type selectionExplanation struct {
	Filtered []FilteredEntry
}

func (rt *RoutingTable) rankedCandidates(model, apiFormat string, req RequestRequirements, stats *Stats, breakers *Breakers, explain *selectionExplanation) ([]rankedCandidate, *rankedCandidate) {
	rt.mu.RLock()
	all := rt.models[model]
	rt.mu.RUnlock()

	if len(all) == 0 {
		return nil, nil
	}

	var candidates []rankedCandidate
	var bestOpen *rankedCandidate // highest-scoring open-breaker candidate, kept as a forced-probe fallback
	for _, sp := range all {
		if !matchFormat(sp.APIFormat, apiFormat) {
			if explain != nil {
				explain.Filtered = append(explain.Filtered, FilteredEntry{
					ProviderID: sp.ProviderID, ProviderName: sp.ProviderName,
					ReasonCode: "format_mismatch",
					Detail:     fmt.Sprintf("chat request to %s-format provider", sp.APIFormat),
				})
			}
			continue
		}
		adjusted, rank, ok := applyRequestRequirements(sp, apiFormat, req)
		if !ok {
			if explain != nil {
				explain.Filtered = append(explain.Filtered, FilteredEntry{
					ProviderID: sp.ProviderID, ProviderName: sp.ProviderName,
					ReasonCode: "capability_unsupported",
					Detail:     filterReasonFromRequirements(sp, apiFormat, req),
				})
			}
			continue
		}
		if breakers != nil {
			state := breakers.GetState(sp.ProviderID, model)
			switch state {
			case BreakerOpen:
				if explain != nil {
					explain.Filtered = append(explain.Filtered, FilteredEntry{
						ProviderID: sp.ProviderID, ProviderName: sp.ProviderName,
						ReasonCode: "breaker_open",
						Detail:     "circuit breaker open",
					})
				}
				// This candidate passed format + capability and is excluded only by
				// its open breaker. Remember the best such one so SelectWithExplanation
				// can fall back to a single forced probe if nothing else can route.
				cand := rankedCandidate{ScoredProvider: adjusted, matchRank: rank}
				if bestOpen == nil || cand.matchRank < bestOpen.matchRank ||
					(cand.matchRank == bestOpen.matchRank && cand.Score > bestOpen.Score) {
					bestOpen = &cand
				}
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
		return nil, bestOpen
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].matchRank != candidates[j].matchRank {
			return candidates[i].matchRank < candidates[j].matchRank
		}
		return candidates[i].Score > candidates[j].Score
	})
	return candidates, bestOpen
}

// filterReasonFromRequirements returns a human-readable explanation of why
// capability requirements weren't met.
func filterReasonFromRequirements(sp ScoredProvider, requestFormat string, req RequestRequirements) string {
	toolUse, streaming, basic := capabilitiesForRequest(sp, requestFormat)
	if requestFormat == "responses" && basic != nil && !*basic {
		return "responses API unsupported"
	}
	if req.NeedsTools && toolUse != nil && !*toolUse {
		return requestFormat + " tool_use unsupported"
	}
	if req.NeedsStreaming && streaming != nil && !*streaming {
		return requestFormat + " streaming unsupported"
	}
	return "capability mismatch"
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

// SelectWithExplanation is like Select but also returns filtering reasons for tracing.
// The final bool is forcedProbe: true when every real candidate was filtered and the
// returned single candidate is a best-effort forced probe of an open-breaker provider.
func (rt *RoutingTable) SelectWithExplanation(model, apiFormat string, req RequestRequirements, stats *Stats, breakers *Breakers) ([]ScoredProvider, []CandidateEntry, []FilteredEntry, bool) {
	var explain selectionExplanation
	candidates, bestOpen := rt.rankedCandidates(model, apiFormat, req, stats, breakers, &explain)

	// Build candidate entries for trace
	var candidateEntries []CandidateEntry
	for _, c := range candidates {
		state := BreakerHealthy
		if breakers != nil {
			state = breakers.GetState(c.ProviderID, model)
		}
		candidateEntries = append(candidateEntries, CandidateEntry{
			ProviderID:   c.ProviderID,
			ProviderName: c.ProviderName,
			Score:        c.Score,
			MatchRank:    c.matchRank,
			BreakerState: state.String(),
			Breakdown:    c.Breakdown,
		})
	}

	if len(candidates) == 0 {
		// Every real candidate was filtered. If the only blocker is open breakers,
		// fall back to the best open provider as a single forced-probe candidate, so
		// a fully-blacked-out model still gets a recovery attempt instead of a hard 404.
		if bestOpen != nil {
			state := BreakerOpen
			if breakers != nil {
				state = breakers.GetState(bestOpen.ProviderID, model)
			}
			entry := CandidateEntry{
				ProviderID:   bestOpen.ProviderID,
				ProviderName: bestOpen.ProviderName,
				Score:        bestOpen.Score,
				MatchRank:    bestOpen.matchRank,
				BreakerState: state.String(),
				Breakdown:    bestOpen.Breakdown,
			}
			return []ScoredProvider{bestOpen.ScoredProvider}, []CandidateEntry{entry}, explain.Filtered, true
		}
		return nil, candidateEntries, explain.Filtered, false
	}

	// Same selection logic as Select
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
	result := []ScoredProvider{primary}
	for _, candidate := range candidates {
		if candidate.ProviderID != primary.ProviderID {
			result = append(result, candidate.ScoredProvider)
		}
	}
	return result, candidateEntries, explain.Filtered, false
}

// RoutingExplanation is the full explanation for a routing decision.
type RoutingExplanation struct {
	Model      string           `json:"model"`
	Endpoint   string           `json:"endpoint"`
	Stream     bool             `json:"stream"`
	Tools      bool             `json:"tools"`
	Candidates []CandidateEntry `json:"candidates"`
	Filtered   []FilteredEntry  `json:"filtered"`
}

// Explain returns a detailed explanation of the routing decision for a model+shape.
func (rt *RoutingTable) Explain(model, apiFormat string, req RequestRequirements, stats *Stats, breakers *Breakers) *RoutingExplanation {
	_, candidates, filtered, _ := rt.SelectWithExplanation(model, apiFormat, req, stats, breakers)
	return &RoutingExplanation{
		Model:      model,
		Endpoint:   apiFormat,
		Stream:     req.NeedsStreaming,
		Tools:      req.NeedsTools,
		Candidates: candidates,
		Filtered:   filtered,
	}
}
