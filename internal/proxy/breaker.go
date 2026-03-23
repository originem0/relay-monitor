package proxy

import (
	"fmt"
	"sync"
	"time"
)

// BreakerState represents the circuit breaker state for a (provider, model) pair.
type BreakerState int

const (
	BreakerHealthy  BreakerState = iota
	BreakerSuspect               // 2+ failures in window, still routed at reduced score
	BreakerOpen                  // not routed, waiting for cooldown
	BreakerHalfOpen              // cooldown expired, one probe allowed
)

type breakerEntry struct {
	mu              sync.Mutex
	state           BreakerState
	failures        int       // consecutive failures
	firstFailure    time.Time // start of current failure window
	lastFailure     time.Time
	cooldownUntil   time.Time // when OPEN, the earliest time to transition to HALF_OPEN
	probeInProgress bool      // true while a HALF_OPEN probe is in flight
}

const (
	failureWindow     = 5 * time.Minute
	suspectThreshold  = 2
	openThreshold     = 3
	cooldownDuration  = 60 * time.Second
)

// Breakers manages per-(provider, model) circuit breaker state.
type Breakers struct {
	entries sync.Map // "providerID:model" → *breakerEntry
}

func NewBreakers() *Breakers {
	return &Breakers{}
}

type breakerKey struct {
	ProviderID int64
	Model      string
}

func (b *Breakers) key(providerID int64, model string) string {
	// Simple string key for sync.Map
	return fmt.Sprintf("%d:%s", providerID, model)
}

func (b *Breakers) getOrCreate(providerID int64, model string) *breakerEntry {
	k := b.key(providerID, model)
	if v, ok := b.entries.Load(k); ok {
		return v.(*breakerEntry)
	}
	e := &breakerEntry{}
	actual, _ := b.entries.LoadOrStore(k, e)
	return actual.(*breakerEntry)
}

// GetState returns the current breaker state, handling time-based transitions.
func (b *Breakers) GetState(providerID int64, model string) BreakerState {
	e := b.getOrCreate(providerID, model)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state == BreakerOpen && time.Now().After(e.cooldownUntil) {
		e.state = BreakerHalfOpen
		e.probeInProgress = false
	}
	return e.state
}

// AcquireProbe attempts to claim the single probe slot in HALF_OPEN state.
// Returns true if this caller should act as the probe request.
func (b *Breakers) AcquireProbe(providerID int64, model string) bool {
	e := b.getOrCreate(providerID, model)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state == BreakerHalfOpen && !e.probeInProgress {
		e.probeInProgress = true
		return true
	}
	return false
}

// RecordSuccess marks a successful request. Resets the breaker to HEALTHY.
func (b *Breakers) RecordSuccess(providerID int64, model string) {
	e := b.getOrCreate(providerID, model)
	e.mu.Lock()
	defer e.mu.Unlock()

	e.state = BreakerHealthy
	e.failures = 0
	e.probeInProgress = false
}

// RecordFailure marks a failed request and transitions state as needed.
func (b *Breakers) RecordFailure(providerID int64, model string) {
	e := b.getOrCreate(providerID, model)
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	// If in HALF_OPEN, a failure sends us back to OPEN
	if e.state == BreakerHalfOpen {
		e.state = BreakerOpen
		e.cooldownUntil = now.Add(cooldownDuration)
		e.probeInProgress = false
		return
	}

	// Reset window if first failure was too long ago
	if e.failures > 0 && now.Sub(e.firstFailure) > failureWindow {
		e.failures = 0
	}

	if e.failures == 0 {
		e.firstFailure = now
	}
	e.failures++
	e.lastFailure = now

	switch {
	case e.failures >= openThreshold:
		e.state = BreakerOpen
		e.cooldownUntil = now.Add(cooldownDuration)
	case e.failures >= suspectThreshold:
		e.state = BreakerSuspect
	}
}

// ForceState overrides the breaker state. Used when scheduled checks complete.
func (b *Breakers) ForceState(providerID int64, model string, state BreakerState) {
	e := b.getOrCreate(providerID, model)
	e.mu.Lock()
	defer e.mu.Unlock()

	e.state = state
	if state == BreakerHealthy {
		e.failures = 0
	}
	e.probeInProgress = false
}
