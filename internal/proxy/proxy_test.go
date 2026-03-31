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

	result, wrote := p.pipeResponsesStream(rec, resp, cfg, false)
	if result != forwardRetry {
		t.Fatalf("result = %v, want %v", result, forwardRetry)
	}
	if wrote {
		t.Fatal("expected no bytes to be written before retrying malformed first event")
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

	result, wrote := p.pipeResponsesStream(rec, resp, cfg, true)
	if result != forwardDone {
		t.Fatalf("result = %v, want %v", result, forwardDone)
	}
	if !wrote {
		t.Fatal("expected streamed protocol error to be written")
	}
	if !strings.Contains(rec.Body.String(), "required tool call missing") {
		t.Fatalf("body = %q, want required tool call error", rec.Body.String())
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
	p := New(cfg, &http.Client{Timeout: 2 * time.Second})

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
