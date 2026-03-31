package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
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
	warming  atomic.Bool // true while initial check is running
}

// New creates a Proxy with the given config. The routing table starts empty.
func New(cfg *config.ProxyConfig, client *http.Client) *Proxy {
	p := &Proxy{
		client:   client,
		table:    NewRoutingTable(),
		breakers: NewBreakers(),
		stats:    NewStats(),
	}
	p.cfg.Store(cfg)
	if !p.table.Ready() {
		p.warming.Store(true)
	}
	return p
}

func (p *Proxy) Config() *config.ProxyConfig          { return p.cfg.Load() }
func (p *Proxy) Table() *RoutingTable                 { return p.table }
func (p *Proxy) Breakers() *Breakers                  { return p.breakers }
func (p *Proxy) Stats() *Stats                        { return p.stats }
func (p *Proxy) SetWarming(v bool)                    { p.warming.Store(v) }
func (p *Proxy) UpdateConfig(cfg *config.ProxyConfig) { p.cfg.Store(cfg) }

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

	// Read and buffer the request body
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB limit
	if err != nil {
		writeError(w, 400, "invalid_request", "Failed to read request body")
		return
	}

	// Parse model and stream flag
	var req proxyRequestMeta
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		writeError(w, 400, "invalid_request", "Request must include a 'model' field")
		return
	}

	// Get routing candidates
	candidates := p.table.Select(req.Model, apiFormat, req.requirements(), p.stats, p.breakers)
	if len(candidates) == 0 {
		writeError(w, 404, "model_not_found",
			fmt.Sprintf("Model '%s' is not available on any healthy provider", req.Model))
		return
	}

	maxAttempts := cfg.MaxRetries + 1
	if maxAttempts > len(candidates) {
		maxAttempts = len(candidates)
	}

	attempt := 0
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
		result, done := p.forwardOne(r.Context(), retryCfg, upstreamURL, candidate.APIKey, body, req, apiFormat, w, r)
		elapsed := time.Since(start).Milliseconds()

		if result == forwardOK {
			p.breakers.RecordSuccess(candidate.ProviderID, req.Model)
			p.stats.Record(candidate.ProviderID, req.Model, elapsed, false)
			return
		}

		// Record stats error for all failure types
		p.stats.Record(candidate.ProviderID, req.Model, elapsed, true)

		// Only trigger circuit breaker for transient failures (5xx, 429, timeout)
		if halfOpenProbe {
			switch {
			case result == forwardRetry:
				p.breakers.RecordFailure(candidate.ProviderID, req.Model)
			case result == forwardRetryMild && !done:
				p.breakers.RecordFailure(candidate.ProviderID, req.Model)
			case result == forwardDone:
				p.breakers.RecordFailure(candidate.ProviderID, req.Model)
			default:
				p.breakers.ReleaseProbe(candidate.ProviderID, req.Model)
			}
		} else if result == forwardRetry {
			p.breakers.RecordFailure(candidate.ProviderID, req.Model)
		}

		if done {
			// Response already started (streaming), can't retry
			return
		}

		log.Printf("[proxy] %s failed on %s (attempt %d/%d), trying next",
			req.Model, candidate.ProviderName, tryIndex+1, maxAttempts)
	}

	// All attempts exhausted
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

// fallbackUserAgent is used when the downstream client sends no User-Agent,
// to avoid exposing Go-http-client/1.1 to upstream providers.
const fallbackUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func (p *Proxy) forwardOne(ctx context.Context, cfg *config.ProxyConfig, url, apiKey string, body []byte, reqMeta proxyRequestMeta, apiFormat string, w http.ResponseWriter, origReq *http.Request) (forwardResult, bool) {
	timeout := cfg.RequestTimeout.Duration
	if reqMeta.Stream {
		timeout = cfg.StreamFirstByteTimeout.Duration
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	upstream, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return forwardRetry, false
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
		log.Printf("[proxy] upstream connect error: %s: %v", url, err)
		return forwardRetry, false
	}

	// 5xx, 429 (rate limit): transient failure, retry on next provider
	if resp.StatusCode >= 500 || resp.StatusCode == 429 {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		log.Printf("[proxy] upstream %d from %s", resp.StatusCode, url)
		return forwardRetry, false
	}

	// 401 (auth failed), 403 (quota exhausted), 404 (model gone):
	// upstream-side issues, retry on next provider without triggering breaker
	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		log.Printf("[proxy] upstream %d from %s, will retry next provider", resp.StatusCode, url)
		return forwardRetryMild, false
	}

	// Other 4xx: client error (bad request body etc), don't retry, forward as-is
	if resp.StatusCode >= 400 {
		copyResponse(w, resp)
		resp.Body.Close()
		return forwardRetryMild, true // done=true because we wrote the response
	}

	if !reqMeta.Stream || resp.StatusCode != 200 {
		if resp.StatusCode == 200 && apiFormat == "responses" {
			respBody, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				log.Printf("[proxy] failed reading upstream responses body from %s: %v", url, err)
				return forwardRetry, false
			}
			if err := validateResponsesPayload(respBody, requiresToolCall(reqMeta.ToolChoice)); err != nil {
				log.Printf("[proxy] malformed /responses payload from %s: %v", url, err)
				return forwardRetry, false
			}
			writeResponseBytes(w, resp, respBody)
			return forwardOK, true
		}

		copyResponse(w, resp)
		resp.Body.Close()
		return forwardOK, true
	}

	// Streaming: pipe SSE chunks (takes ownership of resp.Body)
	if apiFormat == "responses" {
		return p.pipeResponsesStream(w, resp, cfg, requiresToolCall(reqMeta.ToolChoice))
	}
	return p.pipeStream(w, resp, cfg), true
}

func (p *Proxy) pipeResponsesStream(w http.ResponseWriter, resp *http.Response, cfg *config.ProxyConfig, requireToolCall bool) (forwardResult, bool) {
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		copyResponse(w, resp)
		return forwardOK, true
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	idle := time.NewTimer(cfg.StreamIdleTimeout.Duration)
	defer idle.Stop()

	var timedOut atomic.Bool
	done := make(chan struct {
		result forwardResult
		wrote  bool
	}, 1)

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

		flushBlock := func() (forwardResult, bool, bool) {
			if len(blockLines) == 0 {
				return forwardOK, started, false
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
						return forwardRetry, false, true
					}
					emitProtocolError("malformed upstream responses stream")
					return forwardDone, true, true
				}
				if facts.HasFunctionCall {
					sawFunctionCall = true
				}
			}

			if hasDone && requireToolCall && !sawFunctionCall {
				if !started {
					return forwardRetry, false, true
				}
				emitProtocolError("required tool call missing from streamed response")
				return forwardDone, true, true
			}

			writeHeaders()
			for _, line := range blockLines {
				if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
					return forwardDone, true, true
				}
			}
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return forwardDone, true, true
			}
			flusher.Flush()
			if hasDone {
				return forwardOK, true, true
			}
			return forwardOK, true, false
		}

		for scanner.Scan() {
			line := scanner.Text()
			idle.Reset(cfg.StreamIdleTimeout.Duration)
			if line == "" {
				result, wrote, stop := flushBlock()
				blockLines = nil
				dataLines = nil
				if stop {
					done <- struct {
						result forwardResult
						wrote  bool
					}{result: result, wrote: wrote}
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
			result, wrote, stop := flushBlock()
			if stop {
				done <- struct {
					result forwardResult
					wrote  bool
				}{result: result, wrote: wrote}
				return
			}
		}

		if timedOut.Load() {
			if !started {
				done <- struct {
					result forwardResult
					wrote  bool
				}{result: forwardRetry, wrote: false}
				return
			}
			emitProtocolError("upstream timeout")
			done <- struct {
				result forwardResult
				wrote  bool
			}{result: forwardDone, wrote: true}
			return
		}
		if scanner.Err() != nil {
			if !started {
				done <- struct {
					result forwardResult
					wrote  bool
				}{result: forwardRetry, wrote: false}
				return
			}
			done <- struct {
				result forwardResult
				wrote  bool
			}{result: forwardDone, wrote: true}
			return
		}
		done <- struct {
			result forwardResult
			wrote  bool
		}{result: forwardOK, wrote: started}
	}()

	select {
	case result := <-done:
		return result.result, result.wrote
	case <-idle.C:
		timedOut.Store(true)
		resp.Body.Close()
		result := <-done
		return result.result, result.wrote
	}
}

func (p *Proxy) pipeStream(w http.ResponseWriter, resp *http.Response, cfg *config.ProxyConfig) forwardResult {
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		copyResponse(w, resp)
		return forwardOK
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
		return result
	case <-idle.C:
		// Upstream went silent mid-stream; signal timeout and close body to unblock scanner
		timedOut.Store(true)
		resp.Body.Close()
		return <-done
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
