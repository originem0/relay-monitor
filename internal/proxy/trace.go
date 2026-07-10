package proxy

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"relay-monitor/internal/store"
)

// FailureClass categorizes proxy failures for observability.
type FailureClass string

const (
	FailureNone           FailureClass = ""
	FailureTimeout        FailureClass = "timeout"
	FailureUpstream5xx    FailureClass = "upstream_5xx"
	FailureRateLimit      FailureClass = "rate_limit"
	FailureAuthFailed     FailureClass = "auth_failed"
	FailureQuotaExhausted FailureClass = "quota_exhausted"
	FailureModelGone      FailureClass = "model_gone"
	FailureBodyTooLarge   FailureClass = "body_too_large"
	FailureClientError    FailureClass = "client_error"
	FailureProtocolError  FailureClass = "protocol_error"
	FailureStreamIdle     FailureClass = "stream_idle"
	FailureStreamBroken   FailureClass = "stream_broken"
	FailureToolMissing    FailureClass = "tool_call_missing"
)

// persistentFailure reports failure classes that won't heal between requests
// (revoked key, exhausted quota, model removed). They feed the circuit breaker
// even though their individual retry severity is "mild".
func persistentFailure(fc FailureClass) bool {
	return fc == FailureModelGone || fc == FailureAuthFailed || fc == FailureQuotaExhausted
}

// classifyUpstreamFailure maps HTTP status to retry behavior + failure class.
func classifyUpstreamFailure(httpStatus int) (forwardResult, FailureClass) {
	switch {
	case httpStatus >= 500 || httpStatus == 429:
		fc := FailureUpstream5xx
		if httpStatus == 429 {
			fc = FailureRateLimit
		}
		return forwardRetry, fc
	case httpStatus == 401:
		return forwardRetryMild, FailureAuthFailed
	case httpStatus == 403:
		return forwardRetryMild, FailureQuotaExhausted
	case httpStatus == 404:
		return forwardRetryMild, FailureModelGone
	case httpStatus == 413:
		return forwardRetryMild, FailureBodyTooLarge
	case httpStatus >= 400:
		return forwardRetryMild, FailureClientError
	default:
		return forwardRetryMild, FailureClientError
	}
}

// ScoreBreakdown shows how a provider's routing score was computed.
type ScoreBreakdown struct {
	LatencyScore       float64 `json:"latency_score"`
	HealthScore        float64 `json:"health_score"`
	FingerprintScore   float64 `json:"fingerprint_score"`
	FingerprintVerdict string  `json:"fingerprint_verdict,omitempty"`
	BaseScore          float64 `json:"base_score"`
	PriorityMul        float64 `json:"priority_mul"`
}

// Attempt records what happened when a single upstream provider was tried.
type Attempt struct {
	Index        int          `json:"index"`
	ProviderID   int64        `json:"provider_id"`
	ProviderName string       `json:"provider_name"`
	Score        float64      `json:"score"`
	MatchRank    int          `json:"match_rank"`
	BreakerState string       `json:"breaker_state"`
	HTTPStatus   int          `json:"http_status"`
	Failure      FailureClass `json:"failure,omitempty"`
	LatencyMs    int64        `json:"latency_ms"`
	WroteBody    bool         `json:"wrote_body"`
}

// FilteredEntry explains why a provider was excluded from routing.
type FilteredEntry struct {
	ProviderID   int64  `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	ReasonCode   string `json:"reason_code"`
	Detail       string `json:"detail,omitempty"`
}

// CandidateEntry describes a provider that was eligible for routing.
type CandidateEntry struct {
	ProviderID   int64          `json:"provider_id"`
	ProviderName string         `json:"provider_name"`
	Score        float64        `json:"score"`
	MatchRank    int            `json:"match_rank"`
	BreakerState string         `json:"breaker_state"`
	Breakdown    ScoreBreakdown `json:"breakdown"`
}

// Trace is the complete record of one proxy request's journey.
type Trace struct {
	ID           string           `json:"id"`
	ReceivedAt   time.Time        `json:"received_at"`
	Model        string           `json:"model"`
	Endpoint     string           `json:"endpoint"`
	Stream       bool             `json:"stream"`
	HasTools     bool             `json:"has_tools"`
	ToolRequired bool             `json:"tool_required"`
	Candidates   []CandidateEntry `json:"candidates"`
	Filtered     []FilteredEntry  `json:"filtered"`
	Attempts     []Attempt        `json:"attempts"`
	FinalStatus  string           `json:"final_status"`
	LatencyMs    int64            `json:"latency_ms"`
}

// TraceSummary is the lightweight version for list views.
type TraceSummary struct {
	ID            string    `json:"id"`
	ReceivedAt    time.Time `json:"received_at"`
	Model         string    `json:"model"`
	Endpoint      string    `json:"endpoint"`
	Stream        bool      `json:"stream"`
	AttemptCount  int       `json:"attempt_count"`
	FinalProvider string    `json:"final_provider"`
	FinalStatus   string    `json:"final_status"`
	LatencyMs     int64     `json:"latency_ms"`
}

func (t *Trace) Summary() TraceSummary {
	s := TraceSummary{
		ID:           t.ID,
		ReceivedAt:   t.ReceivedAt,
		Model:        t.Model,
		Endpoint:     t.Endpoint,
		Stream:       t.Stream,
		AttemptCount: len(t.Attempts),
		FinalStatus:  t.FinalStatus,
		LatencyMs:    t.LatencyMs,
	}
	// Final provider = the last successful attempt, or the last attempt overall
	for i := len(t.Attempts) - 1; i >= 0; i-- {
		if t.Attempts[i].Failure == FailureNone {
			s.FinalProvider = t.Attempts[i].ProviderName
			break
		}
	}
	if s.FinalProvider == "" && len(t.Attempts) > 0 {
		s.FinalProvider = t.Attempts[len(t.Attempts)-1].ProviderName
	}
	return s
}

// traceIDFallback supplies monotonic IDs if crypto/rand ever fails, so we never
// emit a zero-valued (collision-prone) ID that INSERT OR IGNORE would silently drop.
var traceIDFallback atomic.Uint64

func newTraceID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		binary.BigEndian.PutUint64(buf[:], traceIDFallback.Add(1))
	}
	return "tr_" + hex.EncodeToString(buf[:])
}

// DetailJSON serializes the full trace for storage.
func (t *Trace) DetailJSON() []byte {
	b, _ := json.Marshal(t)
	return b
}

// --- TraceCollector: channel-based, single writer ---

func traceToRow(t *Trace) store.TraceRow {
	s := t.Summary()
	return store.TraceRow{
		TraceID:       t.ID,
		ReceivedAt:    t.ReceivedAt,
		Model:         t.Model,
		Endpoint:      t.Endpoint,
		Stream:        t.Stream,
		HasTools:      t.HasTools,
		Attempts:      len(t.Attempts),
		FinalProvider: s.FinalProvider,
		FinalStatus:   t.FinalStatus,
		LatencyMs:     t.LatencyMs,
		DetailJSON:    t.DetailJSON(),
	}
}

// TraceStats holds aggregated trace statistics, computed incrementally.
type TraceStats struct {
	Total        int64 `json:"total"`
	OK           int64 `json:"ok"`
	Failed       int64 `json:"failed"`
	AvgLatencyMs int64 `json:"avg_latency_ms"`
}

type traceStatsAccum struct {
	total        atomic.Int64
	ok           atomic.Int64
	failed       atomic.Int64
	totalLatency atomic.Int64
}

func (a *traceStatsAccum) record(t *Trace) {
	a.total.Add(1)
	a.totalLatency.Add(t.LatencyMs)
	if t.FinalStatus == "ok" {
		a.ok.Add(1)
	} else {
		a.failed.Add(1)
	}
}

func (a *traceStatsAccum) snapshot() TraceStats {
	total := a.total.Load()
	s := TraceStats{
		Total:  total,
		OK:     a.ok.Load(),
		Failed: a.failed.Load(),
	}
	if total > 0 {
		s.AvgLatencyMs = a.totalLatency.Load() / total
	}
	return s
}

// TraceCollector receives traces via a channel, maintains running stats,
// and batch-writes to SQLite.
type TraceCollector struct {
	ch         chan Trace
	mu         sync.RWMutex
	recent     []Trace // ring buffer of capacity recentSize
	head       int     // index of the next write slot
	count      int     // number of valid entries, <= recentSize
	recentSize int
	stats      traceStatsAccum
	store      *store.Store // nil = memory only
	maxKeep    int          // SQLite ring buffer size
	done       chan struct{}
}

const (
	defaultRecentSize  = 200
	defaultMaxKeep     = 10000
	defaultChannelSize = 256
	flushInterval      = time.Second
	flushBatchSize     = 50
)

// NewTraceCollector creates a collector. st may be nil for memory-only operation.
func NewTraceCollector(st *store.Store, recentSize, maxKeep int) *TraceCollector {
	if recentSize <= 0 {
		recentSize = defaultRecentSize
	}
	if maxKeep <= 0 {
		maxKeep = defaultMaxKeep
	}
	return &TraceCollector{
		ch:         make(chan Trace, defaultChannelSize),
		recent:     make([]Trace, recentSize),
		recentSize: recentSize,
		store:      st,
		maxKeep:    maxKeep,
		done:       make(chan struct{}),
	}
}

// Start launches the collector goroutine. Call Stop to shut down.
func (tc *TraceCollector) Start() {
	go tc.run()
}

// Stop drains pending traces and shuts down the collector.
func (tc *TraceCollector) Stop() {
	close(tc.ch)
	<-tc.done
}

func (tc *TraceCollector) run() {
	defer close(tc.done)

	var batch []store.TraceRow
	var totalInserted int64
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 || tc.store == nil {
			batch = batch[:0]
			return
		}
		if err := tc.store.InsertTraces(batch); err != nil {
			log.Printf("[trace] insert %d traces failed: %v", len(batch), err)
		}
		totalInserted += int64(len(batch))
		batch = batch[:0]
		// Only trim when we've inserted enough to possibly exceed the limit
		if totalInserted >= int64(tc.maxKeep)/10 {
			if err := tc.store.TrimTraces(tc.maxKeep); err != nil {
				log.Printf("[trace] trim to %d failed: %v", tc.maxKeep, err)
			}
			totalInserted = 0
		}
	}

	for {
		select {
		case t, ok := <-tc.ch:
			if !ok {
				flush()
				return
			}
			tc.stats.record(&t)
			tc.pushRecent(t)
			if tc.store != nil {
				batch = append(batch, traceToRow(&t))
				if len(batch) >= flushBatchSize {
					flush()
				}
			}
		case <-ticker.C:
			flush()
		}
	}
}

// pushRecent appends to the ring buffer in O(1) — no per-insert slice shifting.
func (tc *TraceCollector) pushRecent(t Trace) {
	tc.mu.Lock()
	tc.recent[tc.head] = t
	tc.head = (tc.head + 1) % tc.recentSize
	if tc.count < tc.recentSize {
		tc.count++
	}
	tc.mu.Unlock()
}

// Emit sends a trace to the collector. Non-blocking: drops trace if channel full.
func (tc *TraceCollector) Emit(t Trace) {
	select {
	case tc.ch <- t:
	default:
		// Channel full — drop trace rather than blocking the proxy goroutine
	}
}

// Recent returns the N most recent traces (newest first).
func (tc *TraceCollector) Recent(limit int) []TraceSummary {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	n := tc.count
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]TraceSummary, n)
	for i := 0; i < n; i++ {
		// Walk backwards from the most recently written slot.
		idx := (tc.head - 1 - i + tc.recentSize) % tc.recentSize
		out[i] = tc.recent[idx].Summary()
	}
	return out
}

// Get returns a full trace by ID from the in-memory recent buffer.
func (tc *TraceCollector) Get(id string) (Trace, bool) {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	// Only the first `count` slots hold valid entries (the ring fills [0,count)
	// before wrapping; once full, count == recentSize covers every slot).
	for i := 0; i < tc.count; i++ {
		if tc.recent[i].ID == id {
			return tc.recent[i], true
		}
	}
	return Trace{}, false
}

// Stats returns aggregated trace statistics (O(1)).
func (tc *TraceCollector) Stats() TraceStats {
	return tc.stats.snapshot()
}

// Store returns the underlying store, or nil if memory-only.
func (tc *TraceCollector) Store() *store.Store {
	return tc.store
}
