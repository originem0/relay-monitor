package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"relay-monitor/internal/config"
	"relay-monitor/internal/provider"
	"relay-monitor/internal/responsefmt"
	"relay-monitor/internal/store"
)

// Proxy handles reverse-proxy routing of /v1/* requests to the best provider.
type Proxy struct {
	cfg      atomic.Pointer[config.ProxyConfig]
	client   *http.Client
	table    *RoutingTable
	breakers *Breakers
	stats    *Stats
	traces   *TraceCollector
	warming  atomic.Bool // true while initial check is running
}

const (
	defaultMaxRequestBodyBytes   int64 = 8 * 1024 * 1024
	defaultMaxResponsesBodyBytes int64 = 2 * 1024 * 1024
)

// New creates a Proxy with the given config. The routing table starts empty.
// traceStore may be nil to disable trace persistence.
func New(cfg *config.ProxyConfig, client *http.Client, traceStore *store.Store) *Proxy {
	p := &Proxy{
		client:   client,
		table:    NewRoutingTable(),
		breakers: NewBreakers(),
		stats:    NewStats(),
		traces:   NewTraceCollector(traceStore, defaultRecentSize, defaultMaxKeep),
	}
	p.cfg.Store(normalizeProxyConfig(cfg))
	if !p.table.Ready() {
		p.warming.Store(true)
	}
	p.traces.Start()
	return p
}

func (p *Proxy) Config() *config.ProxyConfig          { return p.cfg.Load() }
func (p *Proxy) Table() *RoutingTable                 { return p.table }
func (p *Proxy) Breakers() *Breakers                  { return p.breakers }
func (p *Proxy) Stats() *Stats                        { return p.stats }
func (p *Proxy) Traces() *TraceCollector              { return p.traces }
func (p *Proxy) SetWarming(v bool)                    { p.warming.Store(v) }
func (p *Proxy) UpdateConfig(cfg *config.ProxyConfig) { p.cfg.Store(normalizeProxyConfig(cfg)) }

// RebuildTable rebuilds the routing table from fresh check data.
func (p *Proxy) RebuildTable(results []store.CheckResultRow, dbProviders []store.ProviderRow, memProviders []provider.Provider, fingerprints map[[2]string]FingerprintScore, capabilities map[int64]map[string]store.CapabilityRow) {
	p.table.Rebuild(results, dbProviders, memProviders, fingerprints, capabilities)
	if p.table.Ready() {
		p.warming.Store(false)
	}
}

// authMiddleware checks the Bearer token against the configured proxy API key.
func (p *Proxy) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := p.cfg.Load()
		if cfg.APIKey == "" {
			next(w, r)
			return
		}

		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, 401, "missing_api_key", "Authorization header required")
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.APIKey)) != 1 {
			writeError(w, 401, "invalid_api_key", "Invalid API key")
			return
		}
		next(w, r)
	}
}

// HandleChatCompletions proxies POST /v1/chat/completions.
func (p *Proxy) HandleChatCompletions() http.HandlerFunc {
	return p.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, 405, "method_not_allowed", "POST required")
			return
		}
		p.proxyRequest(w, r, "chat", "/chat/completions")
	})
}

// HandleResponses proxies POST /v1/responses.
func (p *Proxy) HandleResponses() http.HandlerFunc {
	return p.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, 405, "method_not_allowed", "POST required")
			return
		}
		p.proxyRequest(w, r, "responses", "/responses")
	})
}

// HandleModels returns the aggregated model list.
func (p *Proxy) HandleModels() http.HandlerFunc {
	return p.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, 405, "method_not_allowed", "GET required")
			return
		}

		if p.warming.Load() {
			writeError(w, 503, "proxy_warming_up", "Proxy is warming up, please retry in ~30s")
			return
		}

		models := p.table.Models()
		data := make([]map[string]any, len(models))
		for i, m := range models {
			data[i] = map[string]any{
				"id":       m,
				"object":   "model",
				"owned_by": "relay-monitor",
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   data,
		})
	})
}

func (p *Proxy) proxyRequest(w http.ResponseWriter, r *http.Request, apiFormat, pathSuffix string) {
	if p.warming.Load() {
		writeError(w, 503, "proxy_warming_up", "Proxy is warming up, please retry in ~30s")
		return
	}

	cfg := p.cfg.Load()

	// Read and buffer the request body once so retries can replay it verbatim.
	body, err := readRequestBody(r, cfg.MaxRequestBodyBytes)
	if err != nil {
		if tooLarge, ok := err.(requestTooLargeError); ok {
			writeRequestTooLarge(w, tooLarge.limit, "request body", "max_request_body_bytes")
			return
		}
		writeError(w, 400, "invalid_request", "Failed to read request body")
		return
	}

	// Parse model and stream flag
	var req proxyRequestMeta
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		writeError(w, 400, "invalid_request", "Request must include a 'model' field")
		return
	}
	if apiFormat == "responses" && cfg.MaxResponsesBodyBytes > 0 && int64(len(body)) > cfg.MaxResponsesBodyBytes {
		writeRequestTooLarge(w, cfg.MaxResponsesBodyBytes, "/v1/responses request body", "max_responses_body_bytes")
		return
	}

	tools := hasTools(req.Tools)
	trace := Trace{
		ID:           newTraceID(),
		ReceivedAt:   time.Now(),
		Model:        req.Model,
		Endpoint:     apiFormat,
		Stream:       req.Stream,
		HasTools:     tools,
		ToolRequired: tools && requiresToolCall(req.ToolChoice),
	}
	defer func() {
		trace.LatencyMs = time.Since(trace.ReceivedAt).Milliseconds()
		p.traces.Emit(trace)
	}()

	// Get routing candidates with explanation data for tracing
	candidates, candidateEntries, filtered := p.table.SelectWithExplanation(req.Model, apiFormat, req.requirements(), p.stats, p.breakers)
	trace.Candidates = candidateEntries
	trace.Filtered = filtered

	if len(candidates) == 0 {
		trace.FinalStatus = "failed"
		writeError(w, 404, "model_not_found",
			fmt.Sprintf("Model '%s' is not available on any healthy provider", req.Model))
		return
	}

	maxAttempts := cfg.MaxRetries + 1
	if maxAttempts > len(candidates) {
		maxAttempts = len(candidates)
	}

	attempt := 0
	// Map provider → capability match rank so each attempt records why it ranked
	// where it did (rank is computed per-request in SelectWithExplanation and is
	// not carried on the flat candidate list).
	rankByProvider := make(map[int64]int, len(candidateEntries))
	for _, ce := range candidateEntries {
		rankByProvider[ce.ProviderID] = ce.MatchRank
	}

	for _, candidate := range candidates {
		if attempt >= maxAttempts {
			break
		}

		halfOpenProbe := false
		if p.breakers != nil && p.breakers.GetState(candidate.ProviderID, req.Model) == BreakerHalfOpen {
			if !p.breakers.AcquireProbe(candidate.ProviderID, req.Model) {
				log.Printf("[proxy] %s skipping %s: half-open probe already in progress", req.Model, candidate.ProviderName)
				continue
			}
			halfOpenProbe = true
		}

		tryIndex := attempt
		attempt++
		start := time.Now()

		// Shrink timeout on retries: full → ÷2 → ÷4
		retryCfg := cfg
		if tryIndex > 0 {
			divisor := time.Duration(2 * tryIndex) // retry 1→÷2, retry 2→÷4
			shorter := *cfg
			shorter.RequestTimeout.Duration = cfg.RequestTimeout.Duration / divisor
			shorter.StreamFirstByteTimeout.Duration = cfg.StreamFirstByteTimeout.Duration / divisor
			if shorter.RequestTimeout.Duration < 5*time.Second {
				shorter.RequestTimeout.Duration = 5 * time.Second
			}
			if shorter.StreamFirstByteTimeout.Duration < 5*time.Second {
				shorter.StreamFirstByteTimeout.Duration = 5 * time.Second
			}
			retryCfg = &shorter
		}

		upstreamURL := strings.TrimRight(candidate.BaseURL, "/") + pathSuffix
		outcome := p.forwardOne(r.Context(), retryCfg, upstreamURL, candidate.APIKey, body, req, apiFormat, w, r)
		elapsed := time.Since(start).Milliseconds()

		attemptEntry := Attempt{
			Index:        tryIndex,
			ProviderID:   candidate.ProviderID,
			ProviderName: candidate.ProviderName,
			Score:        candidate.Score,
			MatchRank:    rankByProvider[candidate.ProviderID],
			BreakerState: p.breakers.GetState(candidate.ProviderID, req.Model).String(),
			HTTPStatus:   outcome.httpStatus,
			Failure:      outcome.failure,
			LatencyMs:    elapsed,
			WroteBody:    outcome.wrote,
		}

		if outcome.result == forwardOK {
			p.breakers.RecordSuccess(candidate.ProviderID, req.Model)
			p.stats.Record(candidate.ProviderID, req.Model, elapsed, false)
			trace.Attempts = append(trace.Attempts, attemptEntry)
			trace.FinalStatus = "ok"
			return
		}

		trace.Attempts = append(trace.Attempts, attemptEntry)

		// Record stats error for all failure types
		p.stats.Record(candidate.ProviderID, req.Model, elapsed, true)

		// Only trigger circuit breaker for transient failures (5xx, 429, timeout)
		if halfOpenProbe {
			switch {
			case outcome.result == forwardRetry:
				p.breakers.RecordFailure(candidate.ProviderID, req.Model)
			case outcome.result == forwardRetryMild && !outcome.wrote:
				p.breakers.RecordFailure(candidate.ProviderID, req.Model)
			case outcome.result == forwardDone:
				p.breakers.RecordFailure(candidate.ProviderID, req.Model)
			default:
				p.breakers.ReleaseProbe(candidate.ProviderID, req.Model)
			}
		} else if outcome.result == forwardRetry {
			p.breakers.RecordFailure(candidate.ProviderID, req.Model)
		}

		if outcome.wrote {
			// Response already started (streaming), can't retry
			trace.FinalStatus = "partial"
			return
		}

		log.Printf("[proxy] %s failed on %s (attempt %d/%d), trying next",
			req.Model, candidate.ProviderName, tryIndex+1, maxAttempts)
	}

	// All attempts exhausted
	trace.FinalStatus = "failed"
	writeError(w, 502, "all_providers_failed", "All providers failed for this request")
}

type proxyRequestMeta struct {
	Model      string          `json:"model"`
	Stream     bool            `json:"stream"`
	Tools      json.RawMessage `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
}

func (r proxyRequestMeta) requirements() RequestRequirements {
	tools := hasTools(r.Tools)
	return RequestRequirements{
		NeedsStreaming: r.Stream,
		NeedsTools:     tools,
		NeedsToolCall:  tools && requiresToolCall(r.ToolChoice),
	}
}

func hasTools(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) && !bytes.Equal(trimmed, []byte("[]"))
}

func requiresToolCall(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}

	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return false
	}
	switch v := decoded.(type) {
	case string:
		return v == "required"
	case map[string]any:
		typ, _ := v["type"].(string)
		return typ == "function"
	default:
		return false
	}
}

type forwardResult int

const (
	forwardOK        forwardResult = iota
	forwardRetry                   // failed, can retry, counts as breaker failure
	forwardRetryMild               // failed, can retry, but NOT a breaker failure (e.g. 403 quota)
	forwardDone                    // failed, but response already started (streaming)
)

// forwardOutcome is the full result of one upstream attempt. It carries the
// HTTP status and failure class so the caller can populate a trace Attempt
// without re-deriving them — the status is known here and nowhere else.
type forwardOutcome struct {
	result     forwardResult
	wrote      bool         // true once bytes were written to the client (can't retry)
	httpStatus int          // upstream HTTP status; 0 if no response was received
	failure    FailureClass // FailureNone on success
}

// fallbackUserAgent is used when the downstream client sends no User-Agent,
// to avoid exposing Go-http-client/1.1 to upstream providers.
const fallbackUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func (p *Proxy) forwardOne(ctx context.Context, cfg *config.ProxyConfig, url, apiKey string, body []byte, reqMeta proxyRequestMeta, apiFormat string, w http.ResponseWriter, origReq *http.Request) forwardOutcome {
	timeout := cfg.RequestTimeout.Duration
	if reqMeta.Stream {
		timeout = cfg.StreamFirstByteTimeout.Duration
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	upstream, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return forwardOutcome{result: forwardRetry, failure: FailureUpstream5xx}
	}

	// Copy relevant headers from original request
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Authorization", "Bearer "+apiKey)
	if ua := origReq.Header.Get("User-Agent"); ua != "" {
		upstream.Header.Set("User-Agent", ua)
	} else {
		// Fallback: mimic a browser UA rather than exposing Go-http-client
		upstream.Header.Set("User-Agent", fallbackUserAgent)
	}
	if accept := origReq.Header.Get("Accept"); accept != "" {
		upstream.Header.Set("Accept", accept)
	}

	resp, err := p.client.Do(upstream)
	if err != nil {
		// Surface a timeout distinctly from other transport failures so the
		// trace shows the real reason instead of a generic upstream error.
		fc := FailureUpstream5xx
		if errors.Is(err, context.DeadlineExceeded) {
			fc = FailureTimeout
		}
		log.Printf("[proxy] upstream connect error: %s: %v", url, err)
		return forwardOutcome{result: forwardRetry, failure: fc}
	}

	// 5xx / 429 / 401 / 403 / 404 / 413: discard the body and retry the next
	// provider. classifyUpstreamFailure decides retry severity (forwardRetry
	// trips the breaker, forwardRetryMild does not) and the precise failure class.
	switch {
	case resp.StatusCode >= 500, resp.StatusCode == 429,
		resp.StatusCode == 401, resp.StatusCode == 403,
		resp.StatusCode == 404, resp.StatusCode == 413:
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		result, fc := classifyUpstreamFailure(resp.StatusCode)
		log.Printf("[proxy] upstream %d from %s, will retry next provider", resp.StatusCode, url)
		return forwardOutcome{result: result, httpStatus: resp.StatusCode, failure: fc}
	}

	// Other 4xx: a genuine client error (bad request body etc). Forward verbatim
	// and do not retry — a different provider will not help.
	if resp.StatusCode >= 400 {
		copyResponse(w, resp)
		resp.Body.Close()
		_, fc := classifyUpstreamFailure(resp.StatusCode)
		return forwardOutcome{result: forwardRetryMild, wrote: true, httpStatus: resp.StatusCode, failure: fc}
	}

	if !reqMeta.Stream || resp.StatusCode != 200 {
		if resp.StatusCode == 200 && apiFormat == "responses" {
			respBody, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				log.Printf("[proxy] failed reading upstream responses body from %s: %v", url, err)
				return forwardOutcome{result: forwardRetry, httpStatus: 200, failure: FailureUpstream5xx}
			}
			if err := validateResponsesPayload(respBody, requiresToolCall(reqMeta.ToolChoice)); err != nil {
				log.Printf("[proxy] malformed /responses payload from %s: %v", url, err)
				return forwardOutcome{result: forwardRetry, httpStatus: 200, failure: FailureProtocolError}
			}
			writeResponseBytes(w, resp, respBody)
			return forwardOutcome{result: forwardOK, wrote: true, httpStatus: 200}
		}

		copyResponse(w, resp)
		resp.Body.Close()
		return forwardOutcome{result: forwardOK, wrote: true, httpStatus: resp.StatusCode}
	}

	// Streaming (status 200): pipe SSE chunks (takes ownership of resp.Body)
	if apiFormat == "responses" {
		result, wrote, fc := p.pipeResponsesStream(w, resp, cfg, requiresToolCall(reqMeta.ToolChoice))
		return forwardOutcome{result: result, wrote: wrote, httpStatus: 200, failure: fc}
	}
	result, fc := p.pipeStream(w, resp, cfg)
	return forwardOutcome{result: result, wrote: true, httpStatus: 200, failure: fc}
}

func (p *Proxy) pipeResponsesStream(w http.ResponseWriter, resp *http.Response, cfg *config.ProxyConfig, requireToolCall bool) (forwardResult, bool, FailureClass) {
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		copyResponse(w, resp)
		return forwardOK, true, FailureNone
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	idle := time.NewTimer(cfg.StreamIdleTimeout.Duration)
	defer idle.Stop()

	var timedOut atomic.Bool
	type pipeResult struct {
		result  forwardResult
		wrote   bool
		failure FailureClass
	}
	done := make(chan pipeResult, 1)

	go func() {
		started := false
		sawFunctionCall := false
		var blockLines []string
		var dataLines []string

		writeHeaders := func() {
			if started {
				return
			}
			for k, vv := range resp.Header {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(http.StatusOK)
			started = true
		}

		emitProtocolError := func(message string) {
			writeHeaders()
			fmt.Fprintf(w, "data: {\"error\":{\"message\":%q}}\n\n", message)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		}

		// flushBlock returns (result, wrote, stop, failure).
		flushBlock := func() (forwardResult, bool, bool, FailureClass) {
			if len(blockLines) == 0 {
				return forwardOK, started, false, FailureNone
			}

			hasDone := false
			for _, payload := range dataLines {
				if payload == "[DONE]" {
					hasDone = true
					continue
				}
				facts, err := responsefmt.ValidateStreamEvent([]byte(payload))
				if err != nil {
					if !started {
						return forwardRetry, false, true, FailureProtocolError
					}
					emitProtocolError("malformed upstream responses stream")
					return forwardDone, true, true, FailureProtocolError
				}
				if facts.HasFunctionCall {
					sawFunctionCall = true
				}
			}

			if hasDone && requireToolCall && !sawFunctionCall {
				if !started {
					return forwardRetry, false, true, FailureToolMissing
				}
				emitProtocolError("required tool call missing from streamed response")
				return forwardDone, true, true, FailureToolMissing
			}

			writeHeaders()
			for _, line := range blockLines {
				if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
					return forwardDone, true, true, FailureStreamBroken
				}
			}
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return forwardDone, true, true, FailureStreamBroken
			}
			flusher.Flush()
			if hasDone {
				return forwardOK, true, true, FailureNone
			}
			return forwardOK, true, false, FailureNone
		}

		for scanner.Scan() {
			line := scanner.Text()
			idle.Reset(cfg.StreamIdleTimeout.Duration)
			if line == "" {
				result, wrote, stop, fc := flushBlock()
				blockLines = nil
				dataLines = nil
				if stop {
					done <- pipeResult{result: result, wrote: wrote, failure: fc}
					return
				}
				continue
			}

			blockLines = append(blockLines, line)
			if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}

		if len(blockLines) > 0 {
			result, wrote, stop, fc := flushBlock()
			if stop {
				done <- pipeResult{result: result, wrote: wrote, failure: fc}
				return
			}
		}

		if timedOut.Load() {
			if !started {
				done <- pipeResult{result: forwardRetry, wrote: false, failure: FailureStreamIdle}
				return
			}
			emitProtocolError("upstream timeout")
			done <- pipeResult{result: forwardDone, wrote: true, failure: FailureStreamIdle}
			return
		}
		if scanner.Err() != nil {
			if !started {
				done <- pipeResult{result: forwardRetry, wrote: false, failure: FailureStreamBroken}
				return
			}
			done <- pipeResult{result: forwardDone, wrote: true, failure: FailureStreamBroken}
			return
		}
		done <- pipeResult{result: forwardOK, wrote: started, failure: FailureNone}
	}()

	select {
	case result := <-done:
		return result.result, result.wrote, result.failure
	case <-idle.C:
		timedOut.Store(true)
		resp.Body.Close()
		result := <-done
		return result.result, result.wrote, result.failure
	}
}

func (p *Proxy) pipeStream(w http.ResponseWriter, resp *http.Response, cfg *config.ProxyConfig) (forwardResult, FailureClass) {
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		copyResponse(w, resp)
		return forwardOK, FailureNone
	}

	// Copy response headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(200)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	idle := time.NewTimer(cfg.StreamIdleTimeout.Duration)
	defer idle.Stop()

	var timedOut atomic.Bool
	done := make(chan forwardResult, 1)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
				done <- forwardDone
				return
			}
			if line == "" { // SSE event boundary
				flusher.Flush()
			}
			idle.Reset(cfg.StreamIdleTimeout.Duration)
			if strings.HasPrefix(line, "data: [DONE]") {
				done <- forwardOK
				return
			}
		}
		// Scanner loop ended — either timeout-induced body close or natural EOF
		if timedOut.Load() {
			fmt.Fprintf(w, "data: {\"error\":{\"message\":\"upstream timeout\"}}\n\ndata: [DONE]\n\n")
			flusher.Flush()
			done <- forwardDone
			return
		}
		if scanner.Err() != nil {
			done <- forwardDone
			return
		}
		done <- forwardOK
	}()

	select {
	case result := <-done:
		fc := FailureNone
		if result != forwardOK {
			fc = FailureStreamBroken
		}
		return result, fc
	case <-idle.C:
		// Upstream went silent mid-stream; signal timeout and close body to unblock scanner
		timedOut.Store(true)
		resp.Body.Close()
		return <-done, FailureStreamIdle
	}
}

func copyResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func writeResponseBytes(w http.ResponseWriter, resp *http.Response, body []byte) {
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func validateResponsesPayload(body []byte, requireToolCall bool) error {
	return responsefmt.ValidatePayload(body, requireToolCall)
}

type requestTooLargeError struct {
	limit int64
}

func (e requestTooLargeError) Error() string {
	return fmt.Sprintf("request body exceeds limit %d bytes", e.limit)
}

func normalizeProxyConfig(cfg *config.ProxyConfig) *config.ProxyConfig {
	if cfg == nil {
		cfg = &config.ProxyConfig{}
	}
	clone := *cfg
	if clone.MaxRequestBodyBytes <= 0 {
		clone.MaxRequestBodyBytes = defaultMaxRequestBodyBytes
	}
	if clone.MaxResponsesBodyBytes <= 0 {
		clone.MaxResponsesBodyBytes = defaultMaxResponsesBodyBytes
	}
	return &clone
}

func readRequestBody(r *http.Request, limit int64) ([]byte, error) {
	if limit > 0 && r.ContentLength > limit {
		return nil, requestTooLargeError{limit: limit}
	}
	if limit <= 0 {
		return io.ReadAll(r.Body)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, requestTooLargeError{limit: limit}
	}
	return body, nil
}

func writeRequestTooLarge(w http.ResponseWriter, limit int64, scope, configKey string) {
	writeError(
		w,
		http.StatusRequestEntityTooLarge,
		"request_too_large",
		fmt.Sprintf(
			"%s exceeds relay budget (%s via proxy.%s). Start a fresh session, compact the conversation, or raise the limit.",
			scope,
			formatBytes(limit),
			configKey,
		),
	)
}

func formatBytes(n int64) string {
	const (
		kib = int64(1024)
		mib = 1024 * kib
	)
	switch {
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func writeError(w http.ResponseWriter, code int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
		},
	})
}
