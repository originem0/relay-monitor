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

	// Reset window if expired — prevents stale error rates from lingering forever
	windowAge := time.Since(time.Unix(v.windowStart.Load(), 0))
	if windowAge > statsWindowDuration {
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
}

// Snapshot returns a copy of all current counters.
func (s *Stats) Snapshot() []StatsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]StatsSnapshot, 0, len(s.counters))
	for k, v := range s.counters {
		reqs := v.Requests.Load()
		snap := StatsSnapshot{
			ProviderID: k.ProviderID,
			Model:      k.Model,
			Requests:   reqs,
			Errors:     v.Errors.Load(),
		}
		if reqs > 0 {
			snap.AvgLatencyMs = v.TotalLatency.Load() / reqs
		}
		out = append(out, snap)
	}
	return out
}
