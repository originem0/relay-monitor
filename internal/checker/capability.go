package checker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"relay-monitor/internal/responsefmt"
)

type CapabilityProbe struct {
	ToolUse   *bool
	Streaming *bool
}

type ResponsesCapabilities struct {
	Basic     *bool
	ToolUse   *bool
	Streaming *bool
}

// ProbeToolUse tests if a model supports function calling / tool use.
func ProbeToolUse(ctx context.Context, client *http.Client, baseURL, apiKey, modelID, apiFormat string) *bool {
	if apiFormat == "responses" {
		return probeResponsesToolUse(ctx, client, baseURL, apiKey, modelID)
	}
	return probeChatToolUse(ctx, client, baseURL, apiKey, modelID)
}

// ProbeStreaming tests if a model supports streaming responses.
func ProbeStreaming(ctx context.Context, client *http.Client, baseURL, apiKey, modelID, apiFormat string) *bool {
	if apiFormat == "responses" {
		return probeResponsesStreaming(ctx, client, baseURL, apiKey, modelID)
	}
	return probeChatStreaming(ctx, client, baseURL, apiKey, modelID)
}

// ProbeCapabilities tests both tool_use and streaming for a chat-format model.
func ProbeCapabilities(ctx context.Context, client *http.Client, baseURL, apiKey, modelID, apiFormat string) CapabilityProbe {
	streaming := ProbeStreaming(ctx, client, baseURL, apiKey, modelID, apiFormat)
	time.Sleep(time.Second) // Brief pause between probes
	toolUse := ProbeToolUse(ctx, client, baseURL, apiKey, modelID, apiFormat)
	return CapabilityProbe{
		ToolUse:   toolUse,
		Streaming: streaming,
	}
}

// ProbeResponsesCapabilities probes /responses support more precisely than a single boolean.
func ProbeResponsesCapabilities(ctx context.Context, client *http.Client, baseURL, apiKey, modelID string) ResponsesCapabilities {
	basic := probeResponsesBasic(ctx, client, baseURL, apiKey, modelID)
	if basic == nil || !*basic {
		return ResponsesCapabilities{Basic: basic}
	}

	time.Sleep(time.Second)
	streaming := probeResponsesStreaming(ctx, client, baseURL, apiKey, modelID)
	time.Sleep(time.Second)
	toolUse := probeResponsesToolUse(ctx, client, baseURL, apiKey, modelID)

	return ResponsesCapabilities{
		Basic:     basic,
		ToolUse:   toolUse,
		Streaming: streaming,
	}
}

func probeChatToolUse(ctx context.Context, client *http.Client, baseURL, apiKey, modelID string) *bool {
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
		"tool_choice": "required",
		"max_tokens":  200,
		"temperature": 0,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return classifyCapabilityStatus(resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var result map[string]any
	if json.Unmarshal(body, &result) != nil {
		return boolPtr(false)
	}

	choices, _ := result["choices"].([]any)
	if len(choices) == 0 {
		return boolPtr(false)
	}
	first, _ := choices[0].(map[string]any)
	msg, _ := first["message"].(map[string]any)
	toolCalls, exists := msg["tool_calls"]
	if !exists {
		return boolPtr(false)
	}
	calls, ok := toolCalls.([]any)
	return boolPtr(ok && len(calls) > 0)
}

func probeChatStreaming(ctx context.Context, client *http.Client, baseURL, apiKey, modelID string) *bool {
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
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return classifyCapabilityStatus(resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") && !strings.Contains(ct, "application/x-ndjson") {
		return boolPtr(false)
	}

	if strings.Contains(ct, "application/x-ndjson") {
		payload, err := readFirstNDJSON(resp.Body)
		if err != nil {
			if err == io.EOF {
				return boolPtr(false)
			}
			return nil
		}
		return boolPtr(len(payload) > 0)
	}

	firstPayload, err := readFirstSSEPayload(resp.Body)
	if err != nil {
		if err == io.EOF {
			return boolPtr(false)
		}
		return nil
	}
	return boolPtr(len(firstPayload) > 0)
}

func probeResponsesBasic(ctx context.Context, client *http.Client, baseURL, apiKey, modelID string) *bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + "/responses"
	payload, _ := json.Marshal(map[string]any{
		"model":       modelID,
		"input":       "Say hello in one short sentence.",
		"temperature": 0,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return classifyCapabilityStatus(resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	facts, err := responsefmt.InspectPayload(body, false)
	if err != nil {
		return boolPtr(false)
	}
	return boolPtr(facts.OutputItems > 0)
}

func probeResponsesToolUse(ctx context.Context, client *http.Client, baseURL, apiKey, modelID string) *bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + "/responses"
	payload, _ := json.Marshal(map[string]any{
		"model": modelID,
		"input": "What is the weather in Beijing?",
		"tools": []map[string]any{
			{
				"type":        "function",
				"name":        "get_weather",
				"description": "Get weather for a city",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]string{"type": "string"},
					},
					"required":             []string{"city"},
					"additionalProperties": false,
				},
			},
		},
		"tool_choice": "required",
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return classifyCapabilityStatus(resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	facts, err := responsefmt.InspectPayload(body, false)
	if err != nil {
		return boolPtr(false)
	}
	return boolPtr(facts.HasFunctionCall)
}

func probeResponsesStreaming(ctx context.Context, client *http.Client, baseURL, apiKey, modelID string) *bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + "/responses"
	payload, _ := json.Marshal(map[string]any{
		"model":  modelID,
		"input":  "Say hello",
		"stream": true,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return classifyCapabilityStatus(resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") && !strings.Contains(ct, "application/x-ndjson") {
		return boolPtr(false)
	}

	var (
		body    []byte
		readErr error
	)
	if strings.Contains(ct, "application/x-ndjson") {
		body, readErr = readFirstNDJSON(resp.Body)
	} else {
		body, readErr = readFirstSSEPayload(resp.Body)
	}
	if readErr != nil {
		if readErr == io.EOF {
			return boolPtr(false)
		}
		return nil
	}
	if len(body) == 0 {
		return boolPtr(false)
	}
	if _, err := responsefmt.ValidateStreamEvent(body); err != nil {
		return boolPtr(false)
	}
	return boolPtr(true)
}

func classifyCapabilityStatus(statusCode int) *bool {
	switch {
	case statusCode == http.StatusBadRequest,
		statusCode == http.StatusNotFound,
		statusCode == http.StatusMethodNotAllowed,
		statusCode == http.StatusUnsupportedMediaType,
		statusCode == http.StatusUnprocessableEntity:
		return boolPtr(false)
	case statusCode == http.StatusUnauthorized,
		statusCode == http.StatusForbidden,
		statusCode == http.StatusRequestTimeout,
		statusCode == http.StatusConflict,
		statusCode == http.StatusTooEarly,
		statusCode == http.StatusTooManyRequests,
		statusCode >= 500:
		return nil
	default:
		return nil
	}
}

func readFirstSSEPayload(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(dataLines) > 0 {
				return []byte(strings.Join(dataLines, "\n")), nil
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		dataLines = append(dataLines, payload)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(dataLines) > 0 {
		return []byte(strings.Join(dataLines, "\n")), nil
	}
	return nil, io.EOF
}

func readFirstNDJSON(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "[DONE]" {
			continue
		}
		return []byte(line), nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func boolPtr(v bool) *bool {
	b := v
	return &b
}
