package proxy

import (
	"testing"
	"time"
)

func TestTraceCollector_EmitAndRecent(t *testing.T) {
	tc := NewTraceCollector(nil, 5, 100) // memory only, 5 recent slots
	tc.Start()
	defer tc.Stop()

	// Emit 3 traces
	for i := 0; i < 3; i++ {
		tc.Emit(Trace{
			ID:          newTraceID(),
			ReceivedAt:  time.Now(),
			Model:       "gpt-5.4",
			Endpoint:    "chat",
			FinalStatus: "ok",
			LatencyMs:   int64(100 * (i + 1)),
		})
	}

	// Give collector goroutine time to process
	time.Sleep(50 * time.Millisecond)

	recent := tc.Recent(10)
	if len(recent) != 3 {
		t.Fatalf("expected 3 recent traces, got %d", len(recent))
	}

	// Most recent should be first
	if recent[0].LatencyMs != 300 {
		t.Errorf("expected most recent trace (300ms) first, got %dms", recent[0].LatencyMs)
	}
}

func TestTraceCollector_RecentBounded(t *testing.T) {
	tc := NewTraceCollector(nil, 3, 100) // only 3 slots
	tc.Start()
	defer tc.Stop()

	for i := 0; i < 5; i++ {
		tc.Emit(Trace{
			ID:          newTraceID(),
			ReceivedAt:  time.Now(),
			Model:       "gpt-5.4",
			FinalStatus: "ok",
			LatencyMs:   int64(i + 1),
		})
	}

	time.Sleep(50 * time.Millisecond)

	recent := tc.Recent(10)
	if len(recent) != 3 {
		t.Fatalf("expected 3 recent traces (bounded), got %d", len(recent))
	}

	// Should have the 3 most recent (latency 5, 4, 3)
	if recent[0].LatencyMs != 5 {
		t.Errorf("expected most recent (5ms) first, got %dms", recent[0].LatencyMs)
	}
}

func TestTraceCollector_Stats(t *testing.T) {
	tc := NewTraceCollector(nil, 10, 100)
	tc.Start()
	defer tc.Stop()

	tc.Emit(Trace{ID: newTraceID(), FinalStatus: "ok", LatencyMs: 100})
	tc.Emit(Trace{ID: newTraceID(), FinalStatus: "ok", LatencyMs: 200})
	tc.Emit(Trace{ID: newTraceID(), FinalStatus: "failed", LatencyMs: 300})

	time.Sleep(50 * time.Millisecond)

	stats := tc.Stats()
	if stats.Total != 3 {
		t.Errorf("expected total=3, got %d", stats.Total)
	}
	if stats.OK != 2 {
		t.Errorf("expected ok=2, got %d", stats.OK)
	}
	if stats.Failed != 1 {
		t.Errorf("expected failed=1, got %d", stats.Failed)
	}
	if stats.AvgLatencyMs != 200 { // (100+200+300)/3 = 200
		t.Errorf("expected avg latency=200, got %d", stats.AvgLatencyMs)
	}
}

func TestTraceCollector_Get(t *testing.T) {
	tc := NewTraceCollector(nil, 10, 100)
	tc.Start()
	defer tc.Stop()

	id := newTraceID()
	tc.Emit(Trace{ID: id, Model: "gpt-5.4", FinalStatus: "ok", LatencyMs: 150})

	time.Sleep(50 * time.Millisecond)

	trace, ok := tc.Get(id)
	if !ok {
		t.Fatal("expected to find trace by ID")
	}
	if trace.Model != "gpt-5.4" {
		t.Errorf("expected model gpt-5.4, got %s", trace.Model)
	}

	_, ok = tc.Get("nonexistent")
	if ok {
		t.Error("expected not to find nonexistent trace")
	}
}

func TestTraceSummary(t *testing.T) {
	trace := Trace{
		ID:          "tr_abc123",
		ReceivedAt:  time.Now(),
		Model:       "gpt-5.4",
		Endpoint:    "responses",
		Stream:      true,
		FinalStatus: "ok",
		LatencyMs:   2500,
		Attempts: []Attempt{
			{Index: 0, ProviderName: "alpha", Failure: FailureUpstream5xx, LatencyMs: 500},
			{Index: 1, ProviderName: "beta", Failure: FailureNone, LatencyMs: 2000},
		},
	}

	s := trace.Summary()
	if s.FinalProvider != "beta" {
		t.Errorf("expected final provider beta (successful), got %s", s.FinalProvider)
	}
	if s.AttemptCount != 2 {
		t.Errorf("expected 2 attempts, got %d", s.AttemptCount)
	}
}

func TestClassifyUpstreamFailure(t *testing.T) {
	tests := []struct {
		status    int
		wantResult forwardResult
		wantClass  FailureClass
	}{
		{500, forwardRetry, FailureUpstream5xx},
		{502, forwardRetry, FailureUpstream5xx},
		{429, forwardRetry, FailureRateLimit},
		{401, forwardRetryMild, FailureAuthFailed},
		{403, forwardRetryMild, FailureQuotaExhausted},
		{404, forwardRetryMild, FailureModelGone},
		{413, forwardRetryMild, FailureBodyTooLarge},
		{400, forwardRetryMild, FailureClientError},
		{422, forwardRetryMild, FailureClientError},
	}

	for _, tt := range tests {
		result, class := classifyUpstreamFailure(tt.status)
		if result != tt.wantResult {
			t.Errorf("status %d: expected result %d, got %d", tt.status, tt.wantResult, result)
		}
		if class != tt.wantClass {
			t.Errorf("status %d: expected class %s, got %s", tt.status, tt.wantClass, class)
		}
	}
}

func TestNewTraceID(t *testing.T) {
	id := newTraceID()
	if len(id) < 11 { // "tr_" + 8 hex chars
		t.Errorf("trace ID too short: %s", id)
	}
	if id[:3] != "tr_" {
		t.Errorf("trace ID should start with tr_, got %s", id)
	}

	// Should be unique
	id2 := newTraceID()
	if id == id2 {
		t.Error("two trace IDs should not be identical")
	}
}
