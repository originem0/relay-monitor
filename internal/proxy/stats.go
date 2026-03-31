package proxy

import (
	"sync"
	"sync/atomic"
	"time"
)

type statsKey struct {
	ProviderID int64
	Model      string
}

type statsValue struct {
	mu           sync.Mutex // guards window reset; atomics used for reads outside reset
	Requests     atomic.Int64
	Errors       atomic.Int64
	TotalLatency atomic.Int64 // milliseconds
	windowStart  atomic.Int64 // unix seconds
}

const statsWindowDuration = 10 * time.Minute

// StatsSnapshot is a point-in-time copy of counters for a (provider, model) pair.
type StatsSnapshot struct {
	ProviderID   int64
	Model        string
	Requests     int64
	Errors       int64
	AvgLatencyMs int64
}

func (s *Stats) snapshot(providerID int64, model string) (StatsSnapshot, bool) {
	s.mu.RLock()
	v, ok := s.counters[statsKey{ProviderID: providerID, Model: model}]
	s.mu.RUnlock()
	if !ok {
		return StatsSnapshot{}, false
	}

	reqs := v.Requests.Load()
	snap := StatsSnapshot{
		ProviderID: providerID,
		Model:      model,
		Requests:   reqs,
		Errors:     v.Errors.Load(),
	}
	if reqs > 0 {
		snap.AvgLatencyMs = v.TotalLatency.Load() / reqs
	}
	return snap, true
}

// Stats tracks per-(provider, model) proxy request counters in memory.
type Stats struct {
	mu       sync.RWMutex
	counters map[statsKey]*statsValue
}

func NewStats() *Stats {
	return &Stats{
		counters: make(map[statsKey]*statsValue),
	}
}

func (s *Stats) get(providerID int64, model string) *statsValue {
	k := statsKey{providerID, model}
	s.mu.RLock()
	v, ok := s.counters[k]
	s.mu.RUnlock()
	if ok {
		return v
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.counters[k]; ok {
		return v
	}
	v = &statsValue{}
	v.windowStart.Store(time.Now().Unix())
	s.counters[k] = v
	return v
}

// Record logs a proxy request outcome.
func (s *Stats) Record(providerID int64, model string, latencyMs int64, isError bool) {
	v := s.get(providerID, model)

	// Reset window under lock to avoid the CAS-then-Store(0) race where a
	// concurrent Add(1) lands between CompareAndSwap and Store(0) and gets wiped.
	v.mu.Lock()
	if time.Since(time.Unix(v.windowStart.Load(), 0)) > statsWindowDuration {
		v.Requests.Store(0)
		v.Errors.Store(0)
		v.TotalLatency.Store(0)
		v.windowStart.Store(time.Now().Unix())
	}
	v.Requests.Add(1)
	v.TotalLatency.Add(latencyMs)
	if isError {
		v.Errors.Add(1)
	}
	v.mu.Unlock()
}

// Snapshot returns a copy of all current counters.
func (s *Stats) Snapshot() []StatsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]StatsSnapshot, 0, len(s.counters))
	for k, v := range s.counters {
		reqs := v.Requests.Load()
		avgLatency := int64(0)
		if reqs > 0 {
			avgLatency = v.TotalLatency.Load() / reqs
		}
		out = append(out, StatsSnapshot{
			ProviderID:   k.ProviderID,
			Model:        k.Model,
			Requests:     reqs,
			Errors:       v.Errors.Load(),
			AvgLatencyMs: avgLatency,
		})
	}
	return out
}
