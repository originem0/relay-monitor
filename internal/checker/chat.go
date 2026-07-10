package checker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"relay-monitor/internal/clientprofile"
	"relay-monitor/internal/provider"
)

// ChatOptions controls the API call format and parameters.
type ChatOptions struct {
	APIFormat string // "chat" (default) or "responses"
	Profile   clientprofile.Config
	MaxTokens int
}

func (o ChatOptions) format(modelID string) string {
	switch o.Profile.ModeFor(modelID) {
	case provider.ClientModeCodex:
		return "responses"
	case provider.ClientModeClaudeCode:
		return "anthropic"
	}
	if o.APIFormat == "responses" {
		return "responses"
	}
	return "chat"
}

func (o ChatOptions) maxTokens() int {
	if o.MaxTokens > 0 {
		return o.MaxTokens
	}
	return 200
}

// Chat sends a single prompt and returns a normalized ChatResponse.
// It supports both the /chat/completions and /responses API formats.
func Chat(ctx context.Context, client *http.Client, baseURL, apiKey, modelID, prompt string, opts ChatOptions) (*ChatResponse, error) {
	// Per-request timeout for checker calls (proxy handles its own timeouts)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	base := strings.TrimRight(baseURL, "/")
	var url string
	var payload []byte
	var err error
	format := opts.format(modelID)

	switch format {
	case "responses":
		url = base + "/responses"
		payload, err = json.Marshal(map[string]any{
			"model": modelID,
			"input": []map[string]string{{"role": "user", "content": prompt}},
			"store": false,
		})
	case "anthropic":
		url = base + "/messages"
		payload, err = json.Marshal(map[string]any{
			"model":      modelID,
			"messages":   []map[string]string{{"role": "user", "content": prompt}},
			"max_tokens": opts.maxTokens(),
		})
	default:
		url = base + "/chat/completions"
		payload, err = json.Marshal(map[string]any{
			"model":      modelID,
			"messages":   []map[string]string{{"role": "user", "content": prompt}},
			"max_tokens": opts.maxTokens(),
		})
	}
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	opts.Profile.ApplyHeaders(req, modelID)

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)

	if err != nil {
		return &ChatResponse{
			OK:      false,
			Code:    0,
			Elapsed: elapsed,
			Error:   DiagnoseError(0, err.Error(), apiKey),
		}, nil
	}
	defer resp.Body.Close()

	body, tooLarge, readErr := readLimited(resp.Body)
	if readErr != nil {
		return &ChatResponse{
			OK:      false,
			Code:    resp.StatusCode,
			Elapsed: elapsed,
			Error:   DiagnoseError(resp.StatusCode, readErr.Error(), apiKey),
		}, nil
	}
	if tooLarge {
		return &ChatResponse{
			OK:      false,
			Code:    resp.StatusCode,
			Elapsed: elapsed,
			Error:   errBodyTooLarge.Error(),
		}, nil
	}
	bodyStr := string(body)

	if resp.StatusCode != http.StatusOK {
		return &ChatResponse{
			OK:      false,
			Code:    resp.StatusCode,
			Elapsed: elapsed,
			Error:   DiagnoseError(resp.StatusCode, bodyStr, apiKey),
		}, nil
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return &ChatResponse{
			OK:      false,
			Code:    resp.StatusCode,
			Elapsed: elapsed,
			Error:   fmt.Sprintf("JSON parse failed: %v", err),
		}, nil
	}

	var content, reasoning string
	switch format {
	case "responses":
		content, reasoning = parseResponsesOutput(raw)
	case "anthropic":
		content, reasoning = parseAnthropicOutput(raw)
	default:
		content, reasoning = parseChatOutput(raw)
	}

	if content == "" && reasoning != "" {
		content = reasoning
	}

	return &ChatResponse{
		OK:        true,
		Content:   content,
		Reasoning: reasoning,
		Code:      resp.StatusCode,
		Elapsed:   elapsed,
	}, nil
}

// parseAnthropicOutput extracts text and thinking blocks from a Messages API
// response while keeping the rest of the checker protocol-agnostic.
func parseAnthropicOutput(resp map[string]any) (string, string) {
	var content, reasoning string
	parts, _ := resp["content"].([]any)
	for _, p := range parts {
		part, _ := p.(map[string]any)
		switch strVal(part, "type") {
		case "text":
			content += strVal(part, "text")
		case "thinking":
			reasoning += strVal(part, "thinking")
		}
	}
	return strings.TrimSpace(content), strings.TrimSpace(reasoning)
}

// parseChatOutput extracts content and reasoning from a /chat/completions response.
func parseChatOutput(resp map[string]any) (string, string) {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return "", ""
	}
	first, _ := choices[0].(map[string]any)
	msg, _ := first["message"].(map[string]any)
	content := strings.TrimSpace(strVal(msg, "content"))
	reasoning := strings.TrimSpace(strVal(msg, "reasoning_content"))
	return content, reasoning
}

// parseResponsesOutput extracts content and reasoning from a /responses response.
// Port of Python _parse_responses_output (lines 262-275).
func parseResponsesOutput(resp map[string]any) (string, string) {
	var content, reasoning string
	output, _ := resp["output"].([]any)
	for _, item := range output {
		m, _ := item.(map[string]any)
		typ, _ := m["type"].(string)
		parts, _ := m["content"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			partType, _ := part["type"].(string)
			switch {
			case typ == "message" && partType == "output_text":
				content += strVal(part, "text")
			case typ == "reasoning" && partType == "text":
				reasoning += strVal(part, "text")
			}
		}
	}
	return strings.TrimSpace(content), strings.TrimSpace(reasoning)
}

// TruncateRunes returns s truncated to at most n runes, without splitting a
// multi-byte UTF-8 character the way a raw s[:n] byte slice would. n counts
// runes, not bytes. Used for answers/errors that frequently contain CJK text.
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// strVal safely extracts a string from a map.
func strVal(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

// DiagnoseError translates an HTTP error code + body into an actionable
// diagnostic message. Port of Python _diagnose_error (lines 224-259).
func DiagnoseError(code int, body string, apiKey string) string {
	var msg string
	if body != "" && strings.HasPrefix(strings.TrimSpace(body), "{") {
		var ej map[string]any
		if json.Unmarshal([]byte(body), &ej) == nil {
			if errObj, ok := ej["error"].(map[string]any); ok {
				msg, _ = errObj["message"].(string)
			}
			if msg == "" {
				msg, _ = ej["message"].(string)
			}
		}
	}

	keyHint := ""
	if apiKey != "" && !strings.HasPrefix(apiKey, "sk-") {
		keyHint = " (Key does not start with sk-, might be a panel access_token instead of API Key)"
	}

	if code == 0 {
		s := body
		sl := strings.ToLower(s)
		switch {
		case strings.Contains(sl, "timed out") || strings.Contains(sl, "timeout"):
			return "Connection timeout: site may be down or unreachable"
		case strings.Contains(sl, "refused"):
			return "Connection refused: check if URL is correct"
		case strings.Contains(s, "SSL") || strings.Contains(sl, "certificate"):
			return "SSL certificate error: site certificate is invalid"
		case strings.Contains(sl, "reset"):
			return "Connection reset: possibly blocked by firewall"
		default:
			return fmt.Sprintf("Connection failed: %s", TruncateRunes(s, 100))
		}
	}

	truncMsg := TruncateRunes

	switch code {
	case 401:
		detail := truncMsg(msg, 80)
		if detail == "" {
			detail = "invalid token"
		}
		return fmt.Sprintf("Auth failed (401): %s%s", detail, keyHint)
	case 403:
		detail := truncMsg(msg, 160)
		if detail == "" {
			detail = truncMsg(strings.Join(strings.Fields(body), " "), 160)
		}
		if detail == "" {
			detail = "key or client profile may not be allowed for this model/endpoint"
		}
		return fmt.Sprintf("Permission denied (403): %s", detail)
	case 404:
		return "Endpoint not found (404): base_url may be incorrect"
	case 429:
		return "Rate limited (429): reduce request frequency or retry later"
	case 500, 502, 503:
		detail := truncMsg(msg, 60)
		if detail == "" {
			detail = truncMsg(body, 60)
		}
		return fmt.Sprintf("Server error (%d): %s", code, detail)
	default:
		detail := truncMsg(msg, 80)
		if detail == "" {
			detail = truncMsg(body, 80)
		}
		return fmt.Sprintf("HTTP %d: %s", code, detail)
	}
}
