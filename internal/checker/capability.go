package checker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProbeToolUse tests if a model supports function calling / tool use.
func ProbeToolUse(ctx context.Context, client *http.Client, baseURL, apiKey, modelID, apiFormat string) bool {
	if apiFormat == "responses" {
		return false // Responses API has different tool format, skip for now
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	payload, _ := json.Marshal(map[string]any{
		"model":    modelID,
		"messages": []map[string]string{{"role": "user", "content": "What is the weather in Beijing?"}},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "get_weather",
					"description": "Get weather for a city",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"city": map[string]string{"type": "string"},
						},
						"required": []string{"city"},
					},
				},
			},
		},
		"max_tokens":  200,
		"temperature": 0,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}

	var result map[string]any
	if json.Unmarshal(body, &result) != nil {
		return false
	}

	choices, _ := result["choices"].([]any)
	if len(choices) == 0 {
		return false
	}
	first, _ := choices[0].(map[string]any)
	msg, _ := first["message"].(map[string]any)

	// Check for tool_calls field
	toolCalls, exists := msg["tool_calls"]
	if !exists {
		return false
	}
	calls, ok := toolCalls.([]any)
	return ok && len(calls) > 0
}

// ProbeStreaming tests if a model supports streaming responses.
func ProbeStreaming(ctx context.Context, client *http.Client, baseURL, apiKey, modelID, apiFormat string) bool {
	if apiFormat == "responses" {
		return false // Different streaming format, skip
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	payload, _ := json.Marshal(map[string]any{
		"model":       modelID,
		"messages":    []map[string]string{{"role": "user", "content": "Say hello"}},
		"stream":      true,
		"max_tokens":  50,
		"temperature": 0,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false
	}

	// Check Content-Type
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") && !strings.Contains(ct, "application/x-ndjson") {
		return false
	}

	// Try to read at least one "data:" line
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	chunk := string(buf[:n])
	return strings.Contains(chunk, "data:")
}

// ProbeCapabilities tests both tool_use and streaming for a model.
func ProbeCapabilities(ctx context.Context, client *http.Client, baseURL, apiKey, modelID, apiFormat string) (toolUse bool, streaming bool) {
	streaming = ProbeStreaming(ctx, client, baseURL, apiKey, modelID, apiFormat)
	time.Sleep(time.Second) // Brief pause between probes
	toolUse = ProbeToolUse(ctx, client, baseURL, apiKey, modelID, apiFormat)
	return
}
