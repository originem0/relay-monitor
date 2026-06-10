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

func (s BreakerState) String() string {
	switch s {
	case BreakerHealthy:
		return "healthy"
	case BreakerSuspect:
		return "suspect"
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

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
	failureWindow    = 5 * time.Minute
	suspectThreshold = 2
	openThreshold    = 3
	cooldownDuration = 60 * time.Second
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

// ReleaseProbe frees the half-open probe slot without changing breaker state.
func (b *Breakers) ReleaseProbe(providerID int64, model string) {
	e := b.getOrCreate(providerID, model)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state == BreakerHalfOpen {
		e.probeInProgress = false
	}
}

// AcquireForceProbe grants a single probe while the breaker is OPEN. It lets a
// fully-blacked-out model get one recovery attempt without waiting out the 60s
// cooldown; the probeInProgress flag still guarantees only one request probes at
// a time (concurrent callers see false and fail fast, so the dead provider is not
// stampeded). The slot is released by RecordSuccess, or by the OPEN→HALF_OPEN
// transition in GetState once the cooldown elapses. A failed probe keeps the slot
// held (RecordFailure does not touch it in the OPEN branch), so the provider is
// not re-probed until its cooldown turns it half-open.
func (b *Breakers) AcquireForceProbe(providerID int64, model string) bool {
	e := b.getOrCreate(providerID, model)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state == BreakerOpen && !e.probeInProgress {
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

// ForceState overrides the breaker state directly. It is a test/diagnostic
// helper for constructing a known state — production code drives breaker
// transitions through Record{Success,Failure} and the time-based GetState
// transition, not through this method. Scheduled checks deliberately do NOT
// reset breakers: a proxy-observed failure is more recent than an 8h check, and
// the 60s cooldown re-admits a recovered provider fast enough on its own.
func (b *Breakers) ForceState(providerID int64, model string, state BreakerState) {
	e := b.getOrCreate(providerID, model)
	e.mu.Lock()
	defer e.mu.Unlock()

	e.state = state
	if state == BreakerHealthy {
		e.failures = 0
	}
	if state == BreakerOpen {
		e.cooldownUntil = time.Now().Add(cooldownDuration)
	}
	e.probeInProgress = false
}
