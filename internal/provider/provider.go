// Package provider defines relay provider configuration and model filtering/identification.
package provider

import (
	"strings"

	"relay-monitor/internal/clientprofile"
)

// Provider holds the configuration for a single relay provider.
type Provider struct {
	Name                string  `json:"name"`
	BaseURL             string  `json:"base_url"`
	APIKey              string  `json:"api_key"`
	AccessToken         string  `json:"access_token,omitempty"`
	APIFormat           string  `json:"api_format,omitempty"`
	ClientMode          string  `json:"client_mode,omitempty"` // generic, codex, claude_code, or auto (CC/CX by model family)
	CodexUserAgent      string  `json:"codex_user_agent,omitempty"`
	CodexOriginator     string  `json:"codex_originator,omitempty"`
	ClaudeCodeUserAgent string  `json:"claude_code_user_agent,omitempty"`
	AnthropicVersion    string  `json:"anthropic_version,omitempty"`
	AnthropicBeta       string  `json:"anthropic_beta,omitempty"`
	Platform            string  `json:"platform,omitempty"`
	LastKnownQuota      float64 `json:"last_known_quota,omitempty"`
	Pinned              bool    `json:"pinned,omitempty"`
	Note                string  `json:"note,omitempty"`
	Priority            float64 `json:"priority,omitempty"`     // routing multiplier: 0=default(1.0), 2.0=double score
	Disabled            bool    `json:"disabled,omitempty"`     // skip checks AND routing when true (full stop)
	RoutePaused         bool    `json:"route_paused,omitempty"` // skip routing only; keep probing so recovery stays monitored
}

const (
	ClientModeGeneric    = clientprofile.ModeGeneric
	ClientModeCodex      = clientprofile.ModeCodex
	ClientModeClaudeCode = clientprofile.ModeClaudeCode
	ClientModeAuto       = clientprofile.ModeAuto
)

// NormalizeClientMode keeps old provider files backward compatible: an empty
// value means the ordinary OpenAI-compatible client used before client modes
// were introduced.
func NormalizeClientMode(mode string) string {
	return clientprofile.NormalizeMode(mode)
}

func ValidClientMode(mode string) bool {
	return clientprofile.ValidMode(mode)
}

// ResolveClientMode resolves an auto CC/CX provider per model. These relays
// expose both families behind one key but enforce different wire protocols.
func ResolveClientMode(mode, modelID string) string {
	return clientprofile.ResolveMode(mode, modelID)
}

func (p Provider) ClientProfile() clientprofile.Config {
	return clientprofile.Config{
		Mode:                p.ClientMode,
		CodexUserAgent:      p.CodexUserAgent,
		CodexOriginator:     p.CodexOriginator,
		ClaudeCodeUserAgent: p.ClaudeCodeUserAgent,
		AnthropicVersion:    p.AnthropicVersion,
		AnthropicBeta:       p.AnthropicBeta,
	}
}

// vendorPattern maps a keyword (matched case-insensitively) to a vendor name.
type vendorPattern struct {
	keyword string
	vendor  string
}

// vendorPatterns is checked in order; first match wins.
var vendorPatterns = []vendorPattern{
	{"claudex", "Claude"},
	{"claude", "Claude"},
	{"cursor2-claude", "Claude"},
	{"sonnet", "Claude"},
	{"opus", "Claude"},
	{"haiku", "Claude"},
	{"gpt-", "GPT"},
	{"cursor2-gpt", "GPT"},
	{"gemini", "Gemini"},
	{"grok", "Grok"},
	{"deepseek", "DeepSeek"},
	{"glm", "GLM"},
	{"kimi", "Kimi"},
	{"qwen", "Qwen"},
	{"minimax", "MiniMax"},
	{"yi-", "Yi"},
	{"llama", "Meta"},
	{"mistral", "Mistral"},
}

// IdentifyVendor returns the vendor name for a model ID based on keyword matching.
// Returns "Other" if no pattern matches.
func IdentifyVendor(modelID string) string {
	low := strings.ToLower(modelID)
	for _, p := range vendorPatterns {
		if strings.Contains(low, p.keyword) {
			return p.vendor
		}
	}
	return "Other"
}

// skipExact is the set of model IDs to skip by exact match.
var skipExact = map[string]bool{
	"grok-imagine-1.0":       true,
	"grok-imagine-1.0-small": true,
}

// skipKeywords lists substrings that mark a model as non-chat (embeddings, TTS, image gen, etc.).
var skipKeywords = []string{
	"embed", "rerank", "tts", "whisper", "dall-e",
	"stable-diffusion", "midjourney", "suno", "kling", "imagine",
}

// ShouldSkip returns true if the model should be excluded from testing
// (image/audio/embedding models that don't support chat).
func ShouldSkip(modelID string) bool {
	if skipExact[modelID] {
		return true
	}
	low := strings.ToLower(modelID)
	for _, kw := range skipKeywords {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}
