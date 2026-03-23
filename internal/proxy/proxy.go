package proxy

import (
	"bufio"
	"bytes"
	"context"
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

func (p *Proxy) Config() *config.ProxyConfig    { return p.cfg.Load() }
func (p *Proxy) Table() *RoutingTable            { return p.table }
func (p *Proxy) Breakers() *Breakers             { return p.breakers }
func (p *Proxy) Stats() *Stats                   { return p.stats }
func (p *Proxy) SetWarming(v bool)               { p.warming.Store(v) }
func (p *Proxy) UpdateConfig(cfg *config.ProxyConfig) { p.cfg.Store(cfg) }

// RebuildTable rebuilds the routing table from fresh check data.
func (p *Proxy) RebuildTable(results []store.CheckResultRow, dbProviders []store.ProviderRow, memProviders []provider.Provider) {
	p.table.Rebuild(results, dbProviders, memProviders, p.breakers)
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
		if token != cfg.APIKey {
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
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		writeError(w, 400, "invalid_request", "Request must include a 'model' field")
		return
	}

	// Get routing candidates
	candidates := p.table.Select(req.Model, apiFormat)
	if len(candidates) == 0 {
		writeError(w, 404, "model_not_found",
			fmt.Sprintf("Model '%s' is not available on any healthy provider", req.Model))
		return
	}

	maxAttempts := cfg.MaxRetries + 1
	if maxAttempts > len(candidates) {
		maxAttempts = len(candidates)
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		candidate := candidates[attempt]
		start := time.Now()

		upstreamURL := strings.TrimRight(candidate.BaseURL, "/") + pathSuffix
		result, done := p.forwardOne(r.Context(), cfg, upstreamURL, candidate.APIKey, body, req.Stream, w, r)
		elapsed := time.Since(start).Milliseconds()

		if result == forwardOK {
			p.breakers.RecordSuccess(candidate.ProviderID, req.Model)
			p.stats.Record(candidate.ProviderID, req.Model, elapsed, false)
			return
		}

		p.breakers.RecordFailure(candidate.ProviderID, req.Model)
		p.stats.Record(candidate.ProviderID, req.Model, elapsed, true)

		if done {
			// Response already started (streaming), can't retry
			return
		}

		log.Printf("[proxy] %s failed on %s (attempt %d/%d), trying next",
			req.Model, candidate.ProviderName, attempt+1, maxAttempts)
	}

	// All attempts exhausted
	writeError(w, 502, "all_providers_failed", "All providers failed for this request")
}

type forwardResult int

const (
	forwardOK    forwardResult = iota
	forwardRetry               // failed, can retry (nothing sent to client)
	forwardDone                // failed, but response already started (streaming)
)

func (p *Proxy) forwardOne(ctx context.Context, cfg *config.ProxyConfig, url, apiKey string, body []byte, stream bool, w http.ResponseWriter, origReq *http.Request) (forwardResult, bool) {
	timeout := cfg.RequestTimeout.Duration
	if stream {
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
	if accept := origReq.Header.Get("Accept"); accept != "" {
		upstream.Header.Set("Accept", accept)
	}

	resp, err := p.client.Do(upstream)
	if err != nil {
		return forwardRetry, false
	}
	defer func() {
		// Only close if we haven't handed off to streaming
		if !stream || resp.StatusCode != 200 {
			resp.Body.Close()
		}
	}()

	// Non-200: retryable
	if resp.StatusCode >= 500 || resp.StatusCode == 429 {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return forwardRetry, false
	}

	// 4xx (not 429): client error, don't retry, forward as-is
	if resp.StatusCode >= 400 {
		copyResponse(w, resp)
		return forwardRetry, true // done=true because we wrote the response
	}

	if !stream || resp.StatusCode != 200 {
		// Non-streaming success or non-200 streaming: copy entire response
		copyResponse(w, resp)
		return forwardOK, true
	}

	// Streaming: pipe SSE chunks
	return p.pipeStream(w, resp, cfg), true
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

	done := make(chan error, 1)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
				done <- err
				return
			}
			if line == "" { // SSE event boundary
				flusher.Flush()
			}
			idle.Reset(cfg.StreamIdleTimeout.Duration)
			if strings.HasPrefix(line, "data: [DONE]") {
				done <- nil
				return
			}
		}
		done <- scanner.Err()
	}()

	select {
	case err := <-done:
		if err != nil {
			return forwardDone
		}
		return forwardOK
	case <-idle.C:
		// Upstream went silent mid-stream
		fmt.Fprintf(w, "data: {\"error\":{\"message\":\"upstream timeout\"}}\n\ndata: [DONE]\n\n")
		flusher.Flush()
		return forwardDone
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
