package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relay-monitor/internal/config"
	"relay-monitor/internal/provider"
	"relay-monitor/internal/store"
)

func TestValidateResponsesPayloadRejectsMissingText(t *testing.T) {
	body := []byte(`{
		"output": [
			{
				"type": "message",
				"content": [
					{"type": "output_text"}
				]
			}
		]
	}`)

	if err := validateResponsesPayload(body, false); err == nil {
		t.Fatal("expected missing text to be rejected")
	}
}

func TestValidateResponsesPayloadRejectsMissingRequiredToolCall(t *testing.T) {
	body := []byte(`{
		"output": [
			{
				"type": "message",
				"content": [
					{"type": "output_text", "text": "hello"}
				]
			}
		]
	}`)

	if err := validateResponsesPayload(body, true); err == nil {
		t.Fatal("expected required tool call to be rejected")
	}
}

func TestValidateResponsesPayloadAcceptsValidResponsesBody(t *testing.T) {
	body := []byte(`{
		"output": [
			{
				"type": "message",
				"content": [
					{"type": "output_text", "text": "hello"}
				]
			}
		]
	}`)

	if err := validateResponsesPayload(body, false); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidateResponsesPayloadAcceptsRequiredToolCall(t *testing.T) {
	body := []byte(`{
		"output": [
			{
				"type": "function_call",
				"name": "get_weather"
			}
		]
	}`)

	if err := validateResponsesPayload(body, true); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestPipeResponsesStreamRetriesBeforeWritingOnMalformedFirstEvent(t *testing.T) {
	p := &Proxy{}
	cfg := &config.ProxyConfig{
		StreamIdleTimeout: config.Duration{Duration: time.Second},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_item.added\",\"item\":{}}\n\n")),
	}
	rec := httptest.NewRecorder()

	result, wrote, fc := p.pipeResponsesStream(rec, resp, cfg, false)
	if result != forwardRetry {
		t.Fatalf("result = %v, want %v", result, forwardRetry)
	}
	if fc != FailureProtocolError {
		t.Errorf("failure = %s, want %s", fc, FailureProtocolError)
	}
	if wrote {
		t.Fatal("expected no bytes to be written before retrying malformed first event")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
}

func TestPipeResponsesStreamRetriesBeforeWritingOnUpstreamErrorEvent(t *testing.T) {
	p := &Proxy{}
	cfg := &config.ProxyConfig{
		StreamIdleTimeout: config.Duration{Duration: time.Second},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"error\",\"error\":{\"message\":\"upstream exploded\"}}\n\n")),
	}
	rec := httptest.NewRecorder()

	result, wrote, _ := p.pipeResponsesStream(rec, resp, cfg, false)
	if result != forwardRetry {
		t.Fatalf("result = %v, want %v", result, forwardRetry)
	}
	if wrote {
		t.Fatal("expected no bytes to be written before retrying upstream error event")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
}

func TestPipeResponsesStreamEmitsErrorWhenRequiredToolCallNeverArrives(t *testing.T) {
	p := &Proxy{}
	cfg := &config.ProxyConfig{
		StreamIdleTimeout: config.Duration{Duration: time.Second},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"data: {\"type\":\"response.created\"}",
			"",
			"data: [DONE]",
			"",
		}, "\n"))),
	}
	rec := httptest.NewRecorder()

	result, wrote, fc := p.pipeResponsesStream(rec, resp, cfg, true)
	if result != forwardDone {
		t.Fatalf("result = %v, want %v", result, forwardDone)
	}
	if fc != FailureToolMissing {
		t.Errorf("failure = %s, want %s", fc, FailureToolMissing)
	}
	if !wrote {
		t.Fatal("expected streamed protocol error to be written")
	}
	if !strings.Contains(rec.Body.String(), "required tool call missing") {
		t.Fatalf("body = %q, want required tool call error", rec.Body.String())
	}
}

func TestPipeResponsesStreamAcceptsMinimalOutputTextDoneEvent(t *testing.T) {
	p := &Proxy{}
	cfg := &config.ProxyConfig{
		StreamIdleTimeout: config.Duration{Duration: time.Second},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress"}}`,
			"",
			`data: {"type":"response.output_item.added","item":{"content":[],"id":"msg_1","role":"assistant","status":"in_progress","type":"message"},"output_index":0}`,
			"",
			`data: {"type":"response.content_part.added","content_index":0,"item_id":"msg_1","output_index":0,"part":{"annotations":[],"text":"","type":"output_text"}}`,
			"",
			`data: {"type":"response.output_text.delta","content_index":0,"delta":"This","item_id":"msg_1","output_index":0}`,
			"",
			`data: {"type":"response.output_text.delta","content_index":0,"delta":" works","item_id":"msg_1","output_index":0}`,
			"",
			`data: {"type":"response.output_text.done","content_index":0,"item_id":"msg_1","output_index":0}`,
			"",
			`data: {"type":"response.content_part.done","content_index":0,"item_id":"msg_1","output_index":0}`,
			"",
			`data: {"type":"response.output_item.done","item":{"content":[{"annotations":[],"text":"This works","type":"output_text"}],"id":"msg_1","role":"assistant","status":"completed","type":"message"},"output_index":0}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","output":[{"content":[{"annotations":[],"text":"This works","type":"output_text"}],"id":"msg_1","role":"assistant","status":"completed","type":"message"}],"status":"completed"}}`,
			"",
			`data: [DONE]`,
			"",
		}, "\n"))),
	}
	rec := httptest.NewRecorder()

	result, wrote, _ := p.pipeResponsesStream(rec, resp, cfg, false)
	if result != forwardOK {
		t.Fatalf("result = %v, want %v", result, forwardOK)
	}
	if !wrote {
		t.Fatal("expected stream bytes to be written")
	}
	if strings.Contains(rec.Body.String(), "malformed upstream responses stream") {
		t.Fatalf("body = %q, want minimal done event to pass validation", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `response.output_text.done`) {
		t.Fatalf("body = %q, want streamed done event to be forwarded", rec.Body.String())
	}
}

func TestProxyRequestSkipsHalfOpenProviderWithProbeInProgress(t *testing.T) {
	var probeHits atomic.Int32
	var healthyHits atomic.Int32

	probeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeHits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"output": []map[string]any{
				{
					"type": "message",
					"content": []map[string]any{
						{"type": "output_text", "text": "probe"},
					},
				},
			},
		})
	}))
	defer probeUpstream.Close()

	healthyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyHits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"output": []map[string]any{
				{
					"type": "message",
					"content": []map[string]any{
						{"type": "output_text", "text": "healthy"},
					},
				},
			},
		})
	}))
	defer healthyUpstream.Close()

	cfg := &config.ProxyConfig{
		RequestTimeout:         config.Duration{Duration: 2 * time.Second},
		StreamFirstByteTimeout: config.Duration{Duration: 2 * time.Second},
		StreamIdleTimeout:      config.Duration{Duration: 2 * time.Second},
		MaxRetries:             1,
	}
	p := New(cfg, &http.Client{Timeout: 2 * time.Second}, nil)
	t.Cleanup(func() { p.Traces().Stop() })

	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "probe", Model: "gpt-5.4", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 10},
		{ProviderID: 2, ProviderName: "healthy", Model: "gpt-5.4", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 50},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "probe", BaseURL: probeUpstream.URL, Status: "ok", Health: 100, APIFormat: "responses"},
		{ID: 2, Name: "healthy", BaseURL: healthyUpstream.URL, Status: "ok", Health: 100, APIFormat: "responses"},
	}
	memProviders := []provider.Provider{
		{Name: "probe", BaseURL: probeUpstream.URL, APIKey: "k1"},
		{Name: "healthy", BaseURL: healthyUpstream.URL, APIKey: "k2"},
	}
	trueVal := true
	caps := map[int64]map[string]store.CapabilityRow{
		1: {
			"gpt-5.4": {
				ProviderID:     1,
				Model:          "gpt-5.4",
				ResponsesBasic: &trueVal,
			},
		},
		2: {
			"gpt-5.4": {
				ProviderID:     2,
				Model:          "gpt-5.4",
				ResponsesBasic: &trueVal,
			},
		},
	}
	p.RebuildTable(results, dbProviders, memProviders, nil, caps)

	p.breakers.ForceState(1, "gpt-5.4", BreakerHalfOpen)
	if !p.breakers.AcquireProbe(1, "gpt-5.4") {
		t.Fatal("expected to acquire the half-open probe slot for setup")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.proxyRequest(rec, req, "responses", "/responses")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if probeHits.Load() != 0 {
		t.Fatalf("half-open provider should be skipped while probe is in progress, got %d hits", probeHits.Load())
	}
	if healthyHits.Load() != 1 {
		t.Fatalf("healthy fallback hits = %d, want 1", healthyHits.Load())
	}
}

func TestProxyRequestRejectsRequestBodyOverHardLimit(t *testing.T) {
	cfg := &config.ProxyConfig{
		MaxRequestBodyBytes:    64,
		MaxResponsesBodyBytes:  64,
		RequestTimeout:         config.Duration{Duration: time.Second},
		StreamFirstByteTimeout: config.Duration{Duration: time.Second},
		StreamIdleTimeout:      config.Duration{Duration: time.Second},
	}
	p := New(cfg, &http.Client{Timeout: time.Second}, nil)
	t.Cleanup(func() { p.Traces().Stop() })
	p.SetWarming(false)

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"gpt-5.4","input":"`+strings.Repeat("x", 128)+`"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.proxyRequest(rec, req, "responses", "/responses")

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "max_request_body_bytes") {
		t.Fatalf("body = %q, want max_request_body_bytes hint", rec.Body.String())
	}
}

func TestProxyRequestRejectsResponsesBodyOverResponsesBudget(t *testing.T) {
	cfg := &config.ProxyConfig{
		MaxRequestBodyBytes:    1024,
		MaxResponsesBodyBytes:  96,
		RequestTimeout:         config.Duration{Duration: time.Second},
		StreamFirstByteTimeout: config.Duration{Duration: time.Second},
		StreamIdleTimeout:      config.Duration{Duration: time.Second},
	}
	p := New(cfg, &http.Client{Timeout: time.Second}, nil)
	t.Cleanup(func() { p.Traces().Stop() })
	p.SetWarming(false)

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"gpt-5.4","input":"`+strings.Repeat("y", 128)+`"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.proxyRequest(rec, req, "responses", "/responses")

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "max_responses_body_bytes") {
		t.Fatalf("body = %q, want max_responses_body_bytes hint", rec.Body.String())
	}
}

func TestProxyRequestRetriesNextProviderOnUpstream413(t *testing.T) {
	var smallCtxHits atomic.Int32
	var largeCtxHits atomic.Int32

	smallCtxUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		smallCtxHits.Add(1)
		http.Error(w, `{"error":{"message":"context too large","type":"request_too_large"}}`, http.StatusRequestEntityTooLarge)
	}))
	defer smallCtxUpstream.Close()

	largeCtxUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		largeCtxHits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"output": []map[string]any{
				{
					"type": "message",
					"content": []map[string]any{
						{"type": "output_text", "text": "healthy"},
					},
				},
			},
		})
	}))
	defer largeCtxUpstream.Close()

	cfg := &config.ProxyConfig{
		MaxRequestBodyBytes:    1024,
		MaxResponsesBodyBytes:  1024,
		RequestTimeout:         config.Duration{Duration: 2 * time.Second},
		StreamFirstByteTimeout: config.Duration{Duration: 2 * time.Second},
		StreamIdleTimeout:      config.Duration{Duration: 2 * time.Second},
		MaxRetries:             1,
	}
	p := New(cfg, &http.Client{Timeout: 2 * time.Second}, nil)
	t.Cleanup(func() { p.Traces().Stop() })

	results := []store.CheckResultRow{
		{ProviderID: 1, ProviderName: "small", Model: "gpt-5.4", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 10},
		{ProviderID: 2, ProviderName: "large", Model: "gpt-5.4", Vendor: "openai", Status: "ok", Correct: true, LatencyMs: 50},
	}
	dbProviders := []store.ProviderRow{
		{ID: 1, Name: "small", BaseURL: smallCtxUpstream.URL, Status: "ok", Health: 100, APIFormat: "responses"},
		{ID: 2, Name: "large", BaseURL: largeCtxUpstream.URL, Status: "ok", Health: 100, APIFormat: "responses"},
	}
	memProviders := []provider.Provider{
		{Name: "small", BaseURL: smallCtxUpstream.URL, APIKey: "k1"},
		{Name: "large", BaseURL: largeCtxUpstream.URL, APIKey: "k2"},
	}
	trueVal := true
	caps := map[int64]map[string]store.CapabilityRow{
		1: {
			"gpt-5.4": {
				ProviderID:     1,
				Model:          "gpt-5.4",
				ResponsesBasic: &trueVal,
			},
		},
		2: {
			"gpt-5.4": {
				ProviderID:     2,
				Model:          "gpt-5.4",
				ResponsesBasic: &trueVal,
			},
		},
	}
	p.RebuildTable(results, dbProviders, memProviders, nil, caps)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.proxyRequest(rec, req, "responses", "/responses")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Weighted selection may pick either provider first. What matters:
	// the request succeeded and the large-context provider ultimately handled it.
	if largeCtxHits.Load() < 1 {
		t.Fatalf("large context provider hits = %d, want >= 1", largeCtxHits.Load())
	}
	if !strings.Contains(rec.Body.String(), "healthy") {
		t.Fatalf("body = %q, want healthy upstream payload", rec.Body.String())
	}
}

func TestProxyForwardsCodexClientIdentity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "codex_cli_rs/9.9.9" {
			t.Fatalf("User-Agent = %q", got)
		}
		if got := r.Header.Get("originator"); got != "future_codex" {
			t.Fatalf("originator = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer upstream.Close()

	p := New(newTestProxyCfg(), upstream.Client(), nil)
	defer p.Traces().Stop()
	p.RebuildTable(
		[]store.CheckResultRow{{ProviderID: 1, ProviderName: "codex", Model: "gpt-5.6-sol", Vendor: "GPT", Status: "ok", Correct: true, LatencyMs: 10}},
		[]store.ProviderRow{{ID: 1, Name: "codex", BaseURL: upstream.URL, Status: "ok", Health: 100, APIFormat: "chat"}},
		[]provider.Provider{{Name: "codex", BaseURL: upstream.URL, APIKey: "key", ClientMode: provider.ClientModeCodex, CodexUserAgent: "codex_cli_rs/9.9.9", CodexOriginator: "future_codex"}},
		nil,
		nil,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.proxyRequest(rec, req, "responses", "/responses")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// A streaming response that keeps producing chunks must not be cut off by
// stream_first_byte_timeout: that timeout bounds time-to-first-byte only, not
// the total stream duration (stream_idle_timeout guards mid-stream silence).
func TestProxyStreamOutlivesFirstByteTimeout(t *testing.T) {
	const chunks = 8
	const chunkInterval = 60 * time.Millisecond // total ~480ms >> 150ms first-byte timeout

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"chunk-%d\"}}]}\n\n", i)
			flusher.Flush()
			time.Sleep(chunkInterval)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	cfg := &config.ProxyConfig{
		RequestTimeout:         config.Duration{Duration: 150 * time.Millisecond},
		StreamFirstByteTimeout: config.Duration{Duration: 150 * time.Millisecond},
		StreamIdleTimeout:      config.Duration{Duration: 2 * time.Second},
	}
	p := New(cfg, upstream.Client(), nil)
	t.Cleanup(func() { p.Traces().Stop() })
	p.RebuildTable(
		[]store.CheckResultRow{{ProviderID: 1, ProviderName: "streamer", Model: "gpt-5.4", Vendor: "GPT", Status: "ok", Correct: true, LatencyMs: 10}},
		[]store.ProviderRow{{ID: 1, Name: "streamer", BaseURL: upstream.URL, Status: "ok", Health: 100, APIFormat: "chat"}},
		[]provider.Provider{{Name: "streamer", BaseURL: upstream.URL, APIKey: "key"}},
		nil,
		nil,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.proxyRequest(rec, req, "chat", "/chat/completions")

	body := rec.Body.String()
	if !strings.Contains(body, fmt.Sprintf("chunk-%d", chunks-1)) {
		t.Fatalf("stream truncated before final chunk; body = %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream missing [DONE]; body = %q", body)
	}
	if strings.Contains(body, "upstream timeout") {
		t.Fatalf("stream reported spurious timeout; body = %q", body)
	}
}

// The first-byte timeout must still fire when the upstream never answers.
func TestProxyStreamFirstByteTimeoutStillFires(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold headers until the proxy gives up
	}))
	defer upstream.Close()
	defer close(release)

	cfg := &config.ProxyConfig{
		RequestTimeout:         config.Duration{Duration: 100 * time.Millisecond},
		StreamFirstByteTimeout: config.Duration{Duration: 100 * time.Millisecond},
		StreamIdleTimeout:      config.Duration{Duration: 2 * time.Second},
	}
	p := New(cfg, upstream.Client(), nil)
	t.Cleanup(func() { p.Traces().Stop() })
	p.RebuildTable(
		[]store.CheckResultRow{{ProviderID: 1, ProviderName: "silent", Model: "gpt-5.4", Vendor: "GPT", Status: "ok", Correct: true, LatencyMs: 10}},
		[]store.ProviderRow{{ID: 1, Name: "silent", BaseURL: upstream.URL, Status: "ok", Health: 100, APIFormat: "chat"}},
		[]provider.Provider{{Name: "silent", BaseURL: upstream.URL, APIKey: "key"}},
		nil,
		nil,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	start := time.Now()
	p.proxyRequest(rec, req, "chat", "/chat/completions")
	elapsed := time.Since(start)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 after first-byte timeout", rec.Code)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("first-byte timeout took %v, want ~100ms", elapsed)
	}
}

// A provider whose key is revoked (persistent 401) must trip the breaker.
// Otherwise it keeps re-earning primary traffic every time the stats window
// resets, burning one retry attempt per user request until the next check.
func TestPersistentAuthFailureTripsBreaker(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
	}))
	defer upstream.Close()

	p := New(newTestProxyCfg(), upstream.Client(), nil)
	t.Cleanup(func() { p.Traces().Stop() })
	p.RebuildTable(
		[]store.CheckResultRow{{ProviderID: 1, ProviderName: "revoked", Model: "gpt-5.4", Vendor: "GPT", Status: "ok", Correct: true, LatencyMs: 10}},
		[]store.ProviderRow{{ID: 1, Name: "revoked", BaseURL: upstream.URL, Status: "ok", Health: 100, APIFormat: "chat"}},
		[]provider.Provider{{Name: "revoked", BaseURL: upstream.URL, APIKey: "dead-key"}},
		nil,
		nil,
	)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.proxyRequest(rec, req, "chat", "/chat/completions")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("request %d: status = %d, want 502", i, rec.Code)
		}
	}

	if state := p.Breakers().GetState(1, "gpt-5.4"); state != BreakerOpen {
		t.Fatalf("breaker state after 3 auth failures = %s, want open", state)
	}
}

// A malicious/broken upstream that streams an unbounded SSE block without an
// event boundary (blank line) must not let blockLines grow without limit. The
// non-streaming /responses path already caps buffering; the streaming path must
// too. Once the accumulated block exceeds the cap the proxy aborts the stream.
func TestPipeResponsesStreamCapsUnboundedBlock(t *testing.T) {
	p := &Proxy{}
	cfg := &config.ProxyConfig{
		StreamIdleTimeout: config.Duration{Duration: 2 * time.Second},
	}
	// One data line ~1KB, repeated far past the cap, with NO blank line between
	// them, so flushBlock never runs and blockLines would grow unbounded.
	var sb strings.Builder
	line := `data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 1024) + `"}`
	for i := 0; i < maxStreamBlockBytes/1024+64; i++ {
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sb.String())),
	}
	rec := httptest.NewRecorder()

	result, _, fc := p.pipeResponsesStream(rec, resp, cfg, false)
	if result != forwardRetry {
		t.Fatalf("result = %v, want forwardRetry (aborted before writing)", result)
	}
	if fc != FailureProtocolError {
		t.Fatalf("failure = %s, want %s", fc, FailureProtocolError)
	}
}
