package checker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"relay-monitor/internal/clientprofile"
	"relay-monitor/internal/provider"
)

func TestChatAutoUsesCodexResponsesProfile(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s, want /v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); got != clientprofile.DefaultCodexUserAgent {
			t.Fatalf("User-Agent = %q, want %q", got, clientprofile.DefaultCodexUserAgent)
		}
		if got := r.Header.Get("originator"); got != "codex_cli_rs" {
			t.Fatalf("originator = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "gpt-5.6-sol" || body["store"] != false {
			t.Fatalf("unexpected request body: %#v", body)
		}
		if _, exists := body["temperature"]; exists {
			t.Fatalf("Responses probe must not send optional temperature: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"126"}]}]}`))
	}))
	defer upstream.Close()

	resp, err := Chat(context.Background(), upstream.Client(), upstream.URL+"/v1", "key", "gpt-5.6-sol", "test", ChatOptions{Profile: clientprofile.Config{Mode: provider.ClientModeAuto}})
	if err != nil || resp == nil || !resp.OK {
		t.Fatalf("Chat = %#v, err = %v", resp, err)
	}
	if resp.Content != "126" {
		t.Fatalf("content = %q, want 126", resp.Content)
	}
}

func TestChatAutoUsesClaudeMessagesProfile(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s, want /v1/messages", r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); got != clientprofile.DefaultClaudeCodeUserAgent {
			t.Fatalf("User-Agent = %q, want %q", got, clientprofile.DefaultClaudeCodeUserAgent)
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") == "" {
			t.Fatalf("missing Anthropic headers: %#v", r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["temperature"]; exists {
			t.Fatalf("Anthropic probe must not send optional temperature: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","content":[{"type":"thinking","thinking":"reason"},{"type":"text","text":"126"}]}`))
	}))
	defer upstream.Close()

	resp, err := Chat(context.Background(), upstream.Client(), upstream.URL+"/v1", "key", "sonnet5", "test", ChatOptions{Profile: clientprofile.Config{Mode: provider.ClientModeAuto}})
	if err != nil || resp == nil || !resp.OK {
		t.Fatalf("Chat = %#v, err = %v", resp, err)
	}
	if resp.Content != "126" || resp.Reasoning != "reason" {
		t.Fatalf("content/reasoning = %q/%q", resp.Content, resp.Reasoning)
	}
}

func TestChatGenericOmitsTemperature(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["temperature"]; exists {
			t.Fatalf("Chat probe must not send optional temperature: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"126"}}]}`))
	}))
	defer upstream.Close()

	resp, err := Chat(context.Background(), upstream.Client(), upstream.URL, "key", "model", "test", ChatOptions{})
	if err != nil || resp == nil || !resp.OK || resp.Content != "126" {
		t.Fatalf("Chat = %#v, err = %v", resp, err)
	}
}

func TestDiagnoseErrorPreserves403PolicyMessage(t *testing.T) {
	got := DiagnoseError(http.StatusForbidden, `{"error":{"code":"codex_model_required","message":"codex clients may only request codex series models"}}`, "sk-test")
	if !strings.Contains(got, "codex clients may only request codex series models") {
		t.Fatalf("diagnostic lost policy message: %q", got)
	}
}

func TestAnthropicCapabilityProbesUseMessagesProtocol(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("User-Agent") != clientprofile.DefaultClaudeCodeUserAgent {
			t.Fatalf("unexpected request: %s %#v", r.URL.Path, r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"tool_use","id":"tool_1","name":"get_weather","input":{"city":"Beijing"}}]}`))
	}))
	defer upstream.Close()

	profile := clientprofile.Config{Mode: provider.ClientModeClaudeCode}
	toolUse := probeAnthropicToolUse(context.Background(), upstream.Client(), upstream.URL+"/v1", "key", "sonnet5", profile)
	if toolUse == nil || !*toolUse {
		t.Fatalf("tool use = %v", toolUse)
	}
	streaming := probeAnthropicStreaming(context.Background(), upstream.Client(), upstream.URL+"/v1", "key", "sonnet5", profile)
	if streaming == nil || !*streaming {
		t.Fatalf("streaming = %v", streaming)
	}
}

func TestFetchModelsAutoMergesCodexAndClaudeCatalogs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("User-Agent") {
		case clientprofile.DefaultCodexUserAgent:
			w.Write([]byte(`{"data":[{"id":"gpt-5.6-sol"}]}`))
		case clientprofile.DefaultClaudeCodeUserAgent:
			w.Write([]byte(`{"data":[{"id":"sonnet5"},{"id":"gpt-5.6-sol"}]}`))
		default:
			t.Fatalf("unexpected User-Agent: %q", r.Header.Get("User-Agent"))
		}
	}))
	defer upstream.Close()

	engine := &Engine{Client: upstream.Client()}
	models, err := engine.FetchModels(context.Background(), upstream.URL+"/v1", "key", clientprofile.Config{Mode: provider.ClientModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gpt-5.6-sol", "sonnet5"}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Fatalf("models = %v, want %v", models, want)
		}
	}
}

func TestResponsesBasicProbeOmitsTemperature(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["temperature"]; exists {
			t.Fatalf("capability probe sent unsupported temperature: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}`))
	}))
	defer upstream.Close()

	got := probeResponsesBasic(context.Background(), upstream.Client(), upstream.URL, "key", "gpt-5.5", clientprofile.Config{Mode: clientprofile.ModeCodex})
	if got == nil || !*got {
		t.Fatalf("probe result = %v", got)
	}
}

// Relay stations are untrusted; the checker must not buffer an unbounded
// response body. A body past the cap is read truncated, so a huge "valid"
// payload comes back as an error instead of being swallowed whole into memory.
func TestChatCapsOversizedResponseBody(t *testing.T) {
	huge := `{"choices":[{"message":{"content":"` + strings.Repeat("x", maxUpstreamBodyBytes) + `"}}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(huge))
	}))
	defer upstream.Close()

	r, err := Chat(context.Background(), upstream.Client(), upstream.URL, "key", "gpt-test", "hi", ChatOptions{})
	if err != nil {
		t.Fatalf("Chat returned transport error: %v", err)
	}
	if r.OK {
		t.Fatal("oversized body should not parse as a successful response")
	}
}
