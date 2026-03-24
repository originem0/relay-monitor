package proxy

import "testing"

func TestBreakerHealthyToSuspect(t *testing.T) {
	b := NewBreakers()
	var pid int64 = 1
	model := "gpt-5"

	if s := b.GetState(pid, model); s != BreakerHealthy {
		t.Fatalf("initial state = %d, want BreakerHealthy", s)
	}

	b.RecordFailure(pid, model)
	if s := b.GetState(pid, model); s != BreakerHealthy {
		t.Fatalf("after 1 failure = %d, want BreakerHealthy", s)
	}

	b.RecordFailure(pid, model)
	if s := b.GetState(pid, model); s != BreakerSuspect {
		t.Fatalf("after 2 failures = %d, want BreakerSuspect", s)
	}
}

func TestBreakerSuspectToOpen(t *testing.T) {
	b := NewBreakers()
	var pid int64 = 1
	model := "gpt-5"

	b.RecordFailure(pid, model)
	b.RecordFailure(pid, model)
	b.RecordFailure(pid, model)

	if s := b.GetState(pid, model); s != BreakerOpen {
		t.Fatalf("after 3 failures = %d, want BreakerOpen", s)
	}
}

func TestBreakerOpenToHalfOpen(t *testing.T) {
	b := NewBreakers()
	var pid int64 = 1
	model := "gpt-5"

	// Drive to Open
	b.RecordFailure(pid, model)
	b.RecordFailure(pid, model)
	b.RecordFailure(pid, model)

	// Manually set cooldownUntil to past to simulate time passing
	e := b.getOrCreate(pid, model)
	e.mu.Lock()
	e.cooldownUntil = e.cooldownUntil.Add(-cooldownDuration - 1)
	e.mu.Unlock()

	if s := b.GetState(pid, model); s != BreakerHalfOpen {
		t.Fatalf("after cooldown = %d, want BreakerHalfOpen", s)
	}
}

func TestBreakerHalfOpenSuccess(t *testing.T) {
	b := NewBreakers()
	var pid int64 = 1
	model := "gpt-5"

	b.ForceState(pid, model, BreakerHalfOpen)

	b.RecordSuccess(pid, model)
	if s := b.GetState(pid, model); s != BreakerHealthy {
		t.Fatalf("after halfopen success = %d, want BreakerHealthy", s)
	}
}

func TestBreakerHalfOpenFailure(t *testing.T) {
	b := NewBreakers()
	var pid int64 = 1
	model := "gpt-5"

	b.ForceState(pid, model, BreakerHalfOpen)

	b.RecordFailure(pid, model)
	if s := b.GetState(pid, model); s != BreakerOpen {
		t.Fatalf("after halfopen failure = %d, want BreakerOpen", s)
	}
}

func TestAcquireProbeAtomicity(t *testing.T) {
	b := NewBreakers()
	var pid int64 = 1
	model := "gpt-5"

	b.ForceState(pid, model, BreakerHalfOpen)

	first := b.AcquireProbe(pid, model)
	second := b.AcquireProbe(pid, model)

	if !first {
		t.Error("first AcquireProbe should return true")
	}
	if second {
		t.Error("second AcquireProbe should return false")
	}
}

func TestBreakerForceState(t *testing.T) {
	b := NewBreakers()
	var pid int64 = 1
	model := "gpt-5"

	b.RecordFailure(pid, model)
	b.RecordFailure(pid, model)
	b.RecordFailure(pid, model)

	b.ForceState(pid, model, BreakerHealthy)
	if s := b.GetState(pid, model); s != BreakerHealthy {
		t.Fatalf("after ForceState(Healthy) = %d, want BreakerHealthy", s)
	}
}
