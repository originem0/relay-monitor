// Package clientprofile isolates the wire identity used for relays that only
// allow official Codex or Claude Code clients. Ordinary providers never use a
// CLI profile unless they explicitly opt in through provider.client_mode.
package clientprofile

import (
	"net/http"
	"strings"
)

const (
	ModeGeneric    = "generic"
	ModeCodex      = "codex"
	ModeClaudeCode = "claude_code"
	ModeAuto       = "auto"

	DefaultCodexUserAgent      = "codex_cli_rs/0.144.1"
	DefaultCodexOriginator     = "codex_cli_rs"
	DefaultClaudeCodeUserAgent = "claude-code/2.1.205"
	DefaultAnthropicVersion    = "2023-06-01"
	DefaultAnthropicBeta       = "claude-code-20250219"
	DefaultGenericUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// Config is deliberately provider-scoped. CLI identity is an exception for a
// small class of restricted relays, not a new default for every provider.
type Config struct {
	Mode                string `json:"mode,omitempty"`
	CodexUserAgent      string `json:"codex_user_agent,omitempty"`
	CodexOriginator     string `json:"codex_originator,omitempty"`
	ClaudeCodeUserAgent string `json:"claude_code_user_agent,omitempty"`
	AnthropicVersion    string `json:"anthropic_version,omitempty"`
	AnthropicBeta       string `json:"anthropic_beta,omitempty"`
}

func NormalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ModeGeneric:
		return ModeGeneric
	case ModeCodex:
		return ModeCodex
	case ModeClaudeCode, "claude-code", "claude":
		return ModeClaudeCode
	case ModeAuto, "cli_auto", "cc_cx":
		return ModeAuto
	default:
		return ModeGeneric
	}
}

func ValidMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ModeGeneric, ModeCodex, ModeClaudeCode, "claude-code", "claude", ModeAuto, "cli_auto", "cc_cx":
		return true
	default:
		return false
	}
}

func ResolveMode(mode, modelID string) string {
	mode = NormalizeMode(mode)
	if mode != ModeAuto {
		return mode
	}
	if IsClaudeModel(modelID) {
		return ModeClaudeCode
	}
	return ModeCodex
}

func IsClaudeModel(modelID string) bool {
	low := strings.ToLower(modelID)
	for _, marker := range []string{"claude", "sonnet", "opus", "haiku"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

func (c Config) WithMode(mode string) Config {
	c.Mode = NormalizeMode(mode)
	return c
}

func (c Config) ModeFor(modelID string) string {
	return ResolveMode(c.Mode, modelID)
}

func (c Config) ApplyHeaders(req *http.Request, modelID string) {
	switch c.ModeFor(modelID) {
	case ModeCodex:
		ua := c.CodexUserAgent
		if ua == "" {
			ua = DefaultCodexUserAgent
		}
		originator := c.CodexOriginator
		if originator == "" {
			originator = DefaultCodexOriginator
		}
		req.Header.Set("User-Agent", ua)
		req.Header.Set("originator", originator)
	case ModeClaudeCode:
		ua := c.ClaudeCodeUserAgent
		if ua == "" {
			ua = DefaultClaudeCodeUserAgent
		}
		version := c.AnthropicVersion
		if version == "" {
			version = DefaultAnthropicVersion
		}
		beta := optionalHeader(c.AnthropicBeta, DefaultAnthropicBeta)
		req.Header.Set("User-Agent", ua)
		req.Header.Set("anthropic-version", version)
		if beta != "" {
			req.Header.Set("anthropic-beta", beta)
		}
	default:
		req.Header.Set("User-Agent", DefaultGenericUserAgent)
	}
}

func optionalHeader(value, fallback string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "none") || value == "-" {
		return ""
	}
	if value == "" {
		return fallback
	}
	return value
}
