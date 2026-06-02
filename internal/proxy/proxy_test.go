package proxy

import (
	"encoding/json"
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
